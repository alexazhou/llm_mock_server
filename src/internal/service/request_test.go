package service

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/alexazhou/llm_mock_server/internal/model"
)

func newExecutor(t *testing.T, serverYAML string, scripts map[string]string) *Executor {
	t.Helper()
	configPath := writeFixture(t, serverYAML, scripts)
	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return NewExecutor(loaded)
}

func execute(executor *Executor, scriptID, protocol, body string) (int, *model.FrameworkError) {
	rec := httptest.NewRecorder()
	path := "/mock/" + scriptID + "/v1/" + map[string]string{
		"openai": "chat/completions", "anthropic": "messages", "responses": "responses",
	}[protocol]
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	_, ferr := executor.Execute(rec, request, scriptID, protocol)
	if ferr != nil {
		return ferr.Status, ferr
	}
	return http.StatusOK, nil
}

func TestNoOutputMatchedDiagnostics(t *testing.T) {
	script := `version: 1
id: never
rounds:
  - outputs:
      - when:
          - name: only_third
            key: request.count
            op: eq
            expected: 3
        content: third
`
	executor := newExecutor(t, "", map[string]string{"never.yaml": script})
	status, ferr := execute(executor, "never", "openai", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusInternalServerError || ferr.Code != model.CodeOutputNotFound {
		t.Fatalf("expected 500 mock_output_not_found, got %d %v", status, ferr)
	}
	for _, key := range []string{"request_count", "output_count", "round_index", "candidates"} {
		if _, ok := ferr.Details[key]; !ok {
			t.Fatalf("details must contain %q: %#v", key, ferr.Details)
		}
	}
	candidates, ok := ferr.Details["candidates"].([]any)
	if !ok || len(candidates) != 1 {
		t.Fatalf("expected one candidate result, got %#v", ferr.Details["candidates"])
	}
	candidate := candidates[0].(map[string]any)
	if candidate["matched"] != false || candidate["index"] != 0 {
		t.Fatalf("unexpected candidate: %#v", candidate)
	}
	conditions := candidate["conditions"].([]any)
	condition := conditions[0].(map[string]any)
	if condition["key"] != "request.count" || condition["matched"] != false {
		t.Fatalf("unexpected condition: %#v", condition)
	}
}

func TestConditionEvaluatorError(t *testing.T) {
	script := `version: 1
id: cond
rounds:
  - outputs:
      - when:
          - key: request.header
            field: X-Multi
            op: eq
            expected: a
        content: matched
      - content: fallback
`
	executor := newExecutor(t, "", map[string]string{"cond.yaml": script})
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/mock/cond/v1/chat/completions",
		bytes.NewBufferString(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Add("X-Multi", "a")
	request.Header.Add("X-Multi", "b")
	_, ferr := executor.Execute(rec, request, "cond", "openai")
	if ferr == nil || ferr.Code != model.CodeOutputConditionError {
		t.Fatalf("expected mock_output_condition_error, got %v", ferr)
	}
	if ferr.Status != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", ferr.Status)
	}
}

func TestCountConsumptionRules(t *testing.T) {
	script := `version: 1
id: counted
rounds:
  - outputs:
      - when:
          - key: request.count
            op: eq
            expected: 1
        content: first
      - content: rest
`
	executor := newExecutor(t, "", map[string]string{"counted.yaml": script})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	// 断言/解析/round 失败不计数：先制造一次 round 越界，再正常请求应拿到 count=1。
	multiRound := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"x"}]}`
	if status, ferr := execute(executor, "counted", "openai", multiRound); status != 422 || ferr.Code != model.CodeRoundNotFound {
		t.Fatalf("expected round not found, got %d %v", status, ferr)
	}
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/mock/counted/v1/chat/completions", bytes.NewBufferString(body))
	result, ferr := executor.Execute(rec, request, "counted", "openai")
	if ferr != nil {
		t.Fatalf("unexpected error: %v", ferr)
	}
	if result.RequestCount != 1 || result.OutputIndex != 0 {
		t.Fatalf("round-not-found must not consume count: count=%d output=%d", result.RequestCount, result.OutputIndex)
	}
}

func TestCapacityRejectedBeforeBodyRead(t *testing.T) {
	executor := newExecutor(t, "", map[string]string{"hello.yaml": "version: 1\nid: hello\nrounds:\n  - output:\n      content: hi\n"})
	// 占满唯一容量，模拟并发中的活动请求。
	executor.Capacity = NewCapacity(1)
	if !executor.Capacity.TryAcquire() {
		t.Fatal("first acquire must succeed")
	}
	defer executor.Capacity.Release()

	status, ferr := execute(executor, "hello", "openai", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusServiceUnavailable || ferr.Code != model.CodeCapacityExceeded {
		t.Fatalf("expected 503 mock_capacity_exceeded, got %d %v", status, ferr)
	}
	if _, hasCount := ferr.Details["request_count"]; hasCount {
		t.Fatalf("capacity rejection must not allocate request_count: %#v", ferr.Details)
	}
}

func TestConcurrentRequestsShareRoundAndGetUniqueCounts(t *testing.T) {
	script := `version: 1
id: hello
rounds:
  - output:
      content: hi
`
	executor := newExecutor(t, "version: 1\nruntime:\n  max_active_requests: 64\n", map[string]string{"hello.yaml": script})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	const workers = 20
	var wg sync.WaitGroup
	counts := make(chan uint64, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/mock/hello/v1/chat/completions", bytes.NewBufferString(body))
			result, ferr := executor.Execute(rec, request, "hello", "openai")
			if ferr != nil {
				t.Errorf("unexpected error: %v", ferr)
				return
			}
			if result.RoundIndex != 1 {
				t.Errorf("concurrent same request must stay on round 1, got %d", result.RoundIndex)
			}
			counts <- result.RequestCount
		}()
	}
	wg.Wait()
	close(counts)
	seen := make(map[uint64]bool, workers)
	for count := range counts {
		if seen[count] {
			t.Fatalf("duplicate request.count %d", count)
		}
		seen[count] = true
	}
	if len(seen) != workers {
		t.Fatalf("expected %d unique counts, got %d", workers, len(seen))
	}
}

func TestCountersResetRequiresNoActiveRequests(t *testing.T) {
	counters := NewScriptCounters()
	counters.Next("openai")
	if !counters.Reset() {
		t.Fatal("reset with no active requests must succeed")
	}
	if counts := counters.Counts(); counts["openai"] != 0 {
		t.Fatalf("reset must zero counts, got %#v", counts)
	}
	counters.Acquire()
	if counters.Reset() {
		t.Fatal("reset must fail while a request is active")
	}
	counters.Release()
}
