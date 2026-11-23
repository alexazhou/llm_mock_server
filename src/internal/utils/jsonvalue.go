package utils

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Number 是保留原始十进制文本的 JSON/YAML 数字。请求 JSON 与 YAML 字面量都
// 不经过 float64，避免 1 与 1.0、大整数精度等比较误差。
type Number string

// MarshalJSON 输出原始数字文本，而不是带引号的字符串。
func (n Number) MarshalJSON() ([]byte, error) {
	var parsed json.Number
	if err := json.Unmarshal([]byte(n), &parsed); err == nil && string(parsed) == string(n) {
		return []byte(n), nil
	}
	return nil, fmt.Errorf("invalid number literal %q", string(n))
}

// NormalizeJSON 把 encoding/json（UseNumber）解码结果规范化为比较用值：
// json.Number 统一为 Number，map[string]any / []any 递归处理。
func NormalizeJSON(v any) any {
	switch typed := v.(type) {
	case json.Number:
		return Number(typed.String())
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, item := range typed {
			out[k] = NormalizeJSON(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = NormalizeJSON(item)
		}
		return out
	default:
		return v
	}
}

// FromYAMLNode 把 YAML 节点转成与 JSON 值同构的比较值。
func FromYAMLNode(n *yaml.Node) (any, error) {
	if n == nil {
		return nil, nil
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return FromYAMLNode(n.Content[0])
	case yaml.AliasNode:
		return nil, fmt.Errorf("YAML alias is not supported")
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!null":
			return nil, nil
		case "!!bool":
			return n.Value == "true", nil
		case "!!int", "!!float":
			if _, ok := new(big.Rat).SetString(n.Value); !ok {
				return nil, fmt.Errorf("unsupported number literal %q", n.Value)
			}
			return Number(n.Value), nil
		case "!!str":
			return n.Value, nil
		}
		return n.Value, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, item := range n.Content {
			v, err := FromYAMLNode(item)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("non-scalar object keys are not supported")
			}
			v, err := FromYAMLNode(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			out[n.Content[i].Value] = v
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported YAML node kind")
}

// TypeName 返回配置契约使用的类型名：null / boolean / number / string / array / object。
func TypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case Number, json.Number, int, int64, float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// Equal 实现 eq/ne 的相等语义：array 保留顺序、object 忽略 key 顺序、
// number 精确按十进制数值比较（1 == 1.0），不同 JSON 类型始终不等。
func Equal(a, b any) bool {
	ta, tb := TypeName(a), TypeName(b)
	if ta != tb {
		return false
	}
	switch ta {
	case "null":
		return true
	case "boolean":
		return a.(bool) == b.(bool)
	case "string":
		return a.(string) == b.(string)
	case "number":
		return compareNumbers(a, b) == 0
	case "array":
		left, right := a.([]any), b.([]any)
		if len(left) != len(right) {
			return false
		}
		for i := range left {
			if !Equal(left[i], right[i]) {
				return false
			}
		}
		return true
	case "object":
		left, right := a.(map[string]any), b.(map[string]any)
		if len(left) != len(right) {
			return false
		}
		for k, lv := range left {
			rv, ok := right[k]
			if !ok || !Equal(lv, rv) {
				return false
			}
		}
		return true
	}
	return false
}

// Contains 实现 contains：string 看子串，array 看元素（复用 Equal 语义）。
func Contains(haystack, needle any) (bool, error) {
	switch typed := haystack.(type) {
	case string:
		str, ok := needle.(string)
		if !ok {
			return false, fmt.Errorf("contains on a string requires a string needle, got %s", TypeName(needle))
		}
		return strings.Contains(typed, str), nil
	case []any:
		for _, item := range typed {
			if Equal(item, needle) {
				return true, nil
			}
		}
		return false, nil
	}
	return false, fmt.Errorf("contains requires a string or array actual value, got %s", TypeName(haystack))
}

// LengthOf 返回长度：string 按 Unicode code point、array 按元素、object 按 key。
func LengthOf(v any) (int, bool) {
	switch typed := v.(type) {
	case string:
		return utf8.RuneCountInString(typed), true
	case []any:
		return len(typed), true
	case map[string]any:
		return len(typed), true
	}
	return 0, false
}

// CompareNumbers 比较两个数字值（-1 / 0 / 1）。
func CompareNumbers(a, b any) (int, error) {
	if TypeName(a) != "number" || TypeName(b) != "number" {
		return 0, fmt.Errorf("numeric comparison requires numbers, got %s and %s", TypeName(a), TypeName(b))
	}
	return compareNumbers(a, b), nil
}

func compareNumbers(a, b any) int {
	ra, okA := new(big.Rat).SetString(numberText(a))
	rb, okB := new(big.Rat).SetString(numberText(b))
	if !okA || !okB {
		return strings.Compare(numberText(a), numberText(b))
	}
	return ra.Cmp(rb)
}

func numberText(v any) string {
	switch typed := v.(type) {
	case Number:
		return string(typed)
	case json.Number:
		return typed.String()
	case int:
		return fmt.Sprintf("%d", typed)
	case int64:
		return fmt.Sprintf("%d", typed)
	}
	return fmt.Sprint(v)
}

// CompactJSON 返回紧凑 JSON 序列化结果。
func CompactJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

// maxPreviewBytes 是任意诊断值的字节上限（配置契约 §9）。
const maxPreviewBytes = 4096

// Preview 把任意诊断值转成可直接放进错误响应的形式：紧凑 JSON 序列化后
// 在 Unicode code point 边界截断到 ≤4096 UTF-8 字节，被截断时返回
// {"preview": "...", "truncated": true}。
func Preview(v any) any {
	raw, err := CompactJSON(v)
	if err != nil {
		return map[string]any{"preview": fmt.Sprintf("%v", v), "truncated": false}
	}
	if len(raw) <= maxPreviewBytes {
		return withRawJSON(raw)
	}
	truncated := truncateUTF8(raw, maxPreviewBytes)
	return map[string]any{"preview": string(truncated), "truncated": true}
}

// rawJSON 保留已序列化内容，避免 Preview 结果被二次转义。
type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) { return r, nil }

func withRawJSON(raw []byte) any {
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return rawJSON(raw)
	}
	return out
}

// truncateUTF8 在 code point 边界截断到不超过 limit 字节。
func truncateUTF8(b []byte, limit int) []byte {
	if len(b) <= limit {
		return b
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return b[:cut]
}

// SortedKeys 返回 object key 的字典序副本，用于稳定诊断。
func SortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
