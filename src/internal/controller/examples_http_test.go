package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexazhou/llm_mock_server/internal/service"
)

// 本文件验证 `src/examples/scripts` 里的示例剧本能真实工作：全部走 HTTP，
// 每类剧本至少一条端到端判据。传输异常（挂起/RST/分片）在 examples_socket_test.go。

// exampleServer 加载仓库内的示例剧本库并起真实 HTTP 服务。
func exampleServer(t *testing.T) string {
	t.Helper()
	loaded, err := service.Load("../../examples/server.yaml")
	if err != nil {
		t.Fatalf("load examples: %v", err)
	}
	server := httptest.NewServer(NewRouter(loaded))
	t.Cleanup(server.Close)
	return server.URL
}

// scriptPaths 是三条标准协议路径的后缀。
var scriptPaths = map[string]string{
	"openai":    "/v1/chat/completions",
	"anthropic": "/v1/messages",
	"responses": "/v1/responses",
}

// openAIBody 是最小 OpenAI 请求体。
func openAIBody(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`
}

// anthropicBody 是最小 Anthropic 请求体。
func anthropicBody() string {
	return `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`
}

// responsesBody 是最小 Responses 请求体。
func responsesBody() string {
	return `{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`
}

// postScript 向 /mock/{script}/v1/... 发一次请求并读回完整响应体。
func postScript(t *testing.T, base, script, protocol, body string) (int, string) {
	t.Helper()
	response, err := http.Post(base+"/mock/"+script+scriptPaths[protocol], "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s/%s: %v", script, protocol, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s/%s: %v", script, protocol, err)
	}
	return response.StatusCode, string(raw)
}

// TestExamplesServeThreeProtocols 锁定「每个剧本固定生成三条 URL 且都能响应」：
// 同一份 output 经三条协议 URL 各自返回该协议的容器。
func TestExamplesServeThreeProtocols(t *testing.T) {
	base := exampleServer(t)
	cases := []struct {
		protocol string
		body     string
		// marker 是响应里必须出现的协议特征（对象类型或字段名）。
		marker string
	}{
		{"openai", openAIBody("gpt-4o-mini"), `"choices"`},
		{"anthropic", anthropicBody(), `"content"`},
		{"responses", responsesBody(), `"output"`},
	}
	for _, tc := range cases {
		status, body := postScript(t, base, "chat-non-stream", tc.protocol, tc.body)
		if status != http.StatusOK {
			t.Fatalf("%s status = %d (%s)", tc.protocol, status, body)
		}
		if !strings.Contains(body, tc.marker) {
			t.Fatalf("%s response missing %s: %s", tc.protocol, tc.marker, body)
		}
		if !strings.Contains(body, "Hello!") {
			t.Fatalf("%s response missing example content: %s", tc.protocol, body)
		}
	}
}

// TestExampleStreamEndsWithDone 锁定流式示例以协议结束标记收尾。
func TestExampleStreamEndsWithDone(t *testing.T) {
	base := exampleServer(t)
	status, body := postScript(t, base, "chat-stream", "openai",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("stream status = %d (%s)", status, body)
	}
	if !strings.HasSuffix(strings.TrimRight(body, "\r\n"), "data: [DONE]") {
		t.Fatalf("openai stream must end with [DONE]: %q", body)
	}
}

// TestExampleToolTwoRound 锁定工具两轮：第 1 轮自动生成 tool call ID，
// 第 2 轮把历史里的 ID 回显到 content。
func TestExampleToolTwoRound(t *testing.T) {
	base := exampleServer(t)

	round1 := `{"model":"m","messages":[{"role":"user","content":"北京"}]}`
	status, body := postScript(t, base, "tool-two-round", "openai", round1)
	if status != http.StatusOK {
		t.Fatalf("round1 status = %d (%s)", status, body)
	}
	var first struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &first); err != nil {
		t.Fatalf("decode round1: %v (%s)", err, body)
	}
	if len(first.Choices) == 0 || len(first.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("round1 must return one tool call: %s", body)
	}
	call := first.Choices[0].Message.ToolCalls[0]
	if call.ID == "" {
		t.Fatalf("tool call id must be generated: %s", body)
	}
	if !strings.Contains(call.Function.Arguments, "北京") {
		t.Fatalf("arguments must reference the request dynamically, got %s", call.Function.Arguments)
	}

	// 第 2 轮：历史里带上第 1 轮的 tool call 与 tool result。
	round2 := `{"model":"m","messages":[` +
		`{"role":"user","content":"北京"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"` + call.ID + `","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},` +
		`{"role":"tool","tool_call_id":"` + call.ID + `","content":"晴"}]}`
	status, body = postScript(t, base, "tool-two-round", "openai", round2)
	if status != http.StatusOK {
		t.Fatalf("round2 status = %d (%s)", status, body)
	}
	var second struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &second); err != nil {
		t.Fatalf("decode round2: %v (%s)", err, body)
	}
	if len(second.Choices) == 0 {
		t.Fatalf("round2 must return a choice: %s", body)
	}
	if second.Choices[0].Message.Content != call.ID {
		t.Fatalf("round2 content should echo round1 tool call id %q, got %q",
			call.ID, second.Choices[0].Message.Content)
	}
}

// TestExampleCountDrivenOutputs 锁定「首次失败后成功」与「第 2 次失败」两个示例。
func TestExampleCountDrivenOutputs(t *testing.T) {
	base := exampleServer(t)
	body := openAIBody("m")

	if status, raw := postScript(t, base, "retry-then-success", "openai", body); status != http.StatusServiceUnavailable {
		t.Fatalf("retry-then-success #1 must be 503, got %d (%s)", status, raw)
	}
	if status, raw := postScript(t, base, "retry-then-success", "openai", body); status != http.StatusOK {
		t.Fatalf("retry-then-success #2 must be 200, got %d (%s)", status, raw)
	}

	if status, raw := postScript(t, base, "fail-second", "openai", body); status != http.StatusOK {
		t.Fatalf("fail-second #1 must be 200, got %d (%s)", status, raw)
	}
	if status, raw := postScript(t, base, "fail-second", "openai", body); status != http.StatusInternalServerError {
		t.Fatalf("fail-second #2 must be 500, got %d (%s)", status, raw)
	}
}

// TestExampleConditionBranchAndNoMatch 锁定按请求内容分支与无匹配诊断。
func TestExampleConditionBranchAndNoMatch(t *testing.T) {
	base := exampleServer(t)

	withTools := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather"}}]}`
	status, raw := postScript(t, base, "tools-branch", "openai", withTools)
	if status != http.StatusOK || !strings.Contains(raw, "get_weather") {
		t.Fatalf("tools-branch with tools must return a tool call: %d %s", status, raw)
	}
	withoutTools := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	status, raw = postScript(t, base, "tools-branch", "openai", withoutTools)
	if status != http.StatusOK || strings.Contains(raw, "get_weather") {
		t.Fatalf("tools-branch without tools must return plain text: %d %s", status, raw)
	}

	// 无匹配：唯一候选带 when，请求不带 tools ⇒ mock_output_not_found。
	status, raw = postScript(t, base, "no-match", "openai", withoutTools)
	if status != http.StatusInternalServerError || !strings.Contains(raw, "mock_output_not_found") {
		t.Fatalf("no-match must return mock_output_not_found: %d %s", status, raw)
	}
	status, raw = postScript(t, base, "no-match", "openai", withTools)
	if status != http.StatusOK {
		t.Fatalf("no-match with tools must match: %d %s", status, raw)
	}
}

// TestExampleProtocolErrors 锁定四个协议错误示例的状态码。
func TestExampleProtocolErrors(t *testing.T) {
	base := exampleServer(t)
	body := openAIBody("m")
	cases := []struct {
		script string
		want   int
	}{
		{"http-400", http.StatusBadRequest},
		{"http-402", http.StatusPaymentRequired},
		{"http-503", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		status, raw := postScript(t, base, tc.script, "openai", body)
		if status != tc.want {
			t.Fatalf("%s status = %d, want %d (%s)", tc.script, status, tc.want, raw)
		}
		if !strings.Contains(raw, "error") {
			t.Fatalf("%s must carry an error body: %s", tc.script, raw)
		}
	}

	// HTTP 200 逻辑失败：Responses 用 status=failed + error 表达。
	// （openai 非流式 200+error 按 C 阶段口径在 prepare 阶段被拒，属既定行为。）
	status, raw := postScript(t, base, "logical-failure-200", "responses", responsesBody())
	if status != http.StatusOK {
		t.Fatalf("logical-failure-200 (responses) status = %d (%s)", status, raw)
	}
	if !strings.Contains(raw, "failed") || !strings.Contains(raw, "logical_failure") {
		t.Fatalf("logical-failure-200 must carry failed status and error code: %s", raw)
	}
}

// TestExampleRawOutput 锁定 raw 剧本的字节与配置完全一致（不做任何协议加工）。
func TestExampleRawOutput(t *testing.T) {
	base := exampleServer(t)
	status, raw := postScript(t, base, "raw-malformed-sse", "openai",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("raw status = %d", status)
	}
	if !strings.Contains(raw, `event: message_start`) || !strings.Contains(raw, `"broken": `) {
		t.Fatalf("raw body must be sent verbatim: %q", raw)
	}

	status, raw = postScript(t, base, "raw-wrong-order", "openai",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("raw-wrong-order status = %d", status)
	}
	deltaAt := strings.Index(raw, `"delta"`)
	startAt := strings.Index(raw, `"message_start"`)
	if deltaAt < 0 || startAt < 0 || deltaAt > startAt {
		t.Fatalf("raw-wrong-order must keep the wrong order: %q", raw)
	}
}

// TestExampleUsageAndMultimodal 锁定 usage 动态回填与多模态断言。
func TestExampleUsageAndMultimodal(t *testing.T) {
	base := exampleServer(t)

	status, raw := postScript(t, base, "usage-cache", "openai",
		`{"model":"m","cached_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("usage-cache status = %d (%s)", status, raw)
	}
	var usage struct {
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatalf("decode usage: %v (%s)", err, raw)
	}
	if usage.Usage.PromptTokens != 120 || usage.Usage.CompletionTokens != 24 {
		t.Fatalf("usage static fields not applied: %s", raw)
	}
	if usage.Usage.PromptTokensDetails.CachedTokens != 64 {
		t.Fatalf("cached tokens must be back-filled from the request: %s", raw)
	}

	multimodal := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"描述这张图"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	status, raw = postScript(t, base, "multimodal", "openai", multimodal)
	if status != http.StatusOK {
		t.Fatalf("multimodal status = %d (%s)", status, raw)
	}
}

// TestModelsEndpoints 锁定内置 Models 端点（不进剧本 DSL）。
func TestModelsEndpoints(t *testing.T) {
	base := exampleServer(t)
	for _, path := range []string{"/models", "/v1/models"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d (%s)", path, resp.StatusCode, raw)
		}
		var list struct {
			Object string `json:"object"`
			Data   []struct {
				ID     string `json:"id"`
				Object string `json:"object"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("decode %s: %v (%s)", path, err, raw)
		}
		if list.Object != "list" || len(list.Data) == 0 {
			t.Fatalf("%s payload = %s", path, raw)
		}
		for _, entry := range list.Data {
			if entry.ID == "" || entry.Object != "model" {
				t.Fatalf("%s entry invalid: %s", path, raw)
			}
		}
		if !strings.Contains(string(raw), "gpt-4o") {
			t.Fatalf("%s must contain builtin models: %s", path, raw)
		}
	}
	// 非 GET 必须 405。
	resp, err := http.Post(base+"/models", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /models: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /models status = %d, want 405", resp.StatusCode)
	}
}
