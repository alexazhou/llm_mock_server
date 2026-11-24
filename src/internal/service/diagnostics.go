package service

import (
	"encoding/json"
	"io"
	"sync"
)

// 本文件实现固定 JSONL 诊断输出。配置参考 §5 明确：第一版日志固定输出 JSONL，
// 不暴露只有单一取值的配置项，因此这里没有开关字段——是否输出只由调用方是否
// 注入 writer 决定（main 注入 stderr，测试默认不注入以保持输出干净）。
//
// 每条请求在 capture 记录结束时输出一行，包含请求、断言、条件、步骤、写入与
// 结束原因。诊断写入失败被忽略：诊断不能影响请求处理。

// Diagnostics 是并发安全的 JSONL 事件输出器。
type Diagnostics struct {
	out io.Writer
	mu  sync.Mutex
}

// NewDiagnostics 创建一个输出到 out 的诊断器；out 为 nil 时返回 nil（等于关闭）。
func NewDiagnostics(out io.Writer) *Diagnostics {
	if out == nil {
		return nil
	}
	return &Diagnostics{out: out}
}

// EmitRecord 输出一条请求诊断（单行 JSON）。
func (d *Diagnostics) EmitRecord(rec CaptureRecord) {
	if d == nil || d.out == nil {
		return
	}
	event := map[string]any{
		"type":          "request",
		"sequence":      rec.Sequence,
		"request_id":    rec.RequestID,
		"received_at":   rec.ReceivedAt,
		"script_id":     rec.ScriptID,
		"protocol":      rec.Protocol,
		"method":        rec.Method,
		"url":           rec.URL,
		"result":        rec.Result,
		"end_reason":    rec.EndReason,
		"written_bytes": rec.WrittenBytes,
	}
	// 「未知」不写成 0：只有真正已知的上下文才出现（与 B 阶段诊断口径一致）。
	if rec.RequestCount != nil {
		event["request_count"] = *rec.RequestCount
	}
	if rec.OutputCount != nil {
		event["output_count"] = *rec.OutputCount
	}
	if rec.RoundIndex != nil {
		event["round_index"] = *rec.RoundIndex
	}
	if rec.OutputIndex != nil {
		event["output_index"] = *rec.OutputIndex
	}
	if len(rec.Assertions) > 0 {
		event["assertions"] = rec.Assertions
	}
	if len(rec.Conditions) > 0 {
		event["conditions"] = rec.Conditions
	}
	if len(rec.Steps) > 0 {
		event["steps"] = rec.Steps
	}
	if rec.FrameworkError != nil {
		event["framework_error"] = rec.FrameworkError
	}

	line, err := json.Marshal(event)
	if err != nil {
		return
	}
	line = append(line, '\n')
	d.mu.Lock()
	_, _ = d.out.Write(line)
	d.mu.Unlock()
}
