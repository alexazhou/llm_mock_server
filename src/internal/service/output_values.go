package service

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/alexazhou/llm_mock_server/internal/model"
	"github.com/alexazhou/llm_mock_server/internal/utils"
)

// 本文件是输出层「值」的运行时处理：动态字段解析（content、reasoning、
// tool_calls[].arguments 叶子、error.message/param、usage 四数字）、
// 确定性 JSON 序列化与省略 tool call ID 时的生成规则。
// 协议容器与 SSE 事件序列在 adapter_*.go 与 steps 引擎里，不在这里。

// CompiledText 是 content / reasoning / error.message / error.param 的编译结果：
// 静态字面量或系统 key 引用（引用可带 default）。
type CompiledText struct {
	Literal    string
	HasLiteral bool
	Key        *SystemKey
	Default    any
	HasDefault bool
}

// CompileTextValue 编译一个文本字段；allowCount 允许 request.count 引用。
func CompileTextValue(tv *model.TextValue) (*CompiledText, error) {
	if tv == nil {
		return nil, nil
	}
	if tv.Ref == nil {
		return &CompiledText{Literal: tv.Literal, HasLiteral: true}, nil
	}
	key, err := CompileSystemKey("system key reference", tv.Ref.Key, tv.Ref.Field, tv.Ref.Mode, tv.Ref.Path, true)
	if err != nil {
		return nil, err
	}
	compiled := &CompiledText{Key: key}
	if tv.Ref.Default != nil {
		value, err := utils.FromYAMLNode(tv.Ref.Default.Node)
		if err != nil {
			return nil, fmt.Errorf("default: %v", err)
		}
		compiled.Default = value
		compiled.HasDefault = true
	}
	return compiled, nil
}

// IsStatic 表示该字段不需要请求上下文即可求值（加载期可做长度校验）。
func (t *CompiledText) IsStatic() bool { return t.Key == nil }

// Resolve 求值：静态字面量直接返回；引用读取失败时报错，缺失时用 default。
func (t *CompiledText) Resolve(ctx *model.RequestContext) (any, error) {
	if t.Key == nil {
		return t.Literal, nil
	}
	value, found, err := t.Key.Read(ctx)
	if err != nil {
		return nil, err
	}
	if !found {
		if t.HasDefault {
			return t.Default, nil
		}
		return nil, fmt.Errorf("%s is not present in the request and has no default", t.Key.String())
	}
	return value, nil
}

// ResolveString 求值并要求结果是 JSON 字符串；其他类型一律报错。
func (t *CompiledText) ResolveString(field string, ctx *model.RequestContext) (string, error) {
	value, err := t.Resolve(ctx)
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must resolve to a string, got %s", field, describeJSONValue(value))
	}
	return text, nil
}

// outputValueKind 是 CompiledValue 的三种形态。
const (
	outputValueStatic = iota
	outputValueRef
	outputValueObject
	outputValueArray
)

// CompiledValue 是 arguments 这类任意 JSON 值的编译结果：字面量、系统 key 引用
// 或复合结构。object 的 key 必须是字符串，加载期按字典序定序。
type CompiledValue struct {
	Kind   int
	Static any
	Text   *CompiledText
	// Keys 是 object 成员的字典序 key 列表，保证遍历与序列化顺序稳定。
	Keys   []string
	Object map[string]*CompiledValue
	Array  []*CompiledValue
}

// CompileOutputValue 编译一个可序列化为 JSON 的值。rel 是错误信息里的相对路径，
// 例如 "arguments"；叶子节点报错时会带上 ".city" / "[0]" 这样的后缀。
func CompileOutputValue(rel string, n *yaml.Node) (*CompiledValue, error) {
	if n == nil {
		return nil, valueError(rel, "value is required")
	}
	if ref, ok, err := asOutputValueRef(n); err != nil {
		return nil, valueError(rel, err.Error())
	} else if ok {
		text, err := CompileTextValue(ref)
		if err != nil {
			return nil, valueError(rel, err.Error())
		}
		return &CompiledValue{Kind: outputValueRef, Text: text}, nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		value, err := compileStaticScalar(n)
		if err != nil {
			return nil, valueError(rel, err.Error())
		}
		return &CompiledValue{Kind: outputValueStatic, Static: value}, nil
	case yaml.MappingNode:
		object := make(map[string]*CompiledValue, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			keyNode := n.Content[i]
			if keyNode.Kind != yaml.ScalarNode || keyNode.Tag != "!!str" {
				return nil, valueError(rel, "object keys must be strings without YAML-specific types")
			}
			name := keyNode.Value
			if _, duplicate := object[name]; duplicate {
				return nil, valueError(rel, fmt.Sprintf("duplicate object key %q", name))
			}
			child, err := CompileOutputValue(joinValuePath(rel, "."+name), n.Content[i+1])
			if err != nil {
				return nil, err
			}
			object[name] = child
		}
		keys := make([]string, 0, len(object))
		for name := range object {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		return &CompiledValue{Kind: outputValueObject, Keys: keys, Object: object}, nil
	case yaml.SequenceNode:
		array := make([]*CompiledValue, 0, len(n.Content))
		for i, item := range n.Content {
			child, err := CompileOutputValue(joinValuePath(rel, fmt.Sprintf("[%d]", i)), item)
			if err != nil {
				return nil, err
			}
			array = append(array, child)
		}
		return &CompiledValue{Kind: outputValueArray, Array: array}, nil
	case yaml.AliasNode:
		return nil, valueError(rel, "YAML alias is not supported")
	}
	return nil, valueError(rel, "must be a JSON value")
}

// IsStatic 表示该值全部由字面量组成，不含系统 key 引用。
func (v *CompiledValue) IsStatic() bool {
	switch v.Kind {
	case outputValueStatic:
		return true
	case outputValueRef:
		return false
	case outputValueObject:
		for _, name := range v.Keys {
			if !v.Object[name].IsStatic() {
				return false
			}
		}
		return true
	}
	for _, item := range v.Array {
		if !item.IsStatic() {
			return false
		}
	}
	return true
}

// Resolve 求值成与 JSON 同构的 Go 值：map[string]any、[]any、标量。
func (v *CompiledValue) Resolve(ctx *model.RequestContext) (any, error) {
	switch v.Kind {
	case outputValueStatic:
		return v.Static, nil
	case outputValueRef:
		return v.Text.Resolve(ctx)
	case outputValueObject:
		object := make(map[string]any, len(v.Keys))
		for _, name := range v.Keys {
			item, err := v.Object[name].Resolve(ctx)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", name, err)
			}
			object[name] = item
		}
		return object, nil
	}
	array := make([]any, 0, len(v.Array))
	for i, item := range v.Array {
		value, err := item.Resolve(ctx)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %v", i, err)
		}
		array = append(array, value)
	}
	return array, nil
}

// ResolveJSON 求值并确定性序列化：所有层级的 object key 按字典序、紧凑 JSON。
// （encoding/json 对 map key 排序，CompiledValue 也已经按键序插入，双保险。）
func (v *CompiledValue) ResolveJSON(ctx *model.RequestContext) ([]byte, error) {
	value, err := v.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(applyDeterministicOrder(value))
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// applyDeterministicOrder 把 map 递归换成有序结构，保证与 encoding/json 之外的
// 编码路径（例如流式骨架拼接）顺序一致。
func applyDeterministicOrder(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := utils.SortedKeys(typed)
		ordered := make(map[string]any, len(keys))
		for _, key := range keys {
			ordered[key] = applyDeterministicOrder(typed[key])
		}
		return ordered
	case []any:
		ordered := make([]any, len(typed))
		for i, item := range typed {
			ordered[i] = applyDeterministicOrder(item)
		}
		return ordered
	}
	return value
}

// asOutputValueRef 识别叶子形态的系统 key 引用：mapping 且字段集合被白名单覆盖、
// 含字符串 key。非引用形态返回 ok=false，由调用方按普通 JSON 值处理。
func asOutputValueRef(n *yaml.Node) (*model.TextValue, bool, error) {
	if n.Kind != yaml.MappingNode {
		return nil, false, nil
	}
	hasKey := false
	for i := 0; i+1 < len(n.Content); i += 2 {
		keyNode := n.Content[i]
		if keyNode.Kind != yaml.ScalarNode || keyNode.Tag != "!!str" {
			return nil, false, nil
		}
		switch keyNode.Value {
		case "key", "field", "mode", "path", "default":
		default:
			return nil, false, nil
		}
		if keyNode.Value != "key" {
			continue
		}
		if valueNode := n.Content[i+1]; valueNode.Kind != yaml.ScalarNode || valueNode.Tag != "!!str" {
			return nil, false, nil
		}
		hasKey = true
	}
	if !hasKey {
		return nil, false, nil
	}
	var ref model.SystemKeyRef
	if err := ref.UnmarshalYAML(n); err != nil {
		return nil, true, err
	}
	return &model.TextValue{Ref: &ref}, true, nil
}

// compileStaticScalar 只接受 JSON 能表达的标量标签，拒绝 YAML 专有类型。
func compileStaticScalar(n *yaml.Node) (any, error) {
	switch n.Tag {
	case "!!null", "!!bool", "!!int", "!!float", "!!str":
		value, err := utils.FromYAMLNode(n)
		if err != nil {
			return nil, err
		}
		return value, nil
	}
	return nil, fmt.Errorf("unsupported scalar type %s", n.Tag)
}

func valueError(rel, msg string) error {
	if rel == "" {
		return fmt.Errorf("%s", msg)
	}
	return fmt.Errorf("%s: %s", rel, msg)
}

func joinValuePath(rel, segment string) string {
	return rel + segment
}

// describeJSONValue 给出诊断用的值类型描述，不含 Go 类型名之外的细节。
func describeJSONValue(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case utils.Number, json.Number:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	}
	return fmt.Sprintf("%T", value)
}

// CompiledToolCall 是一个工具调用的编译结果；ID 为空表示由框架生成。
type CompiledToolCall struct {
	ID        string
	Name      string
	Arguments *CompiledValue
}

// ResolveID 返回工具调用 ID：显式配置原样使用，省略时按协议生成合法 ID。
// 同一请求内对同一个工具必须只调用一次并复用结果，避免流式与非流式不一致。
func (t *CompiledToolCall) ResolveID(protocol string) string {
	if t.ID != "" {
		return t.ID
	}
	return GenerateToolCallID(protocol)
}

// CompiledError 是 output.error 的编译结果；type/code 只能静态配置。
type CompiledError struct {
	Type    string
	Code    string
	Message *CompiledText
	Param   *CompiledText
}

// ResolveMessage / ResolveParam 在 prepare 阶段解析动态部分（可能为 nil）。
func (e *CompiledError) ResolveMessage(ctx *model.RequestContext) (string, bool, error) {
	if e == nil || e.Message == nil {
		return "", false, nil
	}
	text, err := e.Message.ResolveString("error.message", ctx)
	if err != nil {
		return "", false, err
	}
	return text, true, nil
}

func (e *CompiledError) ResolveParam(ctx *model.RequestContext) (string, bool, error) {
	if e == nil || e.Param == nil {
		return "", false, nil
	}
	text, err := e.Param.ResolveString("error.param", ctx)
	if err != nil {
		return "", false, err
	}
	return text, true, nil
}

// 各协议省略 tool call ID 时的前缀：OpenAI Chat 与 Responses 用 call_，
// Anthropic 用 toolu_，与真实服务端的 ID 形态一致（后接 24 位十六进制）。
const (
	toolIDPrefixOpenAI    = "call_"
	toolIDPrefixAnthropic = "toolu_"
	toolIDRandomBytes     = 12
)

// GenerateToolCallID 生成协议合法的 tool call ID。
func GenerateToolCallID(protocol string) string {
	prefix := toolIDPrefixOpenAI
	if protocol == "anthropic" {
		prefix = toolIDPrefixAnthropic
	}
	buf := make([]byte, toolIDRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败是环境级故障；退化为固定形态仍保证长度与字符集合法。
		return prefix + "000000000000000000000000"
	}
	return prefix + hex.EncodeToString(buf)
}

// ResolveUsageNumber 在 prepare 阶段解析 usage 数字字段。引用键缺失时回退到
// 外层（剧本/服务默认）已解析出的字面值；解析结果必须是 0 或正整数。
func ResolveUsageNumber(ctx *model.RequestContext, field string, n UsageNumber) (int64, error) {
	if n.Ref == nil {
		return n.Literal, nil
	}
	value, found, err := n.Ref.Read(ctx)
	if err != nil {
		return 0, fmt.Errorf("%s: %v", field, err)
	}
	if !found {
		if n.Fallback != nil {
			return *n.Fallback, nil
		}
		return 0, fmt.Errorf("%s: %s is not present in the request", field, n.Ref.String())
	}
	number, err := jsonInteger(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %v", field, err)
	}
	if number < 0 {
		return 0, fmt.Errorf("%s: must be a non-negative integer, got %d", field, number)
	}
	return number, nil
}

// JSON 数字文本用例走 utils.Number，整数校验不接受小数与科学计数法。
func jsonInteger(value any) (int64, error) {
	var text string
	switch typed := value.(type) {
	case utils.Number:
		text = string(typed)
	case json.Number:
		text = typed.String()
	default:
		return 0, fmt.Errorf("must be a non-negative integer, got %s", describeJSONValue(value))
	}
	number, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("must be a non-negative integer, got %s", text)
	}
	return number, nil
}
