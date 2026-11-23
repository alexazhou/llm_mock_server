package service

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/model"
)

// prepareFor 加载一份单剧本夹具并解析第 1 个 output 的 prepared 快照。
func prepareFor(t *testing.T, script, protocol, body string) (*PreparedOutput, *model.FrameworkError) {
	t.Helper()
	loaded, err := Load(writeFixture(t, "", map[string]string{"probe.yaml": script}))
	if err != nil {
		t.Fatalf("load script: %v", err)
	}
	compiled := loaded.Compiled["probe"]
	if compiled == nil || len(compiled.Rounds) == 0 || len(compiled.Rounds[0].Outputs) == 0 {
		t.Fatal("fixture must compile one round output")
	}
	ctx := newValueContext(t, body)
	ctx.Protocol = protocol
	ctx.ScriptID = "probe"
	ctx.RoundIndex = 1
	count := uint64(1)
	ctx.Count = &count
	if model, ok := ctx.JSONBody.(map[string]any)["model"].(string); ok {
		ctx.Model = model
	}
	return PrepareOutput(ctx, &compiled.Rounds[0].Outputs[0], loaded.Resolved.MaxOutputBytes)
}

// decodeNonStream 走 prepare → 编码的完整链路。
func decodeNonStream(t *testing.T, script, protocol, body string) map[string]any {
	t.Helper()
	prepared, ferr := prepareFor(t, script, protocol, body)
	if ferr != nil {
		t.Fatalf("prepare: %v", ferr)
	}
	raw, err := encodeNonStream(prepared)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	return out
}

const contentOnlyScript = `version: 1
id: probe
rounds:
  - output:
      content: Hello world!!
`

func TestOpenAINonStreamContainer(t *testing.T) {
	body := decodeNonStream(t, contentOnlyScript, constants.ProtocolOpenAI, `{"model":"gpt-x"}`)
	if body["object"] != "chat.completion" {
		t.Fatalf("object = %v", body["object"])
	}
	if body["model"] != "gpt-x" {
		t.Fatalf("model must echo the request model, got %v", body["model"])
	}
	if _, ok := body["created"].(float64); !ok {
		t.Fatalf("created must be a number: %v", body["created"])
	}
	choices := body["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	if message["role"] != "assistant" || message["content"] != "Hello world!!" {
		t.Fatalf("unexpected message: %v", message)
	}
	if _, has := message["tool_calls"]; has {
		t.Fatalf("tool_calls must be omitted for text output: %v", message)
	}
	usage := body["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(0) || usage["completion_tokens"] != float64(0) ||
		usage["total_tokens"] != float64(0) {
		t.Fatalf("usage must default to zeros: %v", usage)
	}
}

func TestOpenAIUsageMapping(t *testing.T) {
	script := `version: 1
id: probe
config:
  usage:
    input_tokens: 3
    output_tokens: 5
    cached_input_tokens: 2
    reasoning_tokens: 4
rounds:
  - output:
      content: hi
`
	body := decodeNonStream(t, script, constants.ProtocolOpenAI, `{"model":"m"}`)
	usage := body["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(3) || usage["completion_tokens"] != float64(5) ||
		usage["total_tokens"] != float64(8) {
		t.Fatalf("unexpected usage: %v", usage)
	}
	details := usage["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(2) {
		t.Fatalf("cached_tokens not mapped: %v", details)
	}
	completion := usage["completion_tokens_details"].(map[string]any)
	if completion["reasoning_tokens"] != float64(4) {
		t.Fatalf("reasoning_tokens not mapped: %v", completion)
	}
}

func TestUsageOmit(t *testing.T) {
	script := `version: 1
id: probe
config:
  usage_mode: omit
rounds:
  - output:
      content: hi
`
	for _, protocol := range []string{constants.ProtocolOpenAI, constants.ProtocolAnthropic, constants.ProtocolResponses} {
		body := decodeNonStream(t, script, protocol, `{"model":"m"}`)
		if _, has := body["usage"]; has {
			t.Fatalf("%s: usage_mode omit must omit usage: %v", protocol, body["usage"])
		}
	}
}

func TestOpenAIReasoningAndToolCalls(t *testing.T) {
	script := `version: 1
id: probe
rounds:
  - output:
      reasoning: think first
      tool_calls:
        - name: get_weather
          arguments:
            zeta: 1
            alpha: {key: request.json_body, path: $.city}
            tool_id: {key: request.json_body, path: $.id}
        - id: call_explicit
          name: lookup
          arguments: {q: x}
`
	body := decodeNonStream(t, script, constants.ProtocolOpenAI,
		`{"model":"m","city":"Beijing","id":"abc"}`)
	choice := body["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	if message["content"] != nil {
		t.Fatalf("content must be null without content, got %v", message["content"])
	}
	if message["reasoning_content"] != "think first" {
		t.Fatalf("reasoning_content = %v", message["reasoning_content"])
	}
	calls := message["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("expected two tool calls, got %v", calls)
	}
	first := calls[0].(map[string]any)
	if first["type"] != "function" {
		t.Fatalf("tool type = %v", first["type"])
	}
	id, _ := first["id"].(string)
	if !strings.HasPrefix(id, toolIDPrefixOpenAI) || len(id) != len(toolIDPrefixOpenAI)+24 {
		t.Fatalf("generated tool id = %q", id)
	}
	function := first["function"].(map[string]any)
	if function["name"] != "get_weather" {
		t.Fatalf("function name = %v", function["name"])
	}
	if function["arguments"] != `{"alpha":"Beijing","tool_id":"abc","zeta":1}` {
		t.Fatalf("arguments must be deterministic compact JSON, got %v", function["arguments"])
	}
	second := calls[1].(map[string]any)
	if second["id"] != "call_explicit" {
		t.Fatalf("explicit id must be preserved: %v", second["id"])
	}
}

func TestAnthropicNonStreamContainer(t *testing.T) {
	script := `version: 1
id: probe
config:
  usage:
    input_tokens: 3
    output_tokens: 5
rounds:
  - output:
      reasoning: think first
      content: Hello world!!
      tool_calls:
        - name: get_weather
          arguments: {city: Beijing}
`
	body := decodeNonStream(t, script, constants.ProtocolAnthropic, `{"model":"claude-x"}`)
	if body["type"] != "message" || body["role"] != "assistant" || body["model"] != "claude-x" {
		t.Fatalf("unexpected container: %v", body)
	}
	blocks := body["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("expected thinking+text+tool_use, got %v", blocks)
	}
	thinking := blocks[0].(map[string]any)
	if thinking["type"] != "thinking" || thinking["thinking"] != "think first" ||
		thinking["signature"] != anthropicThinkingSignature {
		t.Fatalf("unexpected thinking block: %v", thinking)
	}
	text := blocks[1].(map[string]any)
	if text["type"] != "text" || text["text"] != "Hello world!!" {
		t.Fatalf("unexpected text block: %v", text)
	}
	toolUse := blocks[2].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["name"] != "get_weather" {
		t.Fatalf("unexpected tool_use block: %v", toolUse)
	}
	if id, _ := toolUse["id"].(string); !strings.HasPrefix(id, toolIDPrefixAnthropic) {
		t.Fatalf("anthropic tool id = %q", id)
	}
	input := toolUse["input"].(map[string]any)
	if input["city"] != "Beijing" {
		t.Fatalf("tool_use input must be the parsed JSON object: %v", input)
	}
	if body["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", body["stop_reason"])
	}
	usage := body["usage"].(map[string]any)
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(5) {
		t.Fatalf("unexpected usage: %v", usage)
	}
}

func TestResponsesNonStreamContainer(t *testing.T) {
	script := `version: 1
id: probe
rounds:
  - output:
      reasoning: think first
      content: Hello world!!
      tool_calls:
        - id: call_fixed
          name: get_weather
          arguments: {city: Beijing}
`
	body := decodeNonStream(t, script, constants.ProtocolResponses, `{"model":"gpt-5"}`)
	if body["object"] != "response" || body["status"] != "completed" || body["model"] != "gpt-5" {
		t.Fatalf("unexpected container: %v", body)
	}
	items := body["output"].([]any)
	if len(items) != 3 {
		t.Fatalf("expected reasoning+message+function_call, got %v", items)
	}
	reasoning := items[0].(map[string]any)
	if reasoning["type"] != "reasoning" {
		t.Fatalf("unexpected reasoning item: %v", reasoning)
	}
	summary := reasoning["summary"].([]any)[0].(map[string]any)
	if summary["type"] != "summary_text" || summary["text"] != "think first" {
		t.Fatalf("unexpected reasoning summary: %v", summary)
	}
	message := items[1].(map[string]any)
	if message["type"] != "message" || message["role"] != "assistant" || message["status"] != "completed" {
		t.Fatalf("unexpected message item: %v", message)
	}
	part := message["content"].([]any)[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "Hello world!!" {
		t.Fatalf("unexpected output_text: %v", part)
	}
	call := items[2].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_fixed" ||
		call["name"] != "get_weather" || call["status"] != "completed" {
		t.Fatalf("unexpected function_call item: %v", call)
	}
	if call["arguments"] != `{"city":"Beijing"}` {
		t.Fatalf("arguments must be a compact JSON string: %v", call["arguments"])
	}
}

func TestEmptyOutputProducesNoBody(t *testing.T) {
	script := `version: 1
id: probe
rounds:
  - output: {}
`
	prepared, ferr := prepareFor(t, script, constants.ProtocolOpenAI, `{"model":"m"}`)
	if ferr != nil {
		t.Fatalf("prepare: %v", ferr)
	}
	if !prepared.Empty {
		t.Fatal("output without fields must be marked empty")
	}
	raw, err := encodeNonStream(prepared)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("empty output must produce 0 bytes, got %d", len(raw))
	}
}

func TestProtocolErrorBodies(t *testing.T) {
	script := `version: 1
id: probe
rounds:
  - output:
      config:
        status: 503
      error:
        type: server_error
        code: upstream_unavailable
        param: model
        message: Mock upstream unavailable
`
	openai := decodeNonStream(t, script, constants.ProtocolOpenAI, `{"model":"m"}`)
	errBody := openai["error"].(map[string]any)
	if errBody["message"] != "Mock upstream unavailable" || errBody["type"] != "server_error" ||
		errBody["code"] != "upstream_unavailable" || errBody["param"] != "model" {
		t.Fatalf("unexpected openai error body: %v", errBody)
	}

	anthropic := decodeNonStream(t, script, constants.ProtocolAnthropic, `{"model":"m"}`)
	if anthropic["type"] != "error" {
		t.Fatalf("anthropic error must be type=error: %v", anthropic)
	}
	anthropicBody := anthropic["error"].(map[string]any)
	if anthropicBody["type"] != "server_error" || anthropicBody["message"] != "Mock upstream unavailable" {
		t.Fatalf("unexpected anthropic error body: %v", anthropicBody)
	}

	responses := decodeNonStream(t, script, constants.ProtocolResponses, `{"model":"m"}`)
	responsesBody := responses["error"].(map[string]any)
	if responsesBody["code"] != "upstream_unavailable" {
		t.Fatalf("unexpected responses error body: %v", responsesBody)
	}
}

func TestResponsesFailedForTwoHundredError(t *testing.T) {
	script := `version: 1
id: probe
config:
  status: 200
rounds:
  - output:
      error:
        type: invalid_request_error
        code: bad_city
        message: bad city
`
	body := decodeNonStream(t, script, constants.ProtocolResponses, `{"model":"m"}`)
	if body["object"] != "response" || body["status"] != "failed" {
		t.Fatalf("unexpected failed response: %v", body)
	}
	errBody := body["error"].(map[string]any)
	if errBody["message"] != "bad city" || errBody["code"] != "bad_city" {
		t.Fatalf("unexpected failed error body: %v", errBody)
	}
}

func TestPrepareErrorForTwoHundredErrorOnNonStream(t *testing.T) {
	script := `version: 1
id: probe
config:
  status: 200
  stream: false
rounds:
  - output:
      error:
        type: invalid_request_error
        message: bad city
`
	_, ferr := prepareFor(t, script, constants.ProtocolOpenAI, `{"model":"m"}`)
	if ferr == nil || ferr.Code != model.CodeOutputPrepareError {
		t.Fatalf("expected mock_output_prepare_error, got %v", ferr)
	}
	if ferr.Status != 500 {
		t.Fatalf("prepare error must be HTTP 500, got %d", ferr.Status)
	}
}

func TestPrepareResolvesDynamicContent(t *testing.T) {
	script := `version: 1
id: probe
rounds:
  - output:
      content: {key: request.json_body, path: $.echo}
`
	body := decodeNonStream(t, script, constants.ProtocolOpenAI, `{"model":"m","echo":"pong"}`)
	choice := body["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "pong" {
		t.Fatalf("dynamic content not resolved: %v", choice)
	}
	// 缺键且无 default ⇒ prepare 500。
	_, ferr := prepareFor(t, script, constants.ProtocolOpenAI, `{"model":"m"}`)
	if ferr == nil || ferr.Code != model.CodeOutputPrepareError {
		t.Fatalf("missing dynamic value must fail prepare, got %v", ferr)
	}
}

func TestLoadRejectsNonStreamContentTarget(t *testing.T) {
	script := `version: 1
id: probe
config:
  stream: false
rounds:
  - output:
      content: hello
      steps:
        - op: send
          target: content[:3]
        - op: end
`
	_, err := Load(writeFixture(t, "", map[string]string{"probe.yaml": script}))
	if err == nil || !strings.Contains(err.Error(), "non-streaming output only accepts headers.end") {
		t.Fatalf("non-streaming content target must fail the load, got %v", err)
	}
}

func TestLoadRejectsNonStreamHeartbeat(t *testing.T) {
	script := `version: 1
id: probe
config:
  stream: false
rounds:
  - output:
      content: hello
      steps:
        - op: send
          source: heartbeat
          text: ping
        - op: end
`
	_, err := Load(writeFixture(t, "", map[string]string{"probe.yaml": script}))
	if err == nil || !strings.Contains(err.Error(), "heartbeat requires streaming") {
		t.Fatalf("non-streaming heartbeat must fail the load, got %v", err)
	}
}

func TestPrepareRejectsResponsesStreamEnd(t *testing.T) {
	script := `version: 1
id: probe
config:
  stream: true
rounds:
  - output:
      content: hello
      steps:
        - op: send
          target: stream.end
        - op: end
`
	_, ferr := prepareFor(t, script, constants.ProtocolResponses, `{"model":"m"}`)
	if ferr == nil || ferr.Code != model.CodeOutputPrepareError {
		t.Fatalf("responses stream.end must fail prepare, got %v", ferr)
	}
}

func TestSplitChunks(t *testing.T) {
	body := []byte("0123456789")
	if units := splitChunks(nil, body); len(units) != 1 || string(units[0]) != "0123456789" {
		t.Fatalf("nil chunks must be a single unit: %v", units)
	}
	sized := &CompiledChunks{Size: 4, HasSize: true}
	units := splitChunks(sized, body)
	if len(units) != 3 || string(units[0]) != "0123" || string(units[2]) != "89" {
		t.Fatalf("size chunks = %v", units)
	}
	sizes := &CompiledChunks{Sizes: []int{2, 3}}
	units = splitChunks(sizes, body)
	if len(units) != 3 || string(units[0]) != "01" || string(units[1]) != "234" || string(units[2]) != "56789" {
		t.Fatalf("sizes chunks = %v", units)
	}
}

// withLimitServer 返回一个 max_output_bytes 很小的 server.yaml。
func withLimitServer(limit int) string {
	return "version: 1\nscripts_dir: scripts\nruntime:\n  max_output_bytes: " +
		strconv.Itoa(limit) + "\n"
}

func TestLoadRejectsStaticOutputOverLimit(t *testing.T) {
	_, err := Load(writeFixture(t, withLimitServer(5), map[string]string{"probe.yaml": contentOnlyScript}))
	if err == nil || !strings.Contains(err.Error(), "max_output_bytes") {
		t.Fatalf("static overflow must fail the load, got %v", err)
	}
}

func TestPrepareRejectsResolvedOutputOverLimit(t *testing.T) {
	script := `version: 1
id: probe
rounds:
  - output:
      content: {key: request.json_body, path: $.pad}
`
	loaded, err := Load(writeFixture(t, withLimitServer(5), map[string]string{"probe.yaml": script}))
	if err != nil {
		t.Fatalf("load script: %v", err)
	}
	ctx := newValueContext(t, `{"pad":"0123456789"}`)
	ctx.Protocol = constants.ProtocolOpenAI
	count := uint64(1)
	ctx.Count = &count
	_, ferr := PrepareOutput(ctx, &loaded.Compiled["probe"].Rounds[0].Outputs[0], loaded.Resolved.MaxOutputBytes)
	if ferr == nil || ferr.Code != model.CodeOutputPrepareError || ferr.Status != 500 {
		t.Fatalf("dynamic overflow must fail prepare with 500, got %v", ferr)
	}
	if ferr.Details["request_count"] != uint64(1) {
		t.Fatalf("prepare error must carry request_count, got %#v", ferr.Details)
	}
	if ferr.Details["protocol"] != constants.ProtocolOpenAI {
		t.Fatalf("prepare error must carry protocol, got %#v", ferr.Details)
	}
}

func TestPrepareErrorCarriesRequestContext(t *testing.T) {
	script := `version: 1
id: probe
rounds:
  - output:
      content: {key: request.json_body, path: $.absent}
`
	_, ferr := prepareFor(t, script, constants.ProtocolAnthropic, `{"model":"m"}`)
	if ferr == nil {
		t.Fatal("missing dynamic content must fail prepare")
	}
	if ferr.Details["script_id"] != "probe" || ferr.Details["protocol"] != constants.ProtocolAnthropic {
		t.Fatalf("prepare error must carry request context, got %#v", ferr.Details)
	}
	if ferr.Details["request_count"] != uint64(1) {
		t.Fatalf("prepare error must carry the consumed request_count, got %#v", ferr.Details)
	}
}
