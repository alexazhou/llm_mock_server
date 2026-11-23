package model

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/alexazhou/llm_mock_server/internal/constants"
)

// ServerConfig 是 server.yaml 的结构：进程级配置与所有剧本的服务级默认值。
type ServerConfig struct {
	Version    int              `yaml:"version"`
	Listen     string           `yaml:"listen"`
	ScriptsDir string           `yaml:"scripts_dir"`
	Config     *ScriptConfig    `yaml:"config"`
	Transport  *TransportConfig `yaml:"transport"`
	Runtime    *RuntimeConfig   `yaml:"runtime"`
	Capture    *CaptureConfig   `yaml:"capture"`
}

// TransportConfig 是 HTTP 传输层配置。
type TransportConfig struct {
	ReadHeaderTimeout *Duration `yaml:"read_header_timeout"`
}

// RuntimeConfig 是运行期资源上限。
type RuntimeConfig struct {
	MaxActiveRequests  *int64    `yaml:"max_active_requests"`
	MaxOutputBytes     *int64    `yaml:"max_output_bytes"`
	MaxFixtureBytes    *int64    `yaml:"max_fixture_bytes"`
	MaxGeneratedEvents *int64    `yaml:"max_generated_events"`
	MaxWriteUnits      *int64    `yaml:"max_write_units"`
	ShutdownTimeout    *Duration `yaml:"shutdown_timeout"`
}

// CaptureConfig 是 capture 窗口配置。
type CaptureConfig struct {
	Capacity      *int64 `yaml:"capacity"`
	MaxBodyBytes  *int64 `yaml:"max_body_bytes"`
	MaxTotalBytes *int64 `yaml:"max_total_bytes"`
}

// ResolvedServer 是补齐内置默认值后的服务配置，供 serve 直接使用。
type ResolvedServer struct {
	Listen               string
	ListenHost           string
	ListenPort           int
	ScriptsDir           string
	ReadHeaderTimeout    time.Duration
	ShutdownTimeout      time.Duration
	MaxActiveRequests    int64
	MaxOutputBytes       int64
	MaxFixtureBytes      int64
	MaxGeneratedEvents   int64
	MaxWriteUnits        int64
	CaptureCapacity      int64
	CaptureMaxBodyBytes  int64
	CaptureMaxTotalBytes int64
	// ScriptConfig 是剧本 config 的服务级默认值（已补内置默认值）。
	ScriptConfig ScriptDefaults
}

// ScriptDefaults 是补全后的静态 config 默认值。
type ScriptDefaults struct {
	MaxBodyBytes    int64
	ReadBodyTimeout time.Duration
	Stream          string
	Status          int
	Headers         map[string]string
	Flush           bool
	LineEnding      string
	Usage           UsageValues
	UsageMode       string
}

// UsageValues 是补全后的 usage 数值。
type UsageValues struct {
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64
	ReasoningTokens   int64
}

// Resolve 校验 server.yaml 并补齐内置默认值。
func (s *ServerConfig) Resolve() (*ResolvedServer, error) {
	if s == nil {
		return nil, fmt.Errorf("server config is missing")
	}
	if s.Version != constants.SchemaVersion {
		return nil, fieldErr(fmt.Sprintf("version must be the integer %d, got %d", constants.SchemaVersion, s.Version), "version")
	}

	listen := s.Listen
	if listen == "" {
		listen = constants.DefaultListen
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fieldErr(fmt.Sprintf("listen %q must be a host:port address", listen), "listen")
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return nil, fieldErr(fmt.Sprintf("listen %q must use a port between 1 and 65535", listen), "listen")
	}

	scriptsDir := s.ScriptsDir
	if scriptsDir == "" {
		scriptsDir = constants.DefaultScriptsDir
	}

	resolved := &ResolvedServer{
		Listen:               listen,
		ListenHost:           host,
		ListenPort:           portNum,
		ScriptsDir:           scriptsDir,
		ReadHeaderTimeout:    constants.DefaultReadHeaderTimeout,
		ShutdownTimeout:      constants.DefaultShutdownTimeout,
		MaxActiveRequests:    constants.DefaultMaxActiveRequests,
		MaxOutputBytes:       constants.DefaultMaxOutputBytes,
		MaxFixtureBytes:      constants.DefaultMaxFixtureBytes,
		MaxGeneratedEvents:   constants.DefaultMaxGeneratedEvents,
		MaxWriteUnits:        constants.DefaultMaxWriteUnits,
		CaptureCapacity:      constants.DefaultCaptureCapacity,
		CaptureMaxBodyBytes:  constants.DefaultCaptureMaxBodyBytes,
		CaptureMaxTotalBytes: constants.DefaultCaptureMaxTotalBytes,
	}

	if t := s.Transport; t != nil && t.ReadHeaderTimeout != nil {
		resolved.ReadHeaderTimeout = t.ReadHeaderTimeout.D
	}
	if r := s.Runtime; r != nil {
		if r.ShutdownTimeout != nil {
			resolved.ShutdownTimeout = r.ShutdownTimeout.D
		}
		positive := []struct {
			name  string
			value *int64
			dst   *int64
		}{
			{"runtime.max_active_requests", r.MaxActiveRequests, &resolved.MaxActiveRequests},
			{"runtime.max_output_bytes", r.MaxOutputBytes, &resolved.MaxOutputBytes},
			{"runtime.max_fixture_bytes", r.MaxFixtureBytes, &resolved.MaxFixtureBytes},
			{"runtime.max_generated_events", r.MaxGeneratedEvents, &resolved.MaxGeneratedEvents},
			{"runtime.max_write_units", r.MaxWriteUnits, &resolved.MaxWriteUnits},
		}
		for _, field := range positive {
			if field.value == nil {
				continue
			}
			if *field.value <= 0 {
				return nil, fieldErr(fmt.Sprintf("%s must be a positive integer, got %d", field.name, *field.value),
					"runtime", strings.TrimPrefix(field.name, "runtime."))
			}
			*field.dst = *field.value
		}
	}
	if c := s.Capture; c != nil {
		if c.Capacity != nil {
			if *c.Capacity <= 0 {
				return nil, fieldErr(fmt.Sprintf("capture.capacity must be a positive integer, got %d", *c.Capacity), "capture", "capacity")
			}
			resolved.CaptureCapacity = *c.Capacity
		}
		if c.MaxBodyBytes != nil {
			if *c.MaxBodyBytes < 0 {
				return nil, fieldErr(fmt.Sprintf("capture.max_body_bytes must be a non-negative integer, got %d", *c.MaxBodyBytes), "capture", "max_body_bytes")
			}
			resolved.CaptureMaxBodyBytes = *c.MaxBodyBytes
		}
		if c.MaxTotalBytes != nil {
			if *c.MaxTotalBytes <= 0 {
				return nil, fieldErr(fmt.Sprintf("capture.max_total_bytes must be a positive integer, got %d", *c.MaxTotalBytes), "capture", "max_total_bytes")
			}
			resolved.CaptureMaxTotalBytes = *c.MaxTotalBytes
		}
	}

	if err := ValidateScriptConfig(s.Config); err != nil {
		return nil, prefixFieldError(err, "config")
	}
	resolved.ScriptConfig = resolveScriptDefaults(s.Config)
	return resolved, nil
}

// resolveScriptDefaults 按 server.config → 内置默认值 的顺序补全剧本 config。
func resolveScriptDefaults(cfg *ScriptConfig) ScriptDefaults {
	defaults := ScriptDefaults{
		MaxBodyBytes:    constants.DefaultMaxBodyBytes,
		ReadBodyTimeout: constants.DefaultReadBodyTimeout,
		Stream:          constants.DefaultStream,
		Status:          constants.DefaultStatus,
		Headers:         map[string]string{},
		Flush:           true,
		LineEnding:      constants.DefaultLineEnding,
		UsageMode:       constants.DefaultUsageMode,
	}
	if cfg == nil {
		return defaults
	}
	if cfg.MaxBodyBytes != nil {
		defaults.MaxBodyBytes = *cfg.MaxBodyBytes
	}
	if cfg.ReadBodyTimeout != nil {
		defaults.ReadBodyTimeout = cfg.ReadBodyTimeout.D
	}
	if cfg.Stream != nil {
		defaults.Stream = cfg.Stream.Value
	}
	if cfg.Status != nil {
		defaults.Status = *cfg.Status
	}
	if cfg.Headers != nil {
		defaults.Headers = cfg.Headers
	}
	if cfg.Flush != nil {
		defaults.Flush = *cfg.Flush
	}
	if cfg.LineEnding != nil {
		defaults.LineEnding = cfg.LineEnding.Value
	}
	if cfg.UsageMode != nil {
		defaults.UsageMode = cfg.UsageMode.Value
	}
	if u := cfg.Usage; u != nil {
		if u.InputTokens != nil {
			defaults.Usage.InputTokens = *u.InputTokens
		}
		if u.OutputTokens != nil {
			defaults.Usage.OutputTokens = *u.OutputTokens
		}
		if u.CachedInputTokens != nil {
			defaults.Usage.CachedInputTokens = *u.CachedInputTokens
		}
		if u.ReasoningTokens != nil {
			defaults.Usage.ReasoningTokens = *u.ReasoningTokens
		}
	}
	return defaults
}
