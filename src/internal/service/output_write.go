package service

import (
	"context"
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
)

// writeOp 是规划出的一步传输操作。
type writeOp struct {
	kind     opKind
	data     []byte
	duration time.Duration
	infinite bool
}

// WriteOutput 执行 prepared output 的传输。返回错误表示客户端已断开或写失败，
// 此时响应头可能已经提交，调用方不得再写框架错误 envelope。
func WriteOutput(w http.ResponseWriter, r *http.Request, prepared *PreparedOutput) error {
	writer := &responseWriter{w: w, status: prepared.Status, headers: prepared.Headers, flush: prepared.Flush}
	if flusher, ok := w.(http.Flusher); ok {
		writer.flusher = flusher
	}
	return writer.run(r.Context(), prepared)
}

// responseWriter 维护「是否已提交响应头」这一唯一状态。
type responseWriter struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	status    int
	headers   map[string]string
	flush     bool
	committed bool
}

// run 规划并执行传输操作。
func (w *responseWriter) run(ctx context.Context, prepared *PreparedOutput) error {
	if prepared.Stream {
		// prepare 阶段已经拒绝流式；这里是防御性兜底（M3 会实现流式记录）。
		return errStreamingNotImplemented
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
	for _, op := range ops {
		switch op.kind {
		case opSleep:
			if err := sleep(ctx, op); err != nil {
				return err
			}
		case opCommit:
			w.commitHeaders()
		case opWrite:
			w.commitHeaders()
			if _, err := w.w.Write(op.data); err != nil {
				return err
			}
			if w.flush && w.flusher != nil {
				w.flusher.Flush()
			}
		}
	}
	return nil
}

// headerContentLength 是框架自行计算的保留 header 名（剧本不允许声明）。
const headerContentLength = "Content-Length"

// errStreamingNotImplemented 是 M2 的临时边界：流式记录在 M3 接入。
var errStreamingNotImplemented = prepareError("streaming output is not implemented yet", nil)

// planNonStream 把 steps 展开成传输操作：send 先按 chunks 切分，再逐片写入。
func planNonStream(prepared *PreparedOutput) ([]writeOp, error) {
	ops := make([]writeOp, 0, len(prepared.Steps))
	for i := range prepared.Steps {
		step := &prepared.Steps[i]
		switch step.Op {
		case OpWait:
			ops = append(ops, writeOp{kind: opSleep, duration: time.Duration(step.Duration), infinite: step.Infinite})
		case OpEnd:
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
