package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexazhou/llm_mock_server/internal/service"
)

type envelope struct {
	Error struct {
		Code      string         `json:"code"`
		Message   string         `json:"message"`
		ScriptID  string         `json:"script_id"`
		RequestID string         `json:"request_id"`
		Protocol  string         `json:"protocol"`
		Details   map[string]any `json:"details"`
	} `json:"error"`
}

// chatCompletion 只解析探针需要的字段；响应容器本身由 adapter 单测覆盖。
type chatCompletion struct {
	Object string `json:"object"`
	Model  string `json:"model"`
	Choice []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// anthropicMessage 解析 Anthropic 非流式容器的探针字段。
type anthropicMessage struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
}

// responsesObject 解析 Responses 非流式容器的探针字段。
type responsesObject struct {
	Object string `json:"object"`
	Status string `json:"status"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

func newTestRouter(t *testing.T, serverYAML string, scripts map[string]string) *http.ServeMux {
	t.Helper()
	root := t.TempDir()
	if serverYAML == "" {
		serverYAML = "version: 1\nscripts_dir: scripts\n"
	}
	if err := os.WriteFile(filepath.Join(root, "server.yaml"), []byte(serverYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range scripts {
		if err := os.WriteFile(filepath.Join(root, "scripts", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := service.Load(filepath.Join(root, "server.yaml"))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return NewRouter(loaded)
}

func doPost(router http.Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, request)
	return rec
}

const helloScript = `version: 1
id: hello
rounds:
  - output:
      content: Hi
`

func TestChatCompletionResponseForSingleOutput(t *testing.T) {
	router := newTestRouter(t, "", map[string]string{"hello.yaml": helloScript})
	body := `{"model":"gpt-4o","messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`
	rec := doPost(router, "/mock/hello/v1/chat/completions", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got chatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "chat.completion" || got.Model != "gpt-4o" {
		t.Fatalf("unexpected container: %+v", got)
	}
	if len(got.Choice) != 1 {
		t.Fatalf("expected exactly one choice, got %+v", got.Choice)
	}
	choice := got.Choice[0]
	if choice.Message.Role != "assistant" || choice.Message.Content != "Hi" || choice.FinishReason != "stop" {
		t.Fatalf("unexpected choice: %+v", choice)
	}
}

// countedScript 用 when: request.count 把计数暴露成可观测的正文，
// 因为 C 阶段不再有决策回显。
const countedScript = `version: 1
id: counted
rounds:
  - outputs:
      - when:
          - key: request.count
            op: eq
            expected: 1
        content: first
      - when:
          - key: request.count
            op: eq
            expected: 2
        content: second
      - content: later
`

func TestCountIsIsolatedPerProtocolAndIncrementsPerAcceptedRequest(t *testing.T) {
	router := newTestRouter(t, "", map[string]string{"counted.yaml": countedScript})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"openai first", "/mock/counted/v1/chat/completions", "first"},
		{"openai second", "/mock/counted/v1/chat/completions", "second"},
		{"openai third", "/mock/counted/v1/chat/completions", "later"},
		{"anthropic first", "/mock/counted/v1/messages", "first"},
	} {
		rec := doPost(router, tc.path, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", tc.name, rec.Code, rec.Body.String())
		}
		if got := contentOf(t, tc.path, rec.Body.Bytes()); got != tc.want {
			t.Fatalf("%s: content=%q want %q", tc.name, got, tc.want)
		}
	}
}

// contentOf 按 URL 协议取出正文，供计数类用例断言。
func contentOf(t *testing.T, path string, body []byte) string {
	t.Helper()
	switch {
	case strings.HasSuffix(path, "/v1/messages"):
		var got anthropicMessage
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode anthropic body: %v", err)
		}
		for _, block := range got.Content {
			if block.Type == "text" {
				return block.Text
			}
		}
		return ""
	case strings.HasSuffix(path, "/v1/responses"):
		var got responsesObject
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode responses body: %v", err)
		}
		for _, item := range got.Output {
			for _, part := range item.Content {
				if part.Type == "output_text" {
					return part.Text
				}
			}
		}
		return ""
	default:
		var got chatCompletion
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode chat body: %v", err)
		}
		if len(got.Choice) == 0 {
			return ""
		}
		return got.Choice[0].Message.Content
	}
}

const retryScript = `version: 1
id: retry
rounds:
  - outputs:
      - when:
          - key: request.count
            op: eq
            expected: 2
        config:
          status: 503
        content: second request fails
      - content: ok
`

func TestWhenSelectsByRequestCount(t *testing.T) {
	router := newTestRouter(t, "", map[string]string{"retry.yaml": retryScript})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	first := doPost(router, "/mock/retry/v1/chat/completions", body)
	second := doPost(router, "/mock/retry/v1/chat/completions", body)

	if first.Code != http.StatusOK {
		t.Fatalf("first request must hit the fallback with 200, got %d: %s", first.Code, first.Body.String())
	}
	if got := contentOf(t, "/mock/retry/v1/chat/completions", first.Body.Bytes()); got != "ok" {
		t.Fatalf("first request must hit the fallback output, got %q", got)
	}
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second request must hit the count==2 candidate with 503, got %d", second.Code)
	}
	if got := contentOf(t, "/mock/retry/v1/chat/completions", second.Body.Bytes()); got != "second request fails" {
		t.Fatalf("second request content = %q", got)
	}
}

func TestAssertionFailureEnvelope(t *testing.T) {
	script := `version: 1
id: strict
assert:
  - name: messages_not_empty
    key: request.json_body
    path: $.messages
    op: length_gte
    expected: 1
rounds:
  - output:
      content: ok
`
	router := newTestRouter(t, "", map[string]string{"strict.yaml": script})
	rec := doPost(router, "/mock/strict/v1/chat/completions", `{"model":"m","messages":[]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	var got envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Code != "mock_assertion_failed" || got.Error.ScriptID != "strict" || got.Error.Protocol != "openai" {
		t.Fatalf("unexpected envelope: %+v", got.Error)
	}
	assertion, ok := got.Error.Details["assertion"].(map[string]any)
	if !ok {
		t.Fatalf("expected details.assertion, got %#v", got.Error.Details)
	}
	if assertion["name"] != "messages_not_empty" || assertion["op"] != "length_gte" {
		t.Fatalf("unexpected assertion details: %#v", assertion)
	}
	if _, hasCount := got.Error.Details["request_count"]; hasCount {
		t.Fatalf("assertion failure must not report request_count: %#v", got.Error.Details)
	}
	if _, hasExpected := assertion["expected"]; !hasExpected {
		t.Fatalf("assertion details must carry expected: %#v", assertion)
	}
}

func TestRoundNotFoundEnvelope(t *testing.T) {
	router := newTestRouter(t, "", map[string]string{"hello.yaml": helloScript})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"},
		{"role":"assistant","content":"hello"},
		{"role":"user","content":"again"}]}`
	rec := doPost(router, "/mock/hello/v1/chat/completions", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
	var got envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Error.Code != "mock_round_not_found" {
		t.Fatalf("unexpected code %q", got.Error.Code)
	}
	if got.Error.Details["output_count"] != float64(1) {
		t.Fatalf("expected output_count 1 in details, got %#v", got.Error.Details)
	}
	if _, hasCount := got.Error.Details["request_count"]; hasCount {
		t.Fatalf("round-not-found must not allocate request_count")
	}
}

func TestMethodAndNotFoundEnvelopes(t *testing.T) {
	router := newTestRouter(t, "", map[string]string{"hello.yaml": helloScript})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mock/hello/v1/messages", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
	var method envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &method); err != nil || method.Error.Code != "mock_method_not_allowed" {
		t.Fatalf("expected JSON 405 envelope, got %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mock/missing/v1/messages", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	var notFound envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &notFound); err != nil || notFound.Error.Code != "mock_script_not_found" {
		t.Fatalf("expected JSON 404 envelope, got %s", rec.Body.String())
	}
}

func TestBodyTooLargeAndInvalidJSON(t *testing.T) {
	script := `version: 1
id: small
config:
  max_body_bytes: 16
rounds:
  - output:
      content: ok
`
	router := newTestRouter(t, "", map[string]string{"small.yaml": script})
	rec := doPost(router, "/mock/small/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", rec.Code, rec.Body.String())
	}

	router = newTestRouter(t, "", map[string]string{"hello.yaml": helloScript})
	rec = doPost(router, "/mock/hello/v1/chat/completions", `{"model":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	var got envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Error.Code != "mock_invalid_json" {
		t.Fatalf("unexpected code %q", got.Error.Code)
	}

	rec = doPost(router, "/mock/hello/v1/chat/completions", `{"model":"m","messages":[]}{"x":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a second JSON value, got %d", rec.Code)
	}
}

func TestProtocolSelectedByURLOnly(t *testing.T) {
	router := newTestRouter(t, "", map[string]string{"hello.yaml": helloScript})

	// 同一 body 打 anthropic URL：messages 存在但 model 合法，协议校验只做最小结构检查。
	anthropicBody := `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	rec := doPost(router, "/mock/hello/v1/messages", anthropicBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got anthropicMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "message" || got.Role != "assistant" {
		t.Fatalf("URL must select the anthropic container, got %+v", got)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "text" || got.Content[0].Text != "Hi" {
		t.Fatalf("unexpected anthropic content: %+v", got.Content)
	}
	if got.StopReason != "end_turn" {
		t.Fatalf("unexpected stop_reason: %q", got.StopReason)
	}

	// openai URL 却缺 messages ⇒ 422，不回落到其它协议。
	rec = doPost(router, "/mock/hello/v1/chat/completions", `{"model":"m","max_tokens":1}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
	var bad envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &bad)
	if bad.Error.Code != "mock_protocol_invalid" {
		t.Fatalf("unexpected code %q", bad.Error.Code)
	}
}
