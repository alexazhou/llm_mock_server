package service

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/alexazhou/llm_mock_server/internal/model"
	"github.com/alexazhou/llm_mock_server/internal/utils"
)

// parseValueNode 取一段 YAML 文本的根节点，用于直接驱动值编译。
func parseValueNode(t *testing.T, text string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("parse yaml: %v", err)
	}
	return utils.Root(&doc)
}

// newValueContext 构造带请求 body 的最小上下文（数字保留原始十进制文本）。
func newValueContext(t *testing.T, body string) *model.RequestContext {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return &model.RequestContext{
		BodyText: body, JSONBody: utils.NormalizeJSON(value), Protocol: "openai",
	}
}

func TestCompileTextValueLiteralAndRef(t *testing.T) {
	literal, err := CompileTextValue(&model.TextValue{Literal: "hi"})
	if err != nil {
		t.Fatalf("compile literal: %v", err)
	}
	if !literal.IsStatic() {
		t.Fatal("literal text must be static")
	}
	ctx := newValueContext(t, `{}`)
	text, err := literal.ResolveString("content", ctx)
	if err != nil || text != "hi" {
		t.Fatalf("resolve literal: %q %v", text, err)
	}
}

func TestCompileTextValueRefDefaultAndMissing(t *testing.T) {
	ref := &model.TextValue{Ref: &model.SystemKeyRef{
		Key:     KeyRequestJSONBody,
		Path:    "$.echo",
		Default: &model.Value{Node: parseValueNode(t, "fallback")},
	}}
	compiled, err := CompileTextValue(ref)
	if err != nil {
		t.Fatalf("compile ref: %v", err)
	}
	if compiled.IsStatic() {
		t.Fatal("ref text must not be static")
	}
	ctx := newValueContext(t, `{"echo": "value"}`)
	text, err := compiled.ResolveString("content", ctx)
	if err != nil || text != "value" {
		t.Fatalf("resolve ref: %q %v", text, err)
	}
	// 路径缺失时使用 default。
	ctx = newValueContext(t, `{}`)
	text, err = compiled.ResolveString("content", ctx)
	if err != nil || text != "fallback" {
		t.Fatalf("resolve default: %q %v", text, err)
	}
}

func TestCompileTextValueRefMissingWithoutDefault(t *testing.T) {
	ref := &model.TextValue{Ref: &model.SystemKeyRef{Key: KeyRequestJSONBody, Path: "$.echo"}}
	compiled, err := CompileTextValue(ref)
	if err != nil {
		t.Fatalf("compile ref: %v", err)
	}
	if _, err := compiled.ResolveString("content", newValueContext(t, `{}`)); err == nil {
		t.Fatal("missing key without default must fail")
	}
}

func TestResolveStringRejectsNonString(t *testing.T) {
	ref := &model.TextValue{Ref: &model.SystemKeyRef{Key: KeyRequestJSONBody, Path: "$.n"}}
	compiled, err := CompileTextValue(ref)
	if err != nil {
		t.Fatalf("compile ref: %v", err)
	}
	_, err = compiled.ResolveString("content", newValueContext(t, `{"n": 3}`))
	if err == nil || !strings.Contains(err.Error(), "must resolve to a string") {
		t.Fatalf("non-string value must be rejected: %v", err)
	}
}

func TestCompileTextValueRejectsCountInAssertOnlyKey(t *testing.T) {
	// content 允许 request.count；这里验证非法 key 名被拒绝。
	_, err := CompileTextValue(&model.TextValue{Ref: &model.SystemKeyRef{Key: "request.unknown"}})
	if err == nil {
		t.Fatal("unknown system key must be rejected")
	}
}

func TestResolveArgumentsDeterministicJSON(t *testing.T) {
	// 故意把 key 写成非字典序，验证序列化结果按字典序排列且紧凑。
	node := parseValueNode(t, "zeta: 1\nalpha: \"x\"\nnested: {b: true, a: [1, 2]}\n")
	compiled, err := CompileOutputValue("arguments", node)
	if err != nil {
		t.Fatalf("compile arguments: %v", err)
	}
	if !compiled.IsStatic() {
		t.Fatal("literal arguments must be static")
	}
	raw, err := compiled.ResolveJSON(newValueContext(t, `{}`))
	if err != nil {
		t.Fatalf("resolve json: %v", err)
	}
	want := `{"alpha":"x","nested":{"a":[1,2],"b":true},"zeta":1}`
	if string(raw) != want {
		t.Fatalf("deterministic json mismatch:\n got %s\nwant %s", raw, want)
	}
}

func TestResolveArgumentsWithDynamicLeaf(t *testing.T) {
	node := parseValueNode(t, "city: {key: request.json_body, path: $.city}\nzip: 100000\ntags: [{key: request.round_input, path: \"$[0]\", default: none}]\n")
	compiled, err := CompileOutputValue("arguments", node)
	if err != nil {
		t.Fatalf("compile arguments: %v", err)
	}
	if compiled.IsStatic() {
		t.Fatal("arguments with a system key reference must not be static")
	}
	ctx := newValueContext(t, `{"city": "Beijing"}`)
	ctx.RoundInput = []any{"first"}
	raw, err := compiled.ResolveJSON(ctx)
	if err != nil {
		t.Fatalf("resolve json: %v", err)
	}
	want := `{"city":"Beijing","tags":["first"],"zip":100000}`
	if string(raw) != want {
		t.Fatalf("dynamic json mismatch:\n got %s\nwant %s", raw, want)
	}
	// 缺失时用叶子 default。
	ctx = newValueContext(t, `{}`)
	raw, err = compiled.ResolveJSON(ctx)
	if err == nil {
		t.Fatalf("missing city has no default and must fail, got %s", raw)
	}
}

func TestCompileOutputValueRejectsNonStringKeys(t *testing.T) {
	_, err := CompileOutputValue("arguments", parseValueNode(t, "1: one\n"))
	if err == nil || !strings.Contains(err.Error(), "object keys must be strings") {
		t.Fatalf("integer key must be rejected: %v", err)
	}
}

func TestCompileOutputValueRejectsDuplicateKeys(t *testing.T) {
	_, err := CompileOutputValue("arguments", parseValueNode(t, "a: 1\nb: 2\na: 3\n"))
	if err == nil || !strings.Contains(err.Error(), "duplicate object key") {
		t.Fatalf("duplicate key must be rejected: %v", err)
	}
}

func TestCompileOutputValueRejectsYAMLSpecificScalar(t *testing.T) {
	_, err := CompileOutputValue("arguments", parseValueNode(t, "when: 2026-01-02T03:04:05Z\n"))
	if err == nil || !strings.Contains(err.Error(), "unsupported scalar type") {
		t.Fatalf("yaml timestamp must be rejected: %v", err)
	}
}

func TestCompileOutputValueReportsLeafPath(t *testing.T) {
	_, err := CompileOutputValue("arguments", parseValueNode(t, "outer: {inner: 2026-01-02T03:04:05Z}\n"))
	if err == nil || !strings.Contains(err.Error(), "arguments.outer.inner") {
		t.Fatalf("error must carry the leaf path: %v", err)
	}
}

func TestResolveToolCallID(t *testing.T) {
	explicit := &CompiledToolCall{ID: "call_fixed"}
	if explicit.ResolveID("openai") != "call_fixed" {
		t.Fatal("explicit id must be preserved")
	}
	openai := (&CompiledToolCall{}).ResolveID("openai")
	if !strings.HasPrefix(openai, toolIDPrefixOpenAI) || len(openai) != len(toolIDPrefixOpenAI)+24 {
		t.Fatalf("unexpected openai id %q", openai)
	}
	anthropic := (&CompiledToolCall{}).ResolveID("anthropic")
	if !strings.HasPrefix(anthropic, toolIDPrefixAnthropic) || len(anthropic) != len(toolIDPrefixAnthropic)+24 {
		t.Fatalf("unexpected anthropic id %q", anthropic)
	}
	if openai == (&CompiledToolCall{}).ResolveID("openai") {
		t.Fatal("generated ids must differ between calls")
	}
}

func TestResolveUsageNumber(t *testing.T) {
	ctx := newValueContext(t, `{"cached": 7}`)
	literal, err := ResolveUsageNumber(ctx, "usage.input_tokens", UsageNumber{Literal: 3})
	if err != nil || literal != 3 {
		t.Fatalf("literal usage: %d %v", literal, err)
	}
	key, err := CompileSystemKey("usage.cached_input_tokens", KeyRequestJSONBody, "", "", "$.cached", true)
	if err != nil {
		t.Fatalf("compile key: %v", err)
	}
	fallback := int64(11)
	value, err := ResolveUsageNumber(ctx, "usage.cached_input_tokens", UsageNumber{Ref: key, Fallback: &fallback})
	if err != nil || value != 7 {
		t.Fatalf("dynamic usage: %d %v", value, err)
	}
	// 键缺失时回退到外层值。
	missing, err := CompileSystemKey("usage.cached_input_tokens", KeyRequestJSONBody, "", "", "$.nope", true)
	if err != nil {
		t.Fatalf("compile key: %v", err)
	}
	value, err = ResolveUsageNumber(ctx, "usage.cached_input_tokens", UsageNumber{Ref: missing, Fallback: &fallback})
	if err != nil || value != 11 {
		t.Fatalf("missing usage must fall back: %d %v", value, err)
	}
	// 非整数/负数一律拒绝。
	negative, err := CompileSystemKey("usage.input_tokens", KeyRequestJSONBody, "", "", "$.neg", true)
	if err != nil {
		t.Fatalf("compile key: %v", err)
	}
	ctxNeg := newValueContext(t, `{"neg": -1, "frac": 1.5}`)
	if _, err := ResolveUsageNumber(ctxNeg, "usage.input_tokens", UsageNumber{Ref: negative}); err == nil {
		t.Fatal("negative usage must be rejected")
	}
	frac, err := CompileSystemKey("usage.input_tokens", KeyRequestJSONBody, "", "", "$.frac", true)
	if err != nil {
		t.Fatalf("compile key: %v", err)
	}
	if _, err := ResolveUsageNumber(ctxNeg, "usage.input_tokens", UsageNumber{Ref: frac}); err == nil {
		t.Fatal("fractional usage must be rejected")
	}
}

// TestLoadCompilesOutputValues 验证加载期把 content/reasoning/tool_calls/error 编译进 output。
func TestLoadCompilesOutputValues(t *testing.T) {
	script := `version: 1
id: values
rounds:
  - output:
      content: hello
      reasoning: think
      tool_calls:
        - name: get_weather
          arguments:
            city: Beijing
`
	loaded, err := Load(writeFixture(t, "", map[string]string{"values.yaml": script}))
	if err != nil {
		t.Fatalf("load script: %v", err)
	}
	output := loaded.Compiled["values"].Rounds[0].Outputs[0]
	if output.Content == nil || output.Content.Literal != "hello" {
		t.Fatalf("content not compiled: %+v", output.Content)
	}
	if output.Reasoning == nil || output.Reasoning.Literal != "think" {
		t.Fatalf("reasoning not compiled: %+v", output.Reasoning)
	}
	if len(output.ToolCalls) != 1 || output.ToolCalls[0].Name != "get_weather" ||
		output.ToolCalls[0].Arguments == nil {
		t.Fatalf("tool calls not compiled: %+v", output.ToolCalls)
	}
	if output.Error != nil {
		t.Fatalf("normal output must not compile an error: %+v", output.Error)
	}
}

// TestLoadCompilesOutputError 验证 error.message/param 也被编译进 output。
func TestLoadCompilesOutputError(t *testing.T) {
	script := `version: 1
id: values_error
rounds:
  - output:
      error:
        type: invalid_request_error
        code: bad_city
        message: bad city
        param: city
`
	loaded, err := Load(writeFixture(t, "", map[string]string{"values_error.yaml": script}))
	if err != nil {
		t.Fatalf("load script: %v", err)
	}
	protoErr := loaded.Compiled["values_error"].Rounds[0].Outputs[0].Error
	if protoErr == nil || protoErr.Code != "bad_city" || protoErr.Message == nil || protoErr.Param == nil {
		t.Fatalf("error not compiled: %+v", protoErr)
	}
	ctx := newValueContext(t, `{}`)
	if text, ok, err := protoErr.ResolveMessage(ctx); err != nil || !ok || text != "bad city" {
		t.Fatalf("resolve error message: %q %v %v", text, ok, err)
	}
}

// TestLoadRejectsInvalidToolArguments 验证非法 arguments 在启动期被拒并带位置。
func TestLoadRejectsInvalidToolArguments(t *testing.T) {
	script := `version: 1
id: badargs
rounds:
  - output:
      tool_calls:
        - name: get_weather
          arguments:
            city:
              key: ""
`
	_, err := Load(writeFixture(t, "", map[string]string{"badargs.yaml": script}))
	if err == nil {
		t.Fatal("invalid arguments must fail the load")
	}
	message := err.Error()
	if !strings.Contains(message, "badargs.yaml:") || !strings.Contains(message, "tool_calls[0]") {
		t.Fatalf("error must carry file and path: %v", message)
	}
}

// TestLoadRejectsAliasInArguments 验证 YAML alias 在 arguments 里被拒绝。
func TestLoadRejectsAliasInArguments(t *testing.T) {
	script := `version: 1
id: aliasargs
rounds:
  - output:
      tool_calls:
        - name: get_weather
          arguments:
            first: &a 1
            second: *a
`
	_, err := Load(writeFixture(t, "", map[string]string{"alias.yaml": script}))
	if err == nil || !strings.Contains(err.Error(), "alias") {
		t.Fatalf("alias must be rejected: %v", err)
	}
}

// TestLoadAllowsPlaceholderKeyInArguments 验证 {key: ...} 之外的同名字段不被误判为引用。
func TestLoadAllowsNonRefArgumentsObject(t *testing.T) {
	script := `version: 1
id: nonref
rounds:
  - output:
      tool_calls:
        - name: echo
          arguments:
            key: fixed
            payload: {a: 1}
`
	loaded, err := Load(writeFixture(t, "", map[string]string{"nonref.yaml": script}))
	if err != nil {
		t.Fatalf("load script: %v", err)
	}
	args := loaded.Compiled["nonref"].Rounds[0].Outputs[0].ToolCalls[0].Arguments
	if !args.IsStatic() {
		t.Fatal("object without a system key reference must be static")
	}
	raw, err := args.ResolveJSON(newValueContext(t, `{}`))
	if err != nil {
		t.Fatalf("resolve json: %v", err)
	}
	if string(raw) != `{"key":"fixed","payload":{"a":1}}` {
		t.Fatalf("unexpected json: %s", raw)
	}
}

// TestLoadAllowsQuotedKeyFieldAsLiteral 说明引用识别只认白名单字段组合：
// 带其他字段的 mapping 按普通 object 处理。
func TestLoadAllowsMixedArgumentsObject(t *testing.T) {
	script := `version: 1
id: mixed
rounds:
  - output:
      tool_calls:
        - name: echo
          arguments:
            key: {nested: true}
`
	loaded, err := Load(writeFixture(t, "", map[string]string{"mixed.yaml": script}))
	if err != nil {
		t.Fatalf("load script: %v", err)
	}
	args := loaded.Compiled["mixed"].Rounds[0].Outputs[0].ToolCalls[0].Arguments
	if !args.IsStatic() {
		t.Fatal("non-string key field must not be treated as a reference")
	}
}
