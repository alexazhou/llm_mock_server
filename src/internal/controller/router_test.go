package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

type decision struct {
	MockPhase    string `json:"mock_phase"`
	Placeholder  bool   `json:"placeholder"`
	ScriptID     string `json:"script_id"`
	Protocol     string `json:"protocol"`
	RequestCount uint64 `json:"request_count"`
	OutputCount  int    `json:"output_count"`
	RoundIndex   int    `json:"round_index"`
	OutputIndex  int    `json:"output_index"`
	Status       int    `json:"status"`
	Stream       bool   `json:"stream"`
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

func TestDecisionResponseForSingleOutput(t *testing.T) {
	router := newTestRouter(t, "", map[string]string{"hello.yaml": helloScript})
	body := `{"model":"gpt-4o","messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`
	rec := doPost(router, "/mock/hello/v1/chat/completions", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got decision
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := decision{MockPhase: "B", Placeholder: true, ScriptID: "hello", Protocol: "openai",
		RequestCount: 1, OutputCount: 0, RoundIndex: 1, OutputIndex: 0, Status: 200, Stream: false}
	if got != want {
		t.Fatalf("unexpected decision: %+v want %+v", got, want)
	}
}

func TestCountIsIsolatedPerProtocolAndIncrementsPerAcceptedRequest(t *testing.T) {
	router := newTestRouter(t, "", map[string]string{"hello.yaml": helloScript})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	first := doPost(router, "/mock/hello/v1/chat/completions", body)
	second := doPost(router, "/mock/hello/v1/chat/completions", body)
	anthropic := doPost(router, "/mock/hello/v1/messages", body)

	for _, tc := range []struct {
		name string
		rec  *httptest.ResponseRecorder
		want uint64
	}{
		{"openai first", first, 1},
		{"openai second", second, 2},
		{"anthropic first", anthropic, 1},
	} {
		var got decision
		if err := json.Unmarshal(tc.rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.RequestCount != tc.want {
			t.Fatalf("%s: request_count=%d want %d", tc.name, got.RequestCount, tc.want)
		}
		if got.RoundIndex != 1 {
			t.Fatalf("%s: round_index must stay 1 regardless of count, got %d", tc.name, got.RoundIndex)
		}
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

	var firstBody, secondBody decision
	_ = json.Unmarshal(first.Body.Bytes(), &firstBody)
	_ = json.Unmarshal(second.Body.Bytes(), &secondBody)
	if firstBody.OutputIndex != 1 || firstBody.Status != 200 {
		t.Fatalf("first request must hit fallback: %+v", firstBody)
	}
	if secondBody.OutputIndex != 0 || secondBody.Status != 503 {
		t.Fatalf("second request must hit the count==2 candidate: %+v", secondBody)
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
	var got decision
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Protocol != "anthropic" {
		t.Fatalf("URL must select anthropic, got %q", got.Protocol)
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
