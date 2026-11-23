package model

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/alexazhou/llm_mock_server/internal/constants"
)

// Value 保留 YAML 原始节点，避免数字在解码阶段被转换成 float64 而丢失
// 原始十进制表示。比较与序列化由后续阶段（断言、动态输出）在节点上完成。
type Value struct {
	Node *yaml.Node
}

// UnmarshalYAML 只保存原始节点，不做任何类型转换。
func (v *Value) UnmarshalYAML(n *yaml.Node) error {
	v.Node = n
	return nil
}

// TextValue 表示一个字符串字面量或系统 key 引用（content、reasoning、
// error.message、error.param 使用同一形态）。
type TextValue struct {
	Literal string
	Ref     *SystemKeyRef
}

// UnmarshalYAML 接受非空字符串或 {key, path, default} 形态的系统 key 引用。
func (t *TextValue) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag != "!!str" {
			return fmt.Errorf("must be a string or a system key reference, got %s", n.Tag)
		}
		if n.Value == "" {
			return fmt.Errorf("must not be an empty string")
		}
		t.Literal = n.Value
		return nil
	case yaml.MappingNode:
		var ref SystemKeyRef
		if err := ref.UnmarshalYAML(n); err != nil {
			return err
		}
		t.Ref = &ref
		return nil
	}
	return fmt.Errorf("must be a string or a system key reference")
}

// SystemKeyRef 是只读系统 key 引用，形如 {key: request.json_body, path: $.cached_tokens, default: 0}。
type SystemKeyRef struct {
	Key     string
	Field   string
	Mode    string
	Path    string
	Default *Value
}

// UnmarshalYAML 手工解析引用结构，保证未知字段与无效类型都被拒绝。
func (r *SystemKeyRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("system key reference must be a mapping with a key field")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		keyNode, valNode := n.Content[i], n.Content[i+1]
		switch keyNode.Value {
		case "key":
			if valNode.Kind != yaml.ScalarNode || valNode.Tag != "!!str" || valNode.Value == "" {
				return fmt.Errorf("system key reference field %q must be a non-empty string", "key")
			}
			r.Key = valNode.Value
		case "field", "mode", "path":
			if valNode.Kind != yaml.ScalarNode || valNode.Tag != "!!str" {
				return fmt.Errorf("system key reference field %q must be a string", keyNode.Value)
			}
			switch keyNode.Value {
			case "field":
				r.Field = valNode.Value
			case "mode":
				r.Mode = valNode.Value
			case "path":
				r.Path = valNode.Value
			}
		case "default":
			r.Default = &Value{Node: valNode}
		default:
			return fmt.Errorf("unknown field %q in system key reference", keyNode.Value)
		}
	}
	if r.Key == "" {
		return fmt.Errorf("system key reference requires a non-empty field \"key\"")
	}
	return nil
}

// Duration 是 Go duration 字符串（如 15s、100ms），必须非负。
type Duration struct {
	D time.Duration
}

// UnmarshalYAML 只接受非负的 Go duration 字符串。
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return fmt.Errorf("duration must be a string such as 15s or 100ms")
	}
	parsed, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %v", n.Value, err)
	}
	if parsed < 0 {
		return fmt.Errorf("duration %q must not be negative", n.Value)
	}
	d.D = parsed
	return nil
}

// WaitDuration 是 wait.duration：普通非负 duration，或小写特殊值 infinite。
type WaitDuration struct {
	D        time.Duration
	Infinite bool
}

// UnmarshalYAML 接受 unlimited 等待关键字或普通 duration。
func (d *WaitDuration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return fmt.Errorf("duration must be a duration string or the word infinite")
	}
	if n.Value == "infinite" {
		d.Infinite = true
		return nil
	}
	parsed, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %v", n.Value, err)
	}
	if parsed < 0 {
		return fmt.Errorf("duration %q must not be negative", n.Value)
	}
	d.D = parsed
	return nil
}

// StreamMode 是 config.stream：auto 或布尔 true/false。
type StreamMode struct {
	Value string
}

// UnmarshalYAML 接受 "auto" 字符串与布尔值。
func (s *StreamMode) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("stream must be auto, true or false")
	}
	switch n.Tag {
	case "!!str":
		if n.Value != constants.StreamAuto {
			return fmt.Errorf("stream must be auto, true or false, got %q", n.Value)
		}
		s.Value = constants.StreamAuto
		return nil
	case "!!bool":
		if n.Value == "true" {
			s.Value = constants.StreamTrue
		} else {
			s.Value = constants.StreamFalse
		}
		return nil
	}
	return fmt.Errorf("stream must be auto, true or false")
}

// LineEnding 是 config.line_ending：lf 或 crlf。
type LineEnding struct {
	Value string
}

// UnmarshalYAML 接受 lf / crlf。
func (l *LineEnding) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return fmt.Errorf("line_ending must be lf or crlf")
	}
	switch n.Value {
	case constants.LineEndingLF, constants.LineEndingCRLF:
		l.Value = n.Value
		return nil
	}
	return fmt.Errorf("line_ending must be lf or crlf, got %q", n.Value)
}

// UsageMode 是 config.usage_mode：auto / emit / omit。
type UsageMode struct {
	Value string
}

// UnmarshalYAML 接受 auto / emit / omit。
func (u *UsageMode) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return fmt.Errorf("usage_mode must be auto, emit or omit")
	}
	switch n.Value {
	case constants.UsageModeAuto, constants.UsageModeEmit, constants.UsageModeOmit:
		u.Value = n.Value
		return nil
	}
	return fmt.Errorf("usage_mode must be auto, emit or omit, got %q", n.Value)
}
