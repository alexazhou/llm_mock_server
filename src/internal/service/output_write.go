package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"
)

// 本文件把 PreparedOutput 写成真实的 HTTP 响应：先静态规划出「提交响应头 /
// 等待 / 写入字节」三类操作，再顺序执行。非流式响应在提交响应头时一次性算出
// Content-Length，避免 Go 把整份 JSON 变成 chunked 帧；流式协议记录（SSE）
// 由 adapter 的流式编码提供，将在 M3 接入同一个执行器。

// opKind 是传输操作的三种形态。
type opKind int

const (
	opCommit opKind = iota
	opSleep
	opWrite
	// opAbort 用 Hijack + SetLinger(0) 强制断连（TCP RST）。
	opAbort
)

// writeOp 是规划出的一步传输操作。
type writeOp struct {
	kind     opKind
	data     []byte
	duration time.Duration
	infinite bool
}

// WriteOutcome 是一次响应的传输结果：实际写入的字节数、是否按 end.mode=abort
// 主动断连，以及写失败原因（客户端断开或写错误）。
type WriteOutcome struct {
	Written int64
	Aborted bool
	Err     error
	// Steps 是 steps 执行诊断（流式按 step，非流式按展开后的传输操作）。
	Steps []any
}

// errAborted 是 end.mode=abort 的内部哨兵：断连是剧本的正常结果，不是写失败。
var errAborted = errors.New("connection aborted by script")

// WriteOutput 执行 prepared output 的传输，返回实际写入字节与断连/失败信息。
// 返回后调用方不得再写响应；响应头可能已提交。
func WriteOutput(w http.ResponseWriter, r *http.Request, prepared *PreparedOutput) WriteOutcome {
	writer := &responseWriter{w: w, status: prepared.Status, headers: prepared.Headers, flush: prepared.Flush}
	if flusher, ok := w.(http.Flusher); ok {
		writer.flusher = flusher
	}
	err := writer.run(r.Context(), prepared)
	outcome := WriteOutcome{Written: writer.written, Steps: writer.steps}
	if errors.Is(err, errAborted) {
		outcome.Aborted = true
		return outcome
	}
	outcome.Err = err
	return outcome
}

// responseWriter 维护「是否已提交响应头」这一唯一状态，并统计实际写入字节与步骤诊断。
type responseWriter struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	status    int
	headers   map[string]string
	flush     bool
	committed bool
	written   int64
	steps     []any
}

// run 规划并执行传输操作。
func (w *responseWriter) run(ctx context.Context, prepared *PreparedOutput) error {
	if prepared.Stream {
		return w.runStream(ctx, prepared)
	}
	ops, err := planNonStream(prepared)
	if err != nil {
		return err
	}
	// 非流式响应的完整字节序列在提交响应头之前已知，因此显式给出
	// Content-Length，保证整份 JSON 不是 chunked 帧。
	if !IsBodylessStatus(prepared.Status) {
		total := 0
		for _, op := range ops {
			if op.kind == opWrite {
				total += len(op.data)
			}
		}
		w.headers = cloneHeaders(w.headers)
		w.headers[headerContentLength] = strconv.Itoa(total)
	}
	for index, op := range ops {
		start := time.Now()
		before := w.written
		err := w.runOp(ctx, op)
		w.recordOp(index, op, start, before, err)
		if err != nil {
			return err
		}
	}
	return nil
}

// runOp 执行一个非流式传输操作。
func (w *responseWriter) runOp(ctx context.Context, op writeOp) error {
	switch op.kind {
	case opSleep:
		return sleep(ctx, op)
	case opAbort:
		return w.abortConnection()
	case opCommit:
		w.commitHeaders()
	case opWrite:
		w.commitHeaders()
		written, err := w.w.Write(op.data)
		w.written += int64(written)
		if err != nil {
			return err
		}
		if w.flush && w.flusher != nil {
			w.flusher.Flush()
		}
	}
	return nil
}

// recordOp 把非流式传输操作的执行结果追加到步骤诊断。
func (w *responseWriter) recordOp(index int, op writeOp, start time.Time, before int64, err error) {
	entry := map[string]any{
		"index":       index,
		"bytes":       w.written - before,
		"duration_ms": time.Since(start).Milliseconds(),
	}
	switch op.kind {
	case opSleep:
		entry["op"] = "wait"
		if op.infinite {
			entry["infinite"] = true
		}
	case opAbort:
		entry["op"] = "abort"
	case opCommit:
		entry["op"] = "commit"
	case opWrite:
		entry["op"] = "write"
	}
	switch {
	case errors.Is(err, errAborted):
		entry["result"] = "aborted"
	case err != nil:
		entry["result"] = "error"
		entry["error"] = err.Error()
	default:
		entry["result"] = "ok"
	}
	w.steps = append(w.steps, entry)
}

// headerContentLength 是框架自行计算的保留 header 名（剧本不允许声明）。
const headerContentLength = "Content-Length"

// ContentTypeSSE 是流式响应的默认 Content-Type；显式 headers 可以覆盖。
const ContentTypeSSE = "text/event-stream"

// runStream 按 steps 顺序生成并写出协议记录：游标只前移，raw / heartbeat 不推进游标。
// 每个 step 的字节数与耗时记入 capture 的步骤结果（steps）。
func (w *responseWriter) runStream(ctx context.Context, prepared *PreparedOutput) error {
	writer := newStreamWriter(prepared)
	geometry := geometryOf(prepared)
	for i := range prepared.Steps {
		step := &prepared.Steps[i]
		start := time.Now()
		before := w.written
		err := w.runStreamStep(ctx, prepared, writer, geometry, step)
		w.recordStep(step, start, before, err)
		if err != nil {
			return err
		}
	}
	w.commitStreamHeaders()
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return nil
}

// runStreamStep 执行单个 step。返回 errAborted 表示剧本主动断连（正常结果）。
func (w *responseWriter) runStreamStep(ctx context.Context, prepared *PreparedOutput, writer *streamWriter, geometry streamGeometry, step *CompiledStep) error {
	switch step.Op {
	case OpWait:
		return sleep(ctx, writeOp{kind: opSleep, duration: time.Duration(step.Duration), infinite: step.Infinite})
	case OpEnd:
		if step.Mode == EndModeAbort {
			return w.abortConnection()
		}
		w.commitStreamHeaders()
		if w.flusher != nil {
			w.flusher.Flush()
		}
		return nil
	case OpSend:
		if step.Target != nil && step.Target.Block == BlockHeaders {
			w.commitStreamHeaders()
			if w.flusher != nil {
				w.flusher.Flush()
			}
			return nil
		}
		switch step.Source {
		case SourceRaw:
			return w.sendStreamUnits(ctx, step, [][]byte{step.Raw})
		case SourceHeartbeat:
			payload := heartbeatBytes(step, lineEndingBytes(prepared.LineEnding))
			return w.sendStreamUnits(ctx, step, [][]byte{payload})
		case SourceError:
			return w.sendStreamUnits(ctx, step, writer.streamErrorRecords())
		default:
			bound := streamPos{stage: stageStreamEnd}
			if step.Target != nil {
				position, err := targetPosition(prepared.Protocol, geometry, step.Target)
				if err != nil {
					return err
				}
				bound = position
			}
			records, err := writer.advanceTo(bound)
			if err != nil {
				return err
			}
			return w.sendStreamUnits(ctx, step, records)
		}
	}
	return nil
}

// recordStep 把单个 step 的执行结果追加到步骤诊断。
func (w *responseWriter) recordStep(step *CompiledStep, start time.Time, before int64, err error) {
	entry := map[string]any{
		"index":       step.Index,
		"op":          step.Op,
		"bytes":       w.written - before,
		"duration_ms": time.Since(start).Milliseconds(),
	}
	if step.Source != "" {
		entry["source"] = step.Source
	}
	if step.Target != nil {
		entry["target"] = step.Target.Raw
	}
	switch {
	case errors.Is(err, errAborted):
		entry["result"] = "aborted"
	case err != nil:
		entry["result"] = "error"
		entry["error"] = err.Error()
	default:
		entry["result"] = "ok"
	}
	w.steps = append(w.steps, entry)
}

// sendStreamUnits 把一次 send 的完整字节序列拆成最终发送单元：
// 未配置 chunks 时单元是单条协议记录；配置后是字节片（sizes 只从序列开头应用一次）。
// 每个单元前按 send.interval 等待（含第一项），写入后按 config.flush 决定是否立即刷新。
func (w *responseWriter) sendStreamUnits(ctx context.Context, step *CompiledStep, records [][]byte) error {
	var units [][]byte
	if step.Chunks != nil {
		units = splitChunks(step.Chunks, joinRecords(records))
	} else {
		for _, record := range records {
			if len(record) > 0 {
				units = append(units, record)
			}
		}
	}
	for _, unit := range units {
		if step.Interval > 0 {
			if err := sleep(ctx, writeOp{kind: opSleep, duration: time.Duration(step.Interval)}); err != nil {
				return err
			}
		}
		if err := w.writeStreamBytes(unit); err != nil {
			return err
		}
		// design §4.5：chunks 定义的分片逐片 Flush，与 config.flush 无关；
		// config.flush 只控制未分片时每条记录的刷新行为。
		if step.Chunks != nil && w.flusher != nil {
			w.flusher.Flush()
		}
	}
	return nil
}

// joinRecords 拼接一次 send 的全部记录字节。
func joinRecords(records [][]byte) []byte {
	total := 0
	for _, record := range records {
		total += len(record)
	}
	out := make([]byte, 0, total)
	for _, record := range records {
		out = append(out, record...)
	}
	return out
}

// writeStreamBytes 写一条记录并按 config.flush 决定是否立即刷新。
func (w *responseWriter) writeStreamBytes(record []byte) error {
	if len(record) == 0 {
		return nil
	}
	w.commitStreamHeaders()
	written, err := w.w.Write(record)
	w.written += int64(written)
	if err != nil {
		return err
	}
	if w.flush && w.flusher != nil {
		w.flusher.Flush()
	}
	return nil
}

// commitStreamHeaders 提交流式响应头：默认 Content-Type 是 text/event-stream。
func (w *responseWriter) commitStreamHeaders() {
	if w.committed {
		return
	}
	w.committed = true
	header := w.w.Header()
	for name, value := range w.headers {
		header.Set(name, value)
	}
	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", ContentTypeSSE)
	}
	// 流式响应块间可能间隔很久，禁用反向代理缓冲。
	if header.Get("X-Accel-Buffering") == "" {
		header.Set("X-Accel-Buffering", "no")
	}
	w.w.WriteHeader(w.status)
}

// planNonStream 把 steps 展开成传输操作：send 先按 chunks 切分，再逐片写入。
func planNonStream(prepared *PreparedOutput) ([]writeOp, error) {
	ops := make([]writeOp, 0, len(prepared.Steps))
	for i := range prepared.Steps {
		step := &prepared.Steps[i]
		switch step.Op {
		case OpWait:
			ops = append(ops, writeOp{kind: opSleep, duration: time.Duration(step.Duration), infinite: step.Infinite})
		case OpEnd:
			if step.Mode == EndModeAbort {
				ops = append(ops, writeOp{kind: opAbort})
				continue
			}
			ops = append(ops, writeOp{kind: opCommit})
		case OpSend:
			if step.Target != nil && step.Target.Block == BlockHeaders {
				ops = append(ops, writeOp{kind: opCommit})
				continue
			}
			body, err := payloadFor(prepared, step)
			if err != nil {
				return nil, err
			}
			for _, unit := range splitChunks(step.Chunks, body) {
				if step.Interval > 0 {
					ops = append(ops, writeOp{kind: opSleep, duration: time.Duration(step.Interval)})
				}
				ops = append(ops, writeOp{kind: opWrite, data: unit})
			}
		}
	}
	return ops, nil
}

// payloadFor 返回一次 send 的完整字节序列。
func payloadFor(prepared *PreparedOutput, step *CompiledStep) ([]byte, error) {
	switch step.Source {
	case SourceRaw:
		return step.Raw, nil
	case SourceError:
		return encodeError(prepared)
	default:
		return encodeNonStream(prepared)
	}
}

// splitChunks 按 size / sizes 把字节序列切成最终发送单元；sizes 只从序列开头应用一次。
func splitChunks(chunks *CompiledChunks, body []byte) [][]byte {
	if chunks == nil || len(body) == 0 {
		return [][]byte{body}
	}
	var sizes []int
	if chunks.HasSize {
		for offset := 0; offset < len(body); offset += chunks.Size {
			end := offset + chunks.Size
			if end > len(body) {
				end = len(body)
			}
			sizes = append(sizes, end-offset)
		}
	} else {
		offset := 0
		for _, size := range chunks.Sizes {
			if offset >= len(body) {
				break
			}
			end := offset + size
			if end > len(body) {
				end = len(body)
			}
			sizes = append(sizes, end-offset)
			offset = end
		}
		if offset < len(body) {
			sizes = append(sizes, len(body)-offset)
		}
	}
	units := make([][]byte, 0, len(sizes))
	offset := 0
	for _, size := range sizes {
		units = append(units, body[offset:offset+size])
		offset += size
	}
	return units
}

// commitHeaders 提交一次响应头：用户 headers 覆盖默认 Content-Type。
func (w *responseWriter) commitHeaders() {
	if w.committed {
		return
	}
	w.committed = true
	header := w.w.Header()
	for name, value := range w.headers {
		header.Set(name, value)
	}
	if header.Get("Content-Type") == "" {
		header.Set("Content-Type", ContentTypeJSON)
	}
	w.w.WriteHeader(w.status)
}

// abortConnection 用 HTTP/1.1 Hijack 接管连接，然后用 SetLinger(0) + Close
// 让内核发出 TCP RST（而不是优雅 FIN）。响应头未提交时客户端收不到任何字节。
func (w *responseWriter) abortConnection() error {
	hijacker, ok := w.w.(http.Hijacker)
	if !ok {
		return errors.New("connection does not support hijacking")
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.SetLinger(0); err != nil {
			_ = conn.Close()
			return err
		}
	}
	_ = conn.Close()
	return errAborted
}

// sleep 执行一次等待；无限等待持续到客户端取消或服务关闭。
func sleep(ctx context.Context, op writeOp) error {
	if op.infinite {
		<-ctx.Done()
		return ctx.Err()
	}
	if op.duration <= 0 {
		return nil
	}
	timer := time.NewTimer(op.duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
