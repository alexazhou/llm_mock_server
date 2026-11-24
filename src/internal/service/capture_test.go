package service

import (
	"strings"
	"testing"

	"github.com/alexazhou/llm_mock_server/internal/model"
)

// newTestCapture 创建一个小窗口，便于触达淘汰与预算边界。
func newTestCapture(capacity, maxBody, maxTotal int64) *Capture {
	return NewCapture("ins_test", capacity, maxBody, maxTotal)
}

func beginRecord(c *Capture, requestID, scriptID string) *CaptureRecord {
	rec := &CaptureRecord{
		RequestID: requestID,
		Method:    "POST",
		URL:       "/mock/" + scriptID + "/v1/chat/completions",
		Headers:   map[string]string{"Content-Type": "application/json"},
		HeaderValues: map[string][]string{
			"Content-Type": {"application/json"},
		},
		ScriptID:   scriptID,
		Protocol:   "openai",
		Assertions: []any{},
		Conditions: []any{},
		Steps:      []any{},
	}
	c.Begin(rec)
	return rec
}

// TestCaptureClearKeepsSequenceAndCounters 锁定「clear 只清窗口」：sequence 不回退。
func TestCaptureClearKeepsSequenceAndCounters(t *testing.T) {
	capture := newTestCapture(4, 1024, 1<<20)
	beginRecord(capture, "req_1", "hi")
	beginRecord(capture, "req_2", "hi")
	cursor := capture.Cursor()
	if cursor != 2 {
		t.Fatalf("cursor should be 2, got %d", cursor)
	}
	n := capture.Clear()
	if n != 2 {
		t.Fatalf("cleared should be 2, got %d", n)
	}
	if got := len(capture.List()); got != 0 {
		t.Fatalf("window should be empty after clear, got %d", got)
	}
	if got := capture.Cursor(); got != cursor {
		t.Fatalf("clear must not rewind sequence: want %d, got %d", cursor, got)
	}
	// clear 之后的记录必须继续用递增 sequence。
	rec := beginRecord(capture, "req_3", "hi")
	if rec.Sequence != cursor+1 {
		t.Fatalf("sequence must keep increasing after clear: want %d, got %d", cursor+1, rec.Sequence)
	}
}

// TestCaptureEvictsOldestOnCapacity 锁定 capacity 淘汰与 fail closed 的边界。
func TestCaptureEvictsOldestOnCapacity(t *testing.T) {
	capture := newTestCapture(2, 1024, 1<<20)
	first := beginRecord(capture, "req_1", "hi")
	beginRecord(capture, "req_2", "hi")
	beginRecord(capture, "req_3", "hi")
	records := capture.List()
	if len(records) != 2 {
		t.Fatalf("window should keep 2 records, got %d", len(records))
	}
	if records[0].RequestID != "req_2" || records[1].RequestID != "req_3" {
		t.Fatalf("oldest record must be evicted first, got %s,%s", records[0].RequestID, records[1].RequestID)
	}
	// cursor 落在被淘汰记录之前 ⇒ fail closed。sequence 1 已被淘汰，
	// cursor=0 表示「一条都没看到」，其后的记录不完整。
	if _, ferr := capture.Verify("ins_test", 0, nil, false); ferr == nil ||
		ferr.Code != model.CodeCaptureWindowLost {
		t.Fatalf("verify on evicted cursor must fail closed, got %v", ferr)
	}
	// cursor 等于最后淘汰的 sequence ⇒ 其后记录完整。
	if _, ferr := capture.Verify("ins_test", first.Sequence, nil, false); ferr != nil {
		t.Fatalf("verify after evicted cursor must pass, got %v", ferr)
	}
}

// TestCaptureBodyTruncation 锁定 max_body_bytes=0 不保存 body、超限截断并打标记。
func TestCaptureBodyTruncation(t *testing.T) {
	capture := newTestCapture(4, 8, 1<<20)
	rec := beginRecord(capture, "req_1", "hi")
	capture.SetBody(rec, []byte("0123456789abcdef"), nil)
	records := capture.List()
	if records[0].Body != "01234567" || !records[0].BodyTruncated {
		t.Fatalf("body must be truncated to 8 bytes with flag, got %q truncated=%v",
			records[0].Body, records[0].BodyTruncated)
	}

	noBody := newTestCapture(4, 0, 1<<20)
	rec2 := beginRecord(noBody, "req_2", "hi")
	noBody.SetBody(rec2, []byte("anything"), nil)
	if got := noBody.List()[0].Body; got != "" {
		t.Fatalf("max_body_bytes=0 must not save body, got %q", got)
	}
	if noBody.List()[0].BodyTruncated {
		t.Fatal("max_body_bytes=0 must not mark body_truncated")
	}
}

// TestCaptureRecordTooLargeReplacesRecord 锁定单条超预算时降级为小型框架错误记录。
func TestCaptureRecordTooLargeReplacesRecord(t *testing.T) {
	capture := newTestCapture(4, 4096, 200)
	rec := beginRecord(capture, "req_big", "hi")
	rec.Headers = map[string]string{"X-Pad": strings.Repeat("p", 4096)}
	capture.Finish(rec, "output", nil, 0, "completed")

	records := capture.List()
	if len(records) != 1 {
		t.Fatalf("window should keep the degraded record, got %d", len(records))
	}
	if records[0].FrameworkError == nil || records[0].FrameworkError.Code != model.CodeCaptureRecordTooLarge {
		t.Fatalf("oversized record must degrade to capture_record_too_large, got %#v", records[0].FrameworkError)
	}
	if records[0].Body != "" || len(records[0].Headers) != 0 {
		t.Fatalf("degraded record must drop body and headers: %#v", records[0])
	}
}

// TestCaptureVerifyFiltersAndAllowsFrameworkErrors 锁定 verify 的过滤与豁免语义。
func TestCaptureVerifyFiltersAndAllowsFrameworkErrors(t *testing.T) {
	capture := newTestCapture(8, 1024, 1<<20)
	beginRecord(capture, "req_1", "a")
	failed := beginRecord(capture, "req_2", "b")
	capture.Finish(failed, "framework_error",
		model.NewFrameworkError(model.CodeAssertionFailed, "input assertion failed"), 0, "assertion_failed")
	cursor := capture.Cursor()

	report, ferr := capture.Verify("ins_test", 0, []string{"a"}, false)
	if ferr != nil {
		t.Fatalf("unexpected error: %v", ferr)
	}
	if !report.OK || len(report.FrameworkErrors) != 0 {
		t.Fatalf("script_ids filter must hide other scripts' errors, got %#v", report)
	}
	if report.NextCursor != cursor {
		t.Fatalf("next_cursor should be %d, got %d", cursor, report.NextCursor)
	}

	report, ferr = capture.Verify("ins_test", 0, nil, false)
	if ferr != nil {
		t.Fatalf("unexpected error: %v", ferr)
	}
	if report.OK || len(report.FrameworkErrors) != 1 {
		t.Fatalf("framework error must fail verify, got %#v", report)
	}
	if report.FrameworkErrors[0].ScriptID != "b" || report.FrameworkErrors[0].Code != model.CodeAssertionFailed {
		t.Fatalf("unexpected error entry: %#v", report.FrameworkErrors[0])
	}

	report, ferr = capture.Verify("ins_test", 0, nil, true)
	if ferr != nil {
		t.Fatalf("unexpected error: %v", ferr)
	}
	if !report.OK {
		t.Fatalf("allow_framework_errors must exempt framework errors, got %#v", report)
	}
}

// TestCaptureVerifyRejectsForeignInstance 锁定 cursor 只在同一 instance_id 内有效。
func TestCaptureVerifyRejectsForeignInstance(t *testing.T) {
	capture := newTestCapture(4, 1024, 1<<20)
	_, ferr := capture.Verify("ins_other", 0, nil, false)
	if ferr == nil || ferr.Code != model.CodeCaptureInstanceMismatch {
		t.Fatalf("expected mock_capture_instance_mismatch, got %v", ferr)
	}
	// 空 instance_id 视为「不校验」，便于本地调试。
	if _, ferr := capture.Verify("", 0, nil, false); ferr != nil {
		t.Fatalf("empty instance_id must skip the check, got %v", ferr)
	}
}

// TestCaptureTotalBytesEvictsOldest 锁定 max_total_bytes 淘汰最旧记录。
func TestCaptureTotalBytesEvictsOldest(t *testing.T) {
	capture := newTestCapture(10, 1024, 300)
	for i := 0; i < 5; i++ {
		rec := beginRecord(capture, "req_"+string(rune('a'+i)), "hi")
		capture.SetBody(rec, []byte(strings.Repeat("x", 80)), nil)
	}
	records := capture.List()
	if len(records) >= 5 {
		t.Fatalf("max_total_bytes must evict records, got %d", len(records))
	}
	if records[0].Sequence == 1 {
		t.Fatalf("oldest record should have been evicted, got sequence %d", records[0].Sequence)
	}
}
