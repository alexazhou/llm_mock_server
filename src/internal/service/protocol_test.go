package service

import (
	"encoding/json"
	"testing"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/model"
	"github.com/alexazhou/llm_mock_server/internal/utils"
)

func mustJSONPath(t *testing.T, path string, root any, want any) {
	t.Helper()
	compiled, err := utils.CompileJSONPath(path)
	if err != nil {
		t.Fatal(err)
	}
	got, found := compiled.Eval(root)
	if !found {
		t.Fatalf("%s: missing", path)
	}
	if !utils.Equal(got, want) {
		t.Fatalf("%s: got %#v want %#v", path, got, want)
	}
}

func TestParseOpenAIChat(t *testing.T) {
	body := `{"model":"gpt-4o","stream":true,"messages":[
		{"role":"system","content":"be nice"},
		{"role":"developer","content":"be terse"},
		{"role":"user","content":"hi"}]}`
	parsed, ferr := ParseProtocolRequest(constants.ProtocolOpenAI, []byte(body))
	if ferr != nil {
		t.Fatalf("unexpected error: %v", ferr)
	}
	if parsed.Model != "gpt-4o" || !parsed.Stream || parsed.OutputCount != 0 {
		t.Fatalf("unexpected parse result: %+v", parsed)
	}
	// 首轮必须剥离前置 system/developer，使 $[0] 对应首条用户输入。
	items := parsed.RoundInput.([]any)
	if len(items) != 1 {
		t.Fatalf("expected only the user message after stripping system/developer, got %d", len(items))
	}
	if role, _ := items[0].(map[string]any)["role"].(string); role != "user" {
		t.Fatalf("first round input must be the user message, got %v", role)
	}

	multi := `{"model":"gpt-4o","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"hello"},
		{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`
	parsed, ferr = ParseProtocolRequest(constants.ProtocolOpenAI, []byte(multi))
	if ferr != nil {
		t.Fatalf("unexpected error: %v", ferr)
	}
	if parsed.OutputCount != 1 {
		t.Fatalf("expected output_count 1, got %d", parsed.OutputCount)
	}
	items = parsed.RoundInput.([]any)
	if len(items) != 1 {
		t.Fatalf("round_input must contain only the new tool message, got %d", len(items))
	}
}

func TestParseAnthropicMessages(t *testing.T) {
	body := `{"model":"claude-3","max_tokens":10,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"text","text":"hello"}]},
		{"role":"user","content":"again"}]}`
	parsed, ferr := ParseProtocolRequest(constants.ProtocolAnthropic, []byte(body))
	if ferr != nil {
		t.Fatalf("unexpected error: %v", ferr)
	}
	if parsed.Model != "claude-3" || parsed.Stream || parsed.OutputCount != 1 {
		t.Fatalf("unexpected parse result: %+v", parsed)
	}
	items := parsed.RoundInput.([]any)
	if len(items) != 1 {
		t.Fatalf("expected one new user message, got %d", len(items))
	}
	if content, _ := items[0].(map[string]any)["content"].(string); content != "again" {
		t.Fatalf("unexpected round_input: %#v", items[0])
	}
}

func TestParseResponsesGroupsModelOutputs(t *testing.T) {
	// 一组并行 function_call（中间无 function_call_output）只算一次模型输出。
	parallel := `{"model":"gpt-5","input":[
		{"role":"user","content":"hi"},
		{"type":"reasoning","summary":[]},
		{"role":"assistant","content":[{"type":"output_text","text":"x"}]},
		{"type":"function_call","name":"a","arguments":"{}"},
		{"type":"function_call","name":"b","arguments":"{}"}]}`
	parsed, ferr := ParseProtocolRequest(constants.ProtocolResponses, []byte(parallel))
	if ferr != nil {
		t.Fatalf("unexpected error: %v", ferr)
	}
	if parsed.OutputCount != 1 {
		t.Fatalf("parallel outputs must count as one round, got %d", parsed.OutputCount)
	}
	if items := parsed.RoundInput.([]any); len(items) != 0 {
		t.Fatalf("expected empty round_input after the model output group, got %d", len(items))
	}

	// 顺序调用（中间夹 function_call_output）算多轮。
	sequential := `{"model":"gpt-5","input":[
		{"role":"user","content":"hi"},
		{"type":"reasoning","summary":[]},
		{"type":"function_call","name":"a","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"1"},
		{"type":"function_call","name":"b","arguments":"{}"},
		{"type":"function_call_output","call_id":"c2","output":"2"},
		{"role":"user","content":"done"}]}`
	parsed, ferr = ParseProtocolRequest(constants.ProtocolResponses, []byte(sequential))
	if ferr != nil {
		t.Fatalf("unexpected error: %v", ferr)
	}
	if parsed.OutputCount != 2 {
		t.Fatalf("sequential tool calls must count as two rounds, got %d", parsed.OutputCount)
	}
	// round_input 是「最后一个模型输出之后的所有输入项」：function_call_output + 新 user 消息。
	items := parsed.RoundInput.([]any)
	if len(items) != 2 {
		t.Fatalf("expected function_call_output + user message as round_input, got %d", len(items))
	}
	if itemType, _ := items[0].(map[string]any)["type"].(string); itemType != "function_call_output" {
		t.Fatalf("unexpected first round_input item: %#v", items[0])
	}
}

func TestParseResponsesStringInput(t *testing.T) {
	body := `{"model":"gpt-5","input":"hello"}`
	parsed, ferr := ParseProtocolRequest(constants.ProtocolResponses, []byte(body))
	if ferr != nil {
		t.Fatalf("unexpected error: %v", ferr)
	}
	if parsed.OutputCount != 0 {
		t.Fatalf("expected output_count 0, got %d", parsed.OutputCount)
	}
	items := parsed.RoundInput.([]any)
	if len(items) != 1 {
		t.Fatalf("string input must normalize to a single item, got %d", len(items))
	}
	if content, _ := items[0].(map[string]any)["content"].(string); content != "hello" {
		t.Fatalf("unexpected normalized item: %#v", items[0])
	}
}

func TestParseResponsesPreviousResponseIDRejected(t *testing.T) {
	body := `{"model":"gpt-5","previous_response_id":"resp_1"}`
	_, ferr := ParseProtocolRequest(constants.ProtocolResponses, []byte(body))
	if ferr == nil || ferr.Code != model.CodeOutputCountUnavailable {
		t.Fatalf("expected mock_output_count_unavailable, got %v", ferr)
	}
	if ferr.Status != 422 {
		t.Fatalf("expected 422, got %d", ferr.Status)
	}

	empty := `{"model":"gpt-5","input":[],"previous_response_id":"resp_1"}`
	if _, ferr := ParseProtocolRequest(constants.ProtocolResponses, []byte(empty)); ferr == nil || ferr.Code != model.CodeOutputCountUnavailable {
		t.Fatalf("expected mock_output_count_unavailable for empty input, got %v", ferr)
	}
}

func TestParseProtocolRejects(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		body     string
		wantCode string
	}{
		{"invalid json", constants.ProtocolOpenAI, `{"model":`, model.CodeInvalidJSON},
		{"two json values", constants.ProtocolOpenAI, `{"model":"m","messages":[]}{"x":1}`, model.CodeInvalidJSON},
		{"trailing garbage", constants.ProtocolOpenAI, `{"model":"m","messages":[]} junk`, model.CodeInvalidJSON},
		{"not an object", constants.ProtocolOpenAI, `[]`, model.CodeProtocolInvalid},
		{"model missing", constants.ProtocolOpenAI, `{"messages":[]}`, model.CodeProtocolInvalid},
		{"model not string", constants.ProtocolOpenAI, `{"model":1,"messages":[]}`, model.CodeProtocolInvalid},
		{"stream not bool", constants.ProtocolOpenAI, `{"model":"m","stream":"yes","messages":[]}`, model.CodeProtocolInvalid},
		{"messages missing", constants.ProtocolAnthropic, `{"model":"m"}`, model.CodeProtocolInvalid},
		{"messages not array", constants.ProtocolAnthropic, `{"model":"m","messages":{}}`, model.CodeProtocolInvalid},
		{"message role missing", constants.ProtocolAnthropic, `{"model":"m","messages":[{"content":"x"}]}`, model.CodeProtocolInvalid},
	}
	for _, c := range cases {
		_, ferr := ParseProtocolRequest(c.protocol, []byte(c.body))
		if ferr == nil || ferr.Code != c.wantCode {
			t.Fatalf("%s: expected %s, got %v", c.name, c.wantCode, ferr)
		}
	}
}

func TestParseKeepsNumberText(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"x"}],"temperature":1.50}`
	parsed, ferr := ParseProtocolRequest(constants.ProtocolOpenAI, []byte(body))
	if ferr != nil {
		t.Fatalf("unexpected error: %v", ferr)
	}
	mustJSONPath(t, "$.temperature", parsed.JSONBody, utils.Number("1.50"))
}

func TestNormalizeJSONDropsJSONNumberType(t *testing.T) {
	var parsed any
	if err := json.Unmarshal([]byte(`{"n":1}`), &parsed); err != nil {
		t.Fatal(err)
	}
	normalized := utils.NormalizeJSON(parsed).(map[string]any)
	if utils.TypeName(normalized["n"]) != "number" {
		t.Fatalf("expected number, got %s", utils.TypeName(normalized["n"]))
	}
}
