package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件补充 D 阶段的捕获内容判据：条件结果、步骤结果、并发 header 归属。

// whenScript 有两个候选 output：第一个按 request.count 条件匹配，第二个是 fallback。
const whenScript = `version: 1
id: when
rounds:
  - outputs:
      - when:
          - key: request.count
            op: eq
            expected: 1
        content: first
        steps:
          - op: send
          - op: end
      - content: fallback
        steps:
          - op: send
          - op: end
`

// TestCaptureRecordsConditionsAndSteps 锁定 capture 里的条件求值结果与步骤结果。
func TestCaptureRecordsConditionsAndSteps(t *testing.T) {
	base := adminServer(t, map[string]string{"when.yaml": whenScript})
	resp, err := http.Post(base+"/mock/when/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	var records []struct {
		Conditions []map[string]any `json:"conditions"`
		Steps      []map[string]any `json:"steps"`
		Written    int64            `json:"written_bytes"`
	}
	if status := getJSON(t, base+"/_test/requests", &records); status != http.StatusOK {
		t.Fatalf("requests status = %d", status)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	record := records[0]
	if len(record.Conditions) == 0 {
		t.Fatal("capture must record outputs.when candidate evaluation results")
	}
	// 第一个候选命中，必须能看出 matched=true。
	first := record.Conditions[0]
	if matched, _ := first["matched"].(bool); !matched {
		t.Fatalf("first candidate should match on request.count=1: %#v", first)
	}
	if len(record.Steps) == 0 {
		t.Fatal("capture must record step execution results")
	}
	for _, step := range record.Steps {
		if step["result"] != "ok" {
			t.Fatalf("step should be ok: %#v", step)
		}
		if _, ok := step["bytes"]; !ok {
			t.Fatalf("step must carry written bytes: %#v", step)
		}
	}
	if record.Written <= 0 {
		t.Fatalf("written_bytes must be positive for a successful response, got %d", record.Written)
	}
}

// TestConcurrentCaptureKeepsHeaderOwnership 锁定并发请求下 capture 记录的 header 不串。
func TestConcurrentCaptureKeepsHeaderOwnership(t *testing.T) {
	base := adminServer(t, map[string]string{"hello.yaml": helloScript})

	const workers = 24
	var wait sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			marker := fmt.Sprintf("worker-%02d", index)
			body := `{"model":"` + marker + `","messages":[{"role":"user","content":"hi"}]}`
			request, err := http.NewRequest(http.MethodPost, base+"/mock/hello/v1/chat/completions", strings.NewReader(body))
			if err != nil {
				errs <- err
				return
			}
			request.Header.Set("X-Worker", marker)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				errs <- err
				return
			}
			response.Body.Close()
		}(i)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent request failed: %v", err)
	}

	var records []struct {
		Body         string              `json:"body"`
		Headers      map[string]string   `json:"headers"`
		HeaderValues map[string][]string `json:"header_values"`
	}
	if status := getJSON(t, base+"/_test/requests", &records); status != http.StatusOK {
		t.Fatalf("requests status = %d", status)
	}
	if len(records) != workers {
		t.Fatalf("capture window should hold %d records, got %d", workers, len(records))
	}
	for _, record := range records {
		worker := record.Headers["X-Worker"]
		if worker == "" {
			t.Fatalf("record missing X-Worker header: %#v", record.Headers)
		}
		if !strings.Contains(record.Body, `"model":"`+worker+`"`) {
			t.Fatalf("header/body ownership mixed: header=%s body=%s", worker, record.Body)
		}
		values := record.HeaderValues["X-Worker"]
		if len(values) != 1 || values[0] != worker {
			t.Fatalf("header_values mismatch: %#v", record.HeaderValues)
		}
	}
}

// TestCaptureTruncatesBodyPerConfig 锁定 capture.max_body_bytes 的截断与标记。
func TestCaptureTruncatesBodyPerConfig(t *testing.T) {
	router := newTestRouter(t, "version: 1\nscripts_dir: scripts\ncapture:\n  max_body_bytes: 16\n",
		map[string]string{"hello.yaml": helloScript})
	server := newServerFromRouter(t, router)

	resp, err := http.Post(server+"/mock/hello/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	var records []struct {
		Body          string `json:"body"`
		BodyTruncated bool   `json:"body_truncated"`
	}
	getJSON(t, server+"/_test/requests", &records)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if len(records[0].Body) != 16 || !records[0].BodyTruncated {
		t.Fatalf("body must be truncated to 16 bytes with flag: len=%d truncated=%v",
			len(records[0].Body), records[0].BodyTruncated)
	}
}

// TestCaptureRequestsIsJSONArrayEvenWhenEmpty 锁定空窗口返回 [] 而不是 null。
func TestCaptureRequestsIsJSONArrayEvenWhenEmpty(t *testing.T) {
	base := adminServer(t, map[string]string{"hello.yaml": helloScript})
	resp, err := http.Get(base + "/_test/requests")
	if err != nil {
		t.Fatalf("requests: %v", err)
	}
	defer resp.Body.Close()
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	trimmed := strings.TrimSpace(raw.String())
	if trimmed != "[]" {
		t.Fatalf("empty capture window must encode as [], got %q", trimmed)
	}
	var decoded []any
	if err := json.Unmarshal(raw.Bytes(), &decoded); err != nil {
		t.Fatalf("empty capture window must be valid JSON: %v", err)
	}
}

// TestCaptureResultForAbortAndClientCancel 覆盖 abort 与客户端取消两条退出路径：
// 两者都必须让 active_requests 归零，并在 capture 里留下可区分的结果分类。
func TestCaptureResultForAbortAndClientCancel(t *testing.T) {
	base := adminServer(t, map[string]string{
		"abort.yaml": abortScript,
		"hang.yaml":  hangScript,
	})

	// abort：meng 客端会收到连接重置，属于剧本的正常结果。
	if response, err := http.Post(base+"/mock/abort/v1/chat/completions", "application/json",
		strings.NewReader(chatBody)); err == nil {
		response.Body.Close()
	}
	waitActiveRequests(t, base, "abort", 0)
	waitResult(t, base, "abort", "aborted")

	// 客户端取消：挂起中的请求被调用方放弃。用裸 TCP 保证「已提交响应头」这一
	// 前置条件可观测，不依赖 http.Client 在只发头时的返回时机。
	conn := openHangingRequest(t, base, "hang")
	waitActiveRequests(t, base, "hang", 1)
	if err := conn.Close(); err != nil {
		t.Fatalf("close hanging connection: %v", err)
	}
	waitActiveRequests(t, base, "hang", 0)
	waitResult(t, base, "hang", "client_cancel")
}

// waitResult 轮询 capture 窗口，直到指定剧本出现期望的结果分类。
func waitResult(t *testing.T, base, scriptID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var records []struct {
			ScriptID string `json:"script_id"`
			Result   string `json:"result"`
		}
		if status := getJSON(t, base+"/_test/requests", &records); status == http.StatusOK {
			for _, record := range records {
				if record.ScriptID == scriptID && record.Result == want {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("capture never recorded result %q for script %s", want, scriptID)
}

// upstreamScript 故意返回 503（剧本的正常结果），用于区分「剧本故意失败」与
// 「Mock 自身断言失败」——verify 只能把后者报成框架错误。
const upstreamScript = `version: 1
id: upstream
rounds:
  - output:
      config:
        status: 503
      error:
        type: server_error
        message: Mock upstream unavailable
`

// TestVerifyDistinguishesScriptFailureFromFrameworkError 锁定 D 的核心区分：
// 剧本故意 503 不能让 verify 失败；自身断言失败必须让 verify 失败。
func TestVerifyDistinguishesScriptFailureFromFrameworkError(t *testing.T) {
	base := adminServer(t, map[string]string{
		"upstream.yaml": upstreamScript,
		"strict.yaml":   strictScript,
	})

	var snapshot struct {
		InstanceID string `json:"instance_id"`
		Cursor     uint64 `json:"cursor"`
	}
	getJSON(t, base+"/_test/capture/snapshot.json", &snapshot)

	// 剧本故意 503。
	resp, err := http.Post(base+"/mock/upstream/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("upstream request: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("script must return its configured 503, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	verifyBody := `{"instance_id":"` + snapshot.InstanceID + `","cursor":` + itoa(snapshot.Cursor) + `}`
	var report struct {
		OK              bool `json:"ok"`
		Records         int  `json:"records"`
		FrameworkErrors []struct {
			Code string `json:"code"`
		} `json:"framework_errors"`
	}
	if status, raw := postJSON(t, base+"/_test/capture/verify.json", verifyBody, &report); status != http.StatusOK {
		t.Fatalf("verify status = %d (%s)", status, raw)
	}
	if !report.OK || len(report.FrameworkErrors) != 0 {
		t.Fatalf("script-level 503 must not be a framework error: %#v", report)
	}
	if report.Records != 1 {
		t.Fatalf("verify must count the script request, got %d", report.Records)
	}

	// 同一区间内再制造一次自身断言失败 ⇒ verify 必须失败。
	resp, err = http.Post(base+"/mock/strict/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("strict request: %v", err)
	}
	resp.Body.Close()
	if status, raw := postJSON(t, base+"/_test/capture/verify.json", verifyBody, &report); status != http.StatusOK {
		t.Fatalf("verify status = %d (%s)", status, raw)
	}
	if report.OK || len(report.FrameworkErrors) != 1 {
		t.Fatalf("framework error must fail verify: %#v", report)
	}

	// script_ids 过滤只看 strict。
	filtered := `{"instance_id":"` + snapshot.InstanceID + `","cursor":` + itoa(snapshot.Cursor) + `,"script_ids":["upstream"]}`
	var onlyUpstream struct {
		OK      bool `json:"ok"`
		Records int  `json:"records"`
	}
	if status, raw := postJSON(t, base+"/_test/capture/verify.json", filtered, &onlyUpstream); status != http.StatusOK {
		t.Fatalf("verify(filtered) status = %d (%s)", status, raw)
	}
	if !onlyUpstream.OK || onlyUpstream.Records != 1 {
		t.Fatalf("script_ids filter must ignore other scripts: %#v", onlyUpstream)
	}
}

// TestCounterUnknownScriptIsRejected 锁定未知剧本 id 的 counter 查询与 reset。
func TestCounterUnknownScriptIsRejected(t *testing.T) {
	base := adminServer(t, map[string]string{"hello.yaml": helloScript})
	if status, raw := postJSON(t, base+"/_test/scripts/nope/counter.json", "", nil); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET-shaped POST should be 405, got %d (%s)", status, raw)
	}
	var envelope map[string]any
	if status := getJSONStatus(t, base+"/_test/scripts/nope/counter.json", &envelope); status != http.StatusBadRequest {
		t.Fatalf("unknown script counter must be 400, got %d", status)
	}
	if status, _ := postJSON(t, base+"/_test/scripts/nope/counter/reset.json", "", nil); status != http.StatusBadRequest {
		t.Fatalf("unknown script reset must be 400, got %d", status)
	}
	if status, _ := postJSON(t, base+"/_test/counters/reset.json", `{"script_ids":["nope"]}`, nil); status != http.StatusBadRequest {
		t.Fatalf("batch reset with unknown script must be 400, got %d", status)
	}
}

// getJSONStatus 与 getJSON 相同，但允许非 200 响应（便于断言错误码）。
func getJSONStatus(t *testing.T, url string, target any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if target != nil {
		if err := json.Unmarshal(body, target); err != nil {
			t.Fatalf("decode %s: %v (body=%s)", url, err, body)
		}
	}
	return resp.StatusCode
}
