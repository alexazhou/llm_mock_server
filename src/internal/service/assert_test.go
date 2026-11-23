package service

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/alexazhou/llm_mock_server/internal/model"
	"github.com/alexazhou/llm_mock_server/internal/utils"
)

// mustAssertion 从 YAML 片段构造一条 model.Assertion。
func mustAssertion(t *testing.T, raw string) model.Assertion {
	t.Helper()
	var wrapper struct {
		A model.Assertion `yaml:"a"`
	}
	if err := yaml.Unmarshal([]byte("a:\n"+raw), &wrapper); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return wrapper.A
}

func compileOne(t *testing.T, raw string) *CompiledAssertion {
	t.Helper()
	list := model.AssertionList{mustAssertion(t, raw)}
	compiled, err := CompileAssertionList("assert", list, false)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	if len(compiled) != 1 {
		t.Fatalf("expected 1 compiled assertion, got %d", len(compiled))
	}
	return compiled[0]
}

func testContext(t *testing.T, body string, header http.Header, query url.Values) *model.RequestContext {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	return &model.RequestContext{
		RequestID:   "req_test",
		ScriptID:    "demo",
		Protocol:    "openai",
		Method:      http.MethodPost,
		Path:        "/mock/demo/v1/chat/completions",
		BodyText:    body,
		JSONBody:    utils.NormalizeJSON(parsed),
		Header:      header,
		Query:       query,
		Model:       "gpt-test",
		Stream:      false,
		OutputCount: 1,
		RoundInput:  []any{map[string]any{"role": "tool", "content": "ok"}},
		RoundIndex:  2,
	}
}

func TestCompileAssertionRejects(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"unknown key", "  key: request.nope\n  op: exists\n", "unknown system key"},
		{"count in assert", "  key: request.count\n  op: eq\n  expected: 1\n", "cannot be used in assert"},
		{"exists with expected", "  key: request.method\n  op: exists\n  expected: POST\n", "must not define expected or default"},
		{"expected required", "  key: request.method\n  op: eq\n", "requires expected"},
		{"unknown op", "  key: request.method\n  op: between\n  expected: 1\n", "unknown op"},
		{"bad mode", "  key: request.header\n  field: X-A\n  mode: many\n  op: exists\n", "mode must be single or values"},
		{"field on scalar key", "  key: request.method\n  field: X-A\n  op: exists\n", "does not accept field/mode/path"},
		{"invalid path", "  key: request.json_body\n  path: $..a\n  op: exists\n", "recursive descent"},
		{"duplicate name", "  key: request.method\n  name: dup\n  op: exists\n", ""},
		{"bad regex", "  key: request.method\n  op: regex\n  expected: \"(\"\n", "invalid regex"},
		{"bad type name", "  key: request.method\n  op: type\n  expected: integer\n", "type expects"},
		{"negative length", "  key: request.method\n  op: length_eq\n  expected: -1\n", "non-negative integer"},
	}
	for _, c := range cases {
		if c.name == "duplicate name" {
			list := model.AssertionList{
				mustAssertion(t, "  key: request.method\n  name: dup\n  op: exists\n"),
				mustAssertion(t, "  key: request.path\n  name: dup\n  op: exists\n"),
			}
			if _, err := CompileAssertionList("assert", list, false); err == nil || !strings.Contains(err.Error(), "duplicate assertion name") {
				t.Fatalf("expected duplicate name error, got %v", err)
			}
			continue
		}
		list := model.AssertionList{mustAssertion(t, c.raw)}
		_, err := CompileAssertionList("assert", list, false)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: expected error containing %q, got %v", c.name, c.want, err)
		}
	}
}

func TestAssertionEvaluateOperators(t *testing.T) {
	header := http.Header{"X-Tenant": []string{"acme"}}
	query := url.Values{"tenant": []string{"acme"}}
	body := `{"messages":[{"role":"user","content":"hi"}],"n":1.0,"tags":["a","b"],"cached_tokens":7}`
	ctx := testContext(t, body, header, query)

	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"eq number textual equality", "  key: request.json_body\n  path: $.n\n  op: eq\n  expected: 1\n", true},
		{"ne", "  key: request.json_body\n  path: $.n\n  op: ne\n  expected: 2\n", true},
		{"type array", "  key: request.json_body\n  path: $.tags\n  op: type\n  expected: array\n", true},
		{"contains substring", "  key: request.body\n  op: contains\n  expected: messages\n", true},
		{"contains array element", "  key: request.json_body\n  path: $.tags\n  op: contains\n  expected: b\n", true},
		{"regex", "  key: request.path\n  op: regex\n  expected: \"^/mock/[a-z]+/v1/chat/completions$\"\n", true},
		{"gt", "  key: request.json_body\n  path: $.cached_tokens\n  op: gt\n  expected: 3\n", true},
		{"gte equal", "  key: request.json_body\n  path: $.cached_tokens\n  op: gte\n  expected: 7\n", true},
		{"lt fails", "  key: request.output_count\n  op: lt\n  expected: 1\n", false},
		{"length string code points", "  key: request.json_body\n  path: $.messages[0].content\n  op: length_eq\n  expected: 2\n", true},
		{"length array", "  key: request.json_body\n  path: $.tags\n  op: length_gte\n  expected: 2\n", true},
		{"exists header", "  key: request.header\n  field: x-tenant\n  op: exists\n", true},
		{"absent header", "  key: request.header\n  field: X-Missing\n  op: absent\n", true},
		{"header eq query ref", "  key: request.header\n  field: X-Tenant\n  op: eq\n  expected:\n    key: request.query\n    field: tenant\n", true},
		{"round index", "  key: round.index\n  op: eq\n  expected: 2\n", true},
		{"script id", "  key: script.id\n  op: eq\n  expected: demo\n", true},
		{"protocol", "  key: request.protocol\n  op: eq\n  expected: openai\n", true},
		{"model", "  key: request.model\n  op: eq\n  expected: gpt-test\n", true},
		{"stream false", "  key: request.stream\n  op: eq\n  expected: false\n", true},
		{"missing path fails eq", "  key: request.json_body\n  path: $.nope\n  op: eq\n  expected: 1\n", false},
	}
	for _, c := range cases {
		assertion := compileOne(t, c.raw)
		result := assertion.Evaluate(ctx)
		if result.EvaluatorError {
			t.Fatalf("%s: unexpected evaluator error: %s", c.name, result.Reason)
		}
		if result.Matched != c.want {
			t.Fatalf("%s: matched=%v want %v (reason %q)", c.name, result.Matched, c.want, result.Reason)
		}
	}
}

func TestAssertionDefaultOnlyReplacesMissing(t *testing.T) {
	ctx := testContext(t, `{"messages":[],"zero":0,"nullv":null,"emptystr":""}`, http.Header{}, url.Values{})

	withDefault := compileOne(t, "  key: request.header\n  field: X-Missing\n  default: public\n  op: eq\n  expected: public\n")
	result := withDefault.Evaluate(ctx)
	if !result.Matched || !result.DefaultApplied {
		t.Fatalf("default must replace missing: %+v", result)
	}

	// null / 0 / "" 不会触发 default。
	for _, path := range []string{"$.nullv", "$.zero", "$.emptystr"} {
		assertion := compileOne(t, "  key: request.json_body\n  path: "+path+"\n  default: fallback\n  op: eq\n  expected: fallback\n")
		if res := assertion.Evaluate(ctx); res.Matched || res.DefaultApplied {
			t.Fatalf("%s: default must not replace present values: %+v", path, res)
		}
	}
}

func TestAssertionMultiValueHeaderWithoutValuesMode(t *testing.T) {
	ctx := testContext(t, `{"messages":[]}`, http.Header{"X-Multi": []string{"a", "b"}}, url.Values{})
	assertion := compileOne(t, "  key: request.header\n  field: X-Multi\n  op: eq\n  expected: a\n")
	result := assertion.Evaluate(ctx)
	if !result.EvaluatorError || !strings.Contains(result.Reason, "mode is single") {
		t.Fatalf("expected key read error for multi-value header, got %+v", result)
	}

	valuesMode := compileOne(t, "  key: request.header\n  field: X-Multi\n  mode: values\n  op: length_eq\n  expected: 2\n")
	if res := valuesMode.Evaluate(ctx); !res.Matched {
		t.Fatalf("values mode should return array: %+v", res)
	}
}

func TestAssertionCountAvailableOnlyAfterAllocation(t *testing.T) {
	ctx := testContext(t, `{"messages":[]}`, http.Header{}, url.Values{})
	assertion, err := CompileSystemKey("cond[0]", "request.count", "", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := assertion.Read(ctx); err != nil || found {
		t.Fatalf("request.count must be missing before allocation: found=%v err=%v", found, err)
	}
	count := uint64(3)
	ctx.Count = &count
	value, found, err := assertion.Read(ctx)
	if err != nil || !found || !utils.Equal(value, utils.Number("3")) {
		t.Fatalf("request.count must read %d, got %v found=%v err=%v", count, value, found, err)
	}
}
