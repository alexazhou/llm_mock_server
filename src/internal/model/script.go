package model

import (
	"errors"
	"fmt"
	"strings"
)

// Script 是一个剧本文件的完整结构。顶层只允许 version、id、config、assert、rounds。
type Script struct {
	Version int           `yaml:"version"`
	ID      string        `yaml:"id"`
	Config  *ScriptConfig `yaml:"config"`
	Assert  AssertionList `yaml:"assert"`
	Rounds  []Round       `yaml:"rounds"`
}

// Round 是一轮模型输出。output 与 outputs 互斥，input 只做输入校验。
type Round struct {
	Input   *RoundInput   `yaml:"input"`
	Output  *RoundOutput  `yaml:"output"`
	Outputs []RoundOutput `yaml:"outputs"`
}

// RoundInput 只允许 assert，不参与 round 或 output 选择。
type RoundInput struct {
	Assert AssertionList `yaml:"assert"`
}

// RoundOutput 是一个协议无关的输出声明。when 只出现在 outputs 候选项中。
type RoundOutput struct {
	When      ConditionList  `yaml:"when"`
	Config    *OutputConfig  `yaml:"config"`
	Content   *TextValue     `yaml:"content"`
	Reasoning *TextValue     `yaml:"reasoning"`
	ToolCalls []ToolCall     `yaml:"tool_calls"`
	Error     *ProtocolError `yaml:"error"`
	Steps     Steps          `yaml:"steps"`
}

// ToolCall 是一个工具调用声明；arguments 必须是可序列化为 JSON 的值。
type ToolCall struct {
	ID        string `yaml:"id"`
	Name      string `yaml:"name"`
	Arguments Value  `yaml:"arguments"`
}

// ProtocolError 是协议错误声明，省略 steps 时由框架直接发送。
type ProtocolError struct {
	Message *TextValue `yaml:"message"`
	Type    string     `yaml:"type"`
	Code    string     `yaml:"code"`
	Param   *TextValue `yaml:"param"`
}

// AssertionList 是 assert 列表；ConditionList 与 when 列表语法一致但语义不同。
type AssertionList []Assertion
type ConditionList []Assertion

// Assertion 是一条输入断言或一个 output 匹配条件。
type Assertion struct {
	Name     string `yaml:"name"`
	Key      string `yaml:"key"`
	Field    string `yaml:"field"`
	Mode     string `yaml:"mode"`
	Path     string `yaml:"path"`
	Op       string `yaml:"op"`
	Expected *Value `yaml:"expected"`
	Default  *Value `yaml:"default"`
}

// ScriptConfig 是剧本顶层 config 与服务级默认值。所有字段都是静态值，
// 用指针区分「省略」与「显式零值」。
type ScriptConfig struct {
	MaxBodyBytes    *int64            `yaml:"max_body_bytes"`
	ReadBodyTimeout *Duration         `yaml:"read_body_timeout"`
	Stream          *StreamMode       `yaml:"stream"`
	Status          *int              `yaml:"status"`
	Headers         map[string]string `yaml:"headers"`
	Flush           *bool             `yaml:"flush"`
	LineEnding      *LineEnding       `yaml:"line_ending"`
	Usage           *Usage            `yaml:"usage"`
	UsageMode       *UsageMode        `yaml:"usage_mode"`
}

// Usage 是协议无关的 usage 声明，只允许非负整数。
type Usage struct {
	InputTokens       *int64 `yaml:"input_tokens"`
	OutputTokens      *int64 `yaml:"output_tokens"`
	CachedInputTokens *int64 `yaml:"cached_input_tokens"`
	ReasoningTokens   *int64 `yaml:"reasoning_tokens"`
}

// OutputConfig 是单个 output 的响应设置覆盖。max_body_bytes 与
// read_body_timeout 只能由剧本顶层 config 或 server.config 设置，因此不在此结构中出现。
type OutputConfig struct {
	Stream     *StreamMode       `yaml:"stream"`
	Status     *int              `yaml:"status"`
	Headers    map[string]string `yaml:"headers"`
	Flush      *bool             `yaml:"flush"`
	LineEnding *LineEnding       `yaml:"line_ending"`
	Usage      *UsageOverride    `yaml:"usage"`
	UsageMode  *UsageMode        `yaml:"usage_mode"`
}

// UsageOverride 是 output.config.usage：四个数字字段都允许系统 key 引用。
type UsageOverride struct {
	InputTokens       *Value `yaml:"input_tokens"`
	OutputTokens      *Value `yaml:"output_tokens"`
	CachedInputTokens *Value `yaml:"cached_input_tokens"`
	ReasoningTokens   *Value `yaml:"reasoning_tokens"`
}

// Steps 是显式传输过程；契约见 Output Steps 专项设计。
type Steps []Step

// Step 是一个 send / wait / end 操作。
type Step struct {
	Op       string        `yaml:"op"`
	Source   string        `yaml:"source"`
	Target   string        `yaml:"target"`
	Interval *Duration     `yaml:"interval"`
	Chunks   *Chunks       `yaml:"chunks"`
	Duration *WaitDuration `yaml:"duration"`
	Mode     string        `yaml:"mode"`
	Text     *string       `yaml:"text"`
	Count    *int          `yaml:"count"`
	Base64   *string       `yaml:"base64"`
	Fixture  *string       `yaml:"fixture"`
}

// Chunks 是 send 的字节分片设置，size 与 sizes 只能二选一。
type Chunks struct {
	Size  *int  `yaml:"size"`
	Sizes []int `yaml:"sizes"`
	Flush *bool `yaml:"flush"`
}

// FieldError 是一个带字段位置的配置校验错误。Path 是相对校验对象根的路径
// （string 表示 mapping key，int 表示 sequence 下标），调用方据此在原始
// YAML 节点上定位行列号，保证错误里既有字段路径也有节点位置。
type FieldError struct {
	Path []any
	Msg  string
}

func (e *FieldError) Error() string { return e.Msg }

func fieldErr(msg string, path ...any) *FieldError {
	return &FieldError{Path: path, Msg: msg}
}

// ValidateScriptConfig 校验剧本顶层 config / server.config 的静态字段。
// 返回的 FieldError 路径相对 config 对象。
func ValidateScriptConfig(cfg *ScriptConfig) error {
	if cfg == nil {
		return nil
	}
	if cfg.MaxBodyBytes != nil && *cfg.MaxBodyBytes <= 0 {
		return fieldErr("max_body_bytes must be a positive integer", "max_body_bytes")
	}
	if cfg.Status != nil && (*cfg.Status < 200 || *cfg.Status > 599) {
		return fieldErr(fmt.Sprintf("status must be an integer between 200 and 599, got %d", *cfg.Status), "status")
	}
	if err := ValidateHeaders(cfg.Headers); err != nil {
		return err
	}
	if err := validateUsage(cfg.Usage); err != nil {
		return err
	}
	return nil
}

// ValidateOutputConfig 校验 output.config 的静态字段。
func ValidateOutputConfig(cfg *OutputConfig) error {
	if cfg == nil {
		return nil
	}
	if cfg.Status != nil && (*cfg.Status < 200 || *cfg.Status > 599) {
		return fieldErr(fmt.Sprintf("status must be an integer between 200 and 599, got %d", *cfg.Status), "status")
	}
	return ValidateHeaders(cfg.Headers)
}

// ValidateHeaders 校验响应头名称与值：名称必须符合 Go net/http 规则，
// 值中禁止 CR/LF，框架保留 header 与规范化后重复的 header 直接拒绝。
func ValidateHeaders(headers map[string]string) error {
	seen := make(map[string]string, len(headers))
	for name, value := range headers {
		if !isValidHeaderName(name) {
			return fieldErr(fmt.Sprintf("invalid header name %q", name), "headers", name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fieldErr(fmt.Sprintf("header %q value must not contain CR or LF", name), "headers", name)
		}
		canonical := canonicalHeaderName(name)
		if prev, ok := seen[canonical]; ok {
			return fieldErr(fmt.Sprintf("header %q duplicates header %q (header names are case-insensitive)", name, prev), "headers", name)
		}
		seen[canonical] = name
		if isReservedHeader(canonical) {
			return fieldErr(fmt.Sprintf("header %q is reserved by the mock framework and cannot be configured", name), "headers", name)
		}
	}
	return nil
}

func validateUsage(u *Usage) error {
	if u == nil {
		return nil
	}
	fields := []struct {
		name  string
		value *int64
	}{
		{"input_tokens", u.InputTokens},
		{"output_tokens", u.OutputTokens},
		{"cached_input_tokens", u.CachedInputTokens},
		{"reasoning_tokens", u.ReasoningTokens},
	}
	for _, field := range fields {
		if field.value != nil && *field.value < 0 {
			return fieldErr(fmt.Sprintf("usage.%s must be a non-negative integer, got %d", field.name, *field.value),
				"usage", field.name)
		}
	}
	return nil
}

// isValidHeaderName 按 RFC 7230 token 规则校验 header 名称。
func isValidHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)):
		default:
			return false
		}
	}
	return true
}

// canonicalHeaderName 与 net/http 的规范化一致：每个 '-' 后的首字母大写。
func canonicalHeaderName(name string) string {
	out := make([]byte, len(name))
	upper := true
	for i := 0; i < len(name); i++ {
		c := name[i]
		if upper && c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out[i] = c
		upper = c == '-'
	}
	return string(out)
}

// reservedHeaders 是框架保留 header，配置中出现时直接拒绝。
var reservedHeaders = map[string]bool{
	"Content-Length":    true,
	"Transfer-Encoding": true,
	"Connection":        true,
	"Trailer":           true,
	"Upgrade":           true,
}

func isReservedHeader(canonical string) bool {
	if reservedHeaders[canonical] {
		return true
	}
	return strings.HasPrefix(canonical, "Access-Control-Allow-")
}

// prefixFieldError 给 FieldError 的路径加上前缀；非 FieldError 原样返回。
func prefixFieldError(err error, prefix ...any) error {
	var fe *FieldError
	if errors.As(err, &fe) {
		path := append(append([]any{}, prefix...), fe.Path...)
		return &FieldError{Path: path, Msg: fe.Msg}
	}
	return err
}
