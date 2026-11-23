package service

import (
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/model"
)

// UsageNumber 是一个 usage 数字字段的有效值：要么是字面量，要么是系统 key 引用。
// 动态引用在单次请求的 output prepare 阶段才解析，加载期只校验引用语法与类型。
type UsageNumber struct {
	Literal int64
	Ref     *SystemKey
	// Default 是引用键缺失时使用的默认值；与「子字段未声明回退外层字面值」不同。
	Default *int64
}

// Dynamic 表示该字段需要在 prepare 阶段读取系统 key。
func (n UsageNumber) Dynamic() bool { return n.Ref != nil }

// EffectiveUsage 是补齐回退链后的 usage 四个数字字段。
type EffectiveUsage struct {
	InputTokens       UsageNumber
	OutputTokens      UsageNumber
	CachedInputTokens UsageNumber
	ReasoningTokens   UsageNumber
}

// EffectiveConfig 是一个 output 的有效输出配置。
// 回退链固定为：被选中 output 的 config → 剧本顶层 config → server.config → 内置默认值。
// 只有字段缺失才回退；显式 0/false/空 map 都是有效值（headers 为按层原子覆盖）。
type EffectiveConfig struct {
	// Stream 是 auto / true / false 三种取值之一，运行时结合 request.stream 决定。
	Stream     string
	Status     int
	Headers    map[string]string
	Flush      bool
	LineEnding string
	Usage      EffectiveUsage
	UsageMode  string
}

// Streaming 判断本次输出是否流式：auto 跟随 request.stream，true/false 强制。
func (c *EffectiveConfig) Streaming(requestStream bool) bool {
	switch c.Stream {
	case constants.StreamTrue:
		return true
	case constants.StreamFalse:
		return false
	default:
		return requestStream
	}
}

// ResolveOutputConfig 按回退链计算有效输出配置。返回的 *model.FieldError 路径相对
// output.config（例如 config.usage.input_tokens），调用方据此定位原始 YAML 节点。
func ResolveOutputConfig(out *model.OutputConfig, script *model.ScriptConfig, defaults model.ScriptDefaults) (*EffectiveConfig, error) {
	effective := &EffectiveConfig{
		Stream:     defaults.Stream,
		Status:     defaults.Status,
		Headers:    cloneHeaders(defaults.Headers),
		Flush:      defaults.Flush,
		LineEnding: defaults.LineEnding,
		UsageMode:  defaults.UsageMode,
		Usage: EffectiveUsage{
			InputTokens:       UsageNumber{Literal: defaults.Usage.InputTokens},
			OutputTokens:      UsageNumber{Literal: defaults.Usage.OutputTokens},
			CachedInputTokens: UsageNumber{Literal: defaults.Usage.CachedInputTokens},
			ReasoningTokens:   UsageNumber{Literal: defaults.Usage.ReasoningTokens},
		},
	}

	// 剧本顶层 config：显式值优先于 server 默认值。
	if script != nil {
		if script.Stream != nil {
			effective.Stream = script.Stream.Value
		}
		if script.Status != nil {
			effective.Status = *script.Status
		}
		if script.Headers != nil {
			effective.Headers = cloneHeaders(script.Headers)
		}
		if script.Flush != nil {
			effective.Flush = *script.Flush
		}
		if script.LineEnding != nil {
			effective.LineEnding = script.LineEnding.Value
		}
		if script.UsageMode != nil {
			effective.UsageMode = script.UsageMode.Value
		}
		applyStaticUsage(&effective.Usage, script.Usage)
	}

	// output.config：最内层覆盖，headers 整体替换而不是逐 key 合并。
	if out != nil {
		if out.Stream != nil {
			effective.Stream = out.Stream.Value
		}
		if out.Status != nil {
			effective.Status = *out.Status
		}
		if out.Headers != nil {
			effective.Headers = cloneHeaders(out.Headers)
		}
		if out.Flush != nil {
			effective.Flush = *out.Flush
		}
		if out.LineEnding != nil {
			effective.LineEnding = out.LineEnding.Value
		}
		if out.UsageMode != nil {
			effective.UsageMode = out.UsageMode.Value
		}
		if out.Usage != nil {
			if err := applyDynamicUsage(&effective.Usage, out.Usage); err != nil {
				return nil, err
			}
		}
	}
	return effective, nil
}

// applyStaticUsage 应用剧本顶层 config.usage（必须是静态非负整数）。
func applyStaticUsage(usage *EffectiveUsage, cfg *model.Usage) {
	if cfg == nil {
		return
	}
	if cfg.InputTokens != nil {
		usage.InputTokens = UsageNumber{Literal: *cfg.InputTokens}
	}
	if cfg.OutputTokens != nil {
		usage.OutputTokens = UsageNumber{Literal: *cfg.OutputTokens}
	}
	if cfg.CachedInputTokens != nil {
		usage.CachedInputTokens = UsageNumber{Literal: *cfg.CachedInputTokens}
	}
	if cfg.ReasoningTokens != nil {
		usage.ReasoningTokens = UsageNumber{Literal: *cfg.ReasoningTokens}
	}
}

// applyDynamicUsage 应用 output.config.usage：四个数字字段都允许系统 key 引用。
func applyDynamicUsage(usage *EffectiveUsage, cfg *model.UsageOverride) error {
	fields := []struct {
		name string
		raw  *model.Value
		slot func() *UsageNumber
	}{
		{"input_tokens", cfg.InputTokens, func() *UsageNumber { return &usage.InputTokens }},
		{"output_tokens", cfg.OutputTokens, func() *UsageNumber { return &usage.OutputTokens }},
		{"cached_input_tokens", cfg.CachedInputTokens, func() *UsageNumber { return &usage.CachedInputTokens }},
		{"reasoning_tokens", cfg.ReasoningTokens, func() *UsageNumber { return &usage.ReasoningTokens }},
	}
	for _, field := range fields {
		if field.raw == nil {
			continue
		}
		number, err := compileUsageNumber(field.raw, "config.usage."+field.name)
		if err != nil {
			return err
		}
		*field.slot() = number
	}
	return nil
}

// compileUsageNumber 解析一个 usage 数字字段：非负整数，或系统 key 引用。
func compileUsageNumber(raw *model.Value, path string) (UsageNumber, error) {
	if raw == nil || raw.Node == nil {
		return UsageNumber{}, &model.FieldError{Path: fieldPath(path), Msg: "value is required"}
	}
	switch raw.Node.Kind {
	case yaml.ScalarNode:
		if raw.Node.Tag != "!!int" {
			return UsageNumber{}, &model.FieldError{
				Path: fieldPath(path),
				Msg:  fmt.Sprintf("must be a non-negative integer or a system key reference, got %s", raw.Node.Tag),
			}
		}
		value, err := strconv.ParseInt(raw.Node.Value, 10, 64)
		if err != nil || value < 0 {
			return UsageNumber{}, &model.FieldError{
				Path: fieldPath(path),
				Msg:  fmt.Sprintf("must be a non-negative integer, got %s", raw.Node.Value),
			}
		}
		return UsageNumber{Literal: value}, nil
	case yaml.MappingNode:
		var ref model.SystemKeyRef
		if err := ref.UnmarshalYAML(raw.Node); err != nil {
			return UsageNumber{}, &model.FieldError{Path: fieldPath(path), Msg: err.Error()}
		}
		compiled, err := CompileSystemKey(path, ref.Key, ref.Field, ref.Mode, ref.Path, true)
		if err != nil {
			return UsageNumber{}, &model.FieldError{Path: fieldPath(path), Msg: err.Error()}
		}
		number := UsageNumber{Ref: compiled}
		if ref.Default != nil {
			node := ref.Default.Node
			if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
				return UsageNumber{}, &model.FieldError{
					Path: fieldPath(path),
					Msg:  "default must be a non-negative integer",
				}
			}
			value, err := strconv.ParseInt(node.Value, 10, 64)
			if err != nil || value < 0 {
				return UsageNumber{}, &model.FieldError{
					Path: fieldPath(path),
					Msg:  fmt.Sprintf("default must be a non-negative integer, got %s", node.Value),
				}
			}
			number.Default = &value
		}
		return number, nil
	}
	return UsageNumber{}, &model.FieldError{Path: fieldPath(path), Msg: "must be an integer or a system key reference"}
}

// IsBodylessStatus 判断 HTTP 状态是否禁止响应 body（204、205、304）。
func IsBodylessStatus(status int) bool {
	return status == 204 || status == 205 || status == 304
}

func cloneHeaders(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for name, value := range in {
		out[name] = value
	}
	return out
}

// fieldPath 把 "a.b.c" 形式转成 FieldError 的路径切片。
func fieldPath(path string) []any {
	parts := make([]any, 0, 4)
	start := 0
	for i := 0; i <= len(path); i++ {
		if i == len(path) || path[i] == '.' {
			if i > start {
				parts = append(parts, path[start:i])
			}
			start = i + 1
		}
	}
	return parts
}
