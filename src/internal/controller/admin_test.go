package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 本文件覆盖 D 阶段的管理接口与 capture 语义。全部走真实 HTTP（httptest.NewServer），
// 因为 active_requests 的租约边界只有在真实连接上才能观测：挂起中的请求必须先让
// Execute 返回、再进入 WriteOutput 的无限 wait，租约若在 Execute 返回时释放就会漏判。
// 挂起剧本复用 socket_test.go 里的 hangScript（send headers.end → wait infinite）。

// adminServer 启动一个真实 HTTP 服务，返回 base URL。
func adminServer(t *testing.T, scripts map[string]string) string {
	t.Helper()
	return newServerFromRouter(t, newTestRouter(t, "", scripts))
}

// newServerFromRouter 用给定 router 启动真实 HTTP 服务，返回 base URL。
func newServerFromRouter(t *testing.T, router http.Handler) string {
	t.Helper()
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server.URL
}

// getJSON 发 GET 并解码响应体。
func getJSON(t *testing.T, url string, target any) int {
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
	if target != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, target); err != nil {
			t.Fatalf("decode %s: %v (body=%s)", url, err, body)
		}
	}
	return resp.StatusCode
}

// postJSON 发 POST（可为空 body）并解码响应体。
func postJSON(t *testing.T, url, body string, target any) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if target != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, target); err != nil {
			t.Fatalf("decode %s: %v (body=%s)", url, err, raw)
		}
	}
	return resp.StatusCode, string(raw)
}

// chatBody 是三协议通用的最小请求体（openai 路由用）。
const chatBody = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

// TestHealthAndScriptsEndpoints 锁定 health.json 与 _test/scripts.json 的字段。
func TestHealthAndScriptsEndpoints(t *testing.T) {
	base := adminServer(t, map[string]string{"hello.yaml": helloScript})

	var health map[string]any
	if status := getJSON(t, base+"/health.json", &health); status != http.StatusOK {
		t.Fatalf("health.json status = %d", status)
	}
	if health["status"] != "ready" {
		t.Fatalf("health status = %v", health["status"])
	}
	if id, _ := health["instance_id"].(string); !strings.HasPrefix(id, "ins_") {
		t.Fatalf("health instance_id = %v", health["instance_id"])
	}
	if count, _ := health["scripts"].(float64); int(count) != 1 {
		t.Fatalf("health scripts = %v", health["scripts"])
	}

	var scripts []struct {
		ID   string `json:"id"`
		URLs struct {
			OpenAI    string `json:"openai"`
			Anthropic string `json:"anthropic"`
			Responses string `json:"responses"`
		} `json:"urls"`
	}
	if status := getJSON(t, base+"/_test/scripts.json", &scripts); status != http.StatusOK {
		t.Fatalf("scripts.json status = %d", status)
	}
	if len(scripts) != 1 || scripts[0].ID != "hello" {
		t.Fatalf("scripts.json = %#v", scripts)
	}
	if scripts[0].URLs.OpenAI != "/mock/hello/v1/chat/completions" ||
		scripts[0].URLs.Anthropic != "/mock/hello/v1/messages" ||
		scripts[0].URLs.Responses != "/mock/hello/v1/responses" {
		t.Fatalf("script urls = %#v", scripts[0].URLs)
	}
}

// TestCaptureRecordsRequestAndClearKeepsCount 锁定 capture 记录字段与
// 「clear 只清窗口、不改变 request.count」。
func TestCaptureRecordsRequestAndClearKeepsCount(t *testing.T) {
	base := adminServer(t, map[string]string{"hello.yaml": helloScript})

	resp, err := http.Post(base+"/mock/hello/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("script request: %v", err)
	}
	resp.Body.Close()

	var records []map[string]any
	if status := getJSON(t, base+"/_test/requests", &records); status != http.StatusOK {
		t.Fatalf("requests status = %d", status)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 capture record, got %d", len(records))
	}
	record := records[0]
	if record["script_id"] != "hello" || record["protocol"] != "openai" {
		t.Fatalf("capture script/protocol = %v/%v", record["script_id"], record["protocol"])
	}
	if record["request_count"] != float64(1) {
		t.Fatalf("capture request_count = %v", record["request_count"])
	}
	if record["result"] != "output" || record["end_reason"] != "completed" {
		t.Fatalf("capture result/end_reason = %v/%v", record["result"], record["end_reason"])
	}
	if !strings.Contains(record["body"].(string), `"model":"m"`) {
		t.Fatalf("capture body = %v", record["body"])
	}
	if _, ok := record["header_values"].(map[string]any); !ok {
		t.Fatalf("capture header_values missing: %#v", record["header_values"])
	}

	// clear 只清窗口。
	var cleared map[string]any
	if status, raw := postJSON(t, base+"/_test/capture/clear.json", "", &cleared); status != http.StatusOK {
		t.Fatalf("clear status = %d (%s)", status, raw)
	}
	if cleared["cleared"] != float64(1) {
		t.Fatalf("cleared = %v", cleared["cleared"])
	}
	var after []map[string]any
	getJSON(t, base+"/_test/requests", &after)
	if len(after) != 0 {
		t.Fatalf("window should be empty after clear, got %d", len(after))
	}

	// clear 不影响 request.count：下一次请求仍然是 2。
	resp, err = http.Post(base+"/mock/hello/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("second script request: %v", err)
	}
	resp.Body.Close()
	var counter struct {
		Counts         map[string]uint64 `json:"counts"`
		ActiveRequests int64             `json:"active_requests"`
	}
	if status := getJSON(t, base+"/_test/scripts/hello/counter.json", &counter); status != http.StatusOK {
		t.Fatalf("counter status = %d", status)
	}
	if counter.Counts["openai"] != 2 {
		t.Fatalf("clear must not reset request.count, openai = %d", counter.Counts["openai"])
	}
	if counter.Counts["anthropic"] != 0 || counter.Counts["responses"] != 0 {
		t.Fatalf("protocol counters must stay isolated: %#v", counter.Counts)
	}
}

// TestCounterResetSemantics 锁定 reset 归零三个协议、reset 后下一次 count 为 1。
func TestCounterResetSemantics(t *testing.T) {
	base := adminServer(t, map[string]string{"hello.yaml": helloScript})
	for i := 0; i < 2; i++ {
		resp, err := http.Post(base+"/mock/hello/v1/chat/completions", "application/json", strings.NewReader(chatBody))
		if err != nil {
			t.Fatalf("script request: %v", err)
		}
		resp.Body.Close()
	}

	var view struct {
		Counts         map[string]uint64 `json:"counts"`
		ActiveRequests int64             `json:"active_requests"`
	}
	if status, raw := postJSON(t, base+"/_test/scripts/hello/counter/reset.json", "", &view); status != http.StatusOK {
		t.Fatalf("reset status = %d (%s)", status, raw)
	}
	for _, protocol := range []string{"openai", "anthropic", "responses"} {
		if view.Counts[protocol] != 0 {
			t.Fatalf("reset must zero all three counters, %s = %d", protocol, view.Counts[protocol])
		}
	}
	if view.ActiveRequests != 0 {
		t.Fatalf("active_requests after reset = %d", view.ActiveRequests)
	}

	// 下一次通过输入校验的请求 count 必须是 1。
	resp, err := http.Post(base+"/mock/hello/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("script request after reset: %v", err)
	}
	resp.Body.Close()
	var records []map[string]any
	getJSON(t, base+"/_test/requests", &records)
	last := records[len(records)-1]
	if last["request_count"] != float64(1) {
		t.Fatalf("first request after reset must get count 1, got %v", last["request_count"])
	}
}

// TestResetRejectedWhileRequestIsActive 锁定两件事：
// 1) active_requests 覆盖到响应写出阶段（挂起中的请求仍被计入）；
// 2) 有活动请求时 reset 返回 409 且不部分重置。
func TestResetRejectedWhileRequestIsActive(t *testing.T) {
	base := adminServer(t, map[string]string{"hang.yaml": hangScript})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost,
			base+"/mock/hang/v1/chat/completions", strings.NewReader(chatBody))
		if err != nil {
			return
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			response.Body.Close()
		}
	}()

	waitActiveRequests(t, base, "hang", 1)

	status, raw := postJSON(t, base+"/_test/scripts/hang/counter/reset.json", "", nil)
	if status != http.StatusConflict {
		t.Fatalf("reset during active request must be 409, got %d (%s)", status, raw)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode 409 body: %v (%s)", err, raw)
	}
	if envelope.Error.Code != "mock_counter_reset_conflict" {
		t.Fatalf("409 code = %s", envelope.Error.Code)
	}

	// 批量 reset 同样整体拒绝。
	if status, _ := postJSON(t, base+"/_test/counters/reset.json", `{"script_ids":["hang"]}`, nil); status != http.StatusConflict {
		t.Fatalf("batch reset during active request must be 409, got %d", status)
	}

	cancel()
	<-done
	waitActiveRequests(t, base, "hang", 0)

	// 挂起结束、租约释放后 reset 才能成功。
	if status, raw := postJSON(t, base+"/_test/scripts/hang/counter/reset.json", "", nil); status != http.StatusOK {
		t.Fatalf("reset after request end must succeed, got %d (%s)", status, raw)
	}
}

// waitActiveRequests 轮询 counter.json，等到 active_requests 达到期望值。
func waitActiveRequests(t *testing.T, base, scriptID string, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var view struct {
			ActiveRequests int64 `json:"active_requests"`
		}
		if status := getJSON(t, base+"/_test/scripts/"+scriptID+"/counter.json", &view); status == http.StatusOK {
			if view.ActiveRequests == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("active_requests for %s never reached %d", scriptID, want)
}

// TestCaptureVerifyReportsFrameworkErrors 锁定 snapshot → verify 的区间校验：
// 断言失败记入框架错误并让 verify 失败，allow_framework_errors 时豁免。
func TestCaptureVerifyReportsFrameworkErrors(t *testing.T) {
	base := adminServer(t, map[string]string{"strict.yaml": strictScript})

	var snapshot struct {
		InstanceID string `json:"instance_id"`
		Cursor     uint64 `json:"cursor"`
	}
	if status := getJSON(t, base+"/_test/capture/snapshot.json", &snapshot); status != http.StatusOK {
		t.Fatalf("snapshot status = %d", status)
	}
	if snapshot.InstanceID == "" {
		t.Fatal("snapshot must carry instance_id")
	}

	// 断言失败（request.model 必须是别的值）⇒ 框架错误。
	resp, err := http.Post(base+"/mock/strict/v1/chat/completions", "application/json", strings.NewReader(chatBody))
	if err != nil {
		t.Fatalf("strict request: %v", err)
	}
	resp.Body.Close()

	verifyBody := `{"instance_id":"` + snapshot.InstanceID + `","cursor":` + itoa(snapshot.Cursor) + `}`
	var report struct {
		OK              bool `json:"ok"`
		Records         int  `json:"records"`
		FrameworkErrors []struct {
			ScriptID string `json:"script_id"`
			Code     string `json:"code"`
		} `json:"framework_errors"`
	}
	if status, raw := postJSON(t, base+"/_test/capture/verify.json", verifyBody, &report); status != http.StatusOK {
		t.Fatalf("verify status = %d (%s)", status, raw)
	}
	if report.OK || len(report.FrameworkErrors) != 1 {
		t.Fatalf("verify must report the assertion framework error: %#v", report)
	}
	if report.FrameworkErrors[0].Code != "mock_assertion_failed" {
		t.Fatalf("framework error code = %s", report.FrameworkErrors[0].Code)
	}

	// allow_framework_errors 豁免负向用例。
	verifyAllow := `{"instance_id":"` + snapshot.InstanceID + `","cursor":` + itoa(snapshot.Cursor) + `,"allow_framework_errors":true}`
	var allowed struct {
		OK bool `json:"ok"`
	}
	if status, raw := postJSON(t, base+"/_test/capture/verify.json", verifyAllow, &allowed); status != http.StatusOK {
		t.Fatalf("verify(allow) status = %d (%s)", status, raw)
	}
	if !allowed.OK {
		t.Fatalf("allow_framework_errors must exempt the failure")
	}

	// 别的 instance_id 必须被拒绝。
	verifyForeign := `{"instance_id":"ins_other","cursor":0}`
	if status, raw := postJSON(t, base+"/_test/capture/verify.json", verifyForeign, nil); status != http.StatusBadRequest {
		t.Fatalf("foreign instance must be 400, got %d (%s)", status, raw)
	}
}

// strictScript 的顶层断言必然失败，用于制造框架错误。
const strictScript = `version: 1
id: strict
assert:
  - key: request.model
    op: eq
    expected: never-matches
rounds:
  - output:
      content: hi
`

// itoa 把 uint64 转成十进制字符串（避免额外 import）。
func itoa(value uint64) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	pos := len(digits)
	for value > 0 {
		pos--
		digits[pos] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[pos:])
}
