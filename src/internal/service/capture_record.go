package service

import (
	"net/http"

	"github.com/alexazhou/llm_mock_server/internal/model"
)

// 本文件把一次请求的执行过程写进 capture 窗口：请求进入流水线时入窗（只含请求面），
// body 读完后补 body/json，结束时补 result、框架错误、写入字节与结束原因。
// 所有字段写入都通过 Capture 的持锁方法完成，避免 List/Verify 读到并发写。

// requestRecorder 是一次请求的 capture 写入器；Capture 为 nil 时所有方法都是空操作。
type requestRecorder struct {
	capture *Capture
	record  *CaptureRecord
}

// newRequestRecorder 在流水线入口记录请求面并分配 sequence。
func newRequestRecorder(capture *Capture, r *http.Request, requestID, scriptID, protocol string) *requestRecorder {
	if capture == nil {
		return nil
	}
	record := &CaptureRecord{
		RequestID:    requestID,
		Method:       r.Method,
		URL:          r.URL.RequestURI(),
		Headers:      joinHeaderValues(r.Header),
		HeaderValues: cloneStringSliceMap(map[string][]string(r.Header)),
		ScriptID:     scriptID,
		Protocol:     protocol,
		Assertions:   []any{},
		Conditions:   []any{},
		Steps:        []any{},
	}
	capture.Begin(record)
	return &requestRecorder{capture: capture, record: record}
}

// setBody 记录请求 body（按 capture.max_body_bytes 截断）与解析后的 JSON。
func (rec *requestRecorder) setBody(body []byte, parsed any) {
	if rec == nil {
		return
	}
	rec.capture.SetBody(rec.record, body, parsed)
}

// setOutputCount 记录 request.output_count 与 round.index（0 表示未定位）。
func (rec *requestRecorder) setOutputCount(outputCount, roundIndex int) {
	rec.update(func(r *CaptureRecord) {
		output := outputCount
		round := roundIndex
		r.OutputCount = &output
		if roundIndex > 0 {
			r.RoundIndex = &round
		}
	})
}

// setRequestCount 记录已分配的 request.count。
func (rec *requestRecorder) setRequestCount(count uint64) {
	rec.update(func(r *CaptureRecord) {
		value := count
		r.RequestCount = &value
	})
}

// setOutputIndex 记录命中的 output 序号。
func (rec *requestRecorder) setOutputIndex(index int) {
	rec.update(func(r *CaptureRecord) {
		value := index
		r.OutputIndex = &value
	})
}

// setAssertions 追加输入断言逐条求值结果（含失败项）。
func (rec *requestRecorder) setAssertions(results []AssertionResult) {
	if len(results) == 0 {
		return
	}
	details := assertResultsDetails(results)
	rec.update(func(r *CaptureRecord) {
		r.Assertions = append(r.Assertions, details...)
	})
}

// setResult 设置结果分类（output / error / aborted / client_cancel / framework_error）。
func (rec *requestRecorder) setResult(result string) {
	rec.update(func(r *CaptureRecord) { r.Result = result })
}

// finish 补齐结果面并结束这条记录。
func (rec *requestRecorder) finish(result string, ferr *model.FrameworkError, written int64, endReason string) {
	if rec == nil {
		return
	}
	rec.capture.Finish(rec.record, result, ferr, written, endReason)
}

// update 在 capture 锁内改写记录；recorder 为 nil 时是空操作。
func (rec *requestRecorder) update(apply func(*CaptureRecord)) {
	if rec == nil {
		return
	}
	rec.capture.Update(rec.record, apply)
}

// joinHeaderValues 把多值 header 按技术设计 §8 的规则拼成 ", "。
func joinHeaderValues(header http.Header) map[string]string {
	out := make(map[string]string, len(header))
	for name, values := range header {
		joined := ""
		for i, value := range values {
			if i > 0 {
				joined += ", "
			}
			joined += value
		}
		out[name] = joined
	}
	return out
}
