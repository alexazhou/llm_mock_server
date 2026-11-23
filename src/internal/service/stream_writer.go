package service

import (
	"unicode/utf8"

	"github.com/alexazhou/llm_mock_server/internal/constants"
)

// 本文件是流式输出的记录生成器：把 prepare 后的快照按协议展开成有序的 SSE
// 记录序列，并按 send.target 的逻辑位置逐步推进游标。
//
// 三条不变式：
//  1. 记录的生成顺序与 stream_position.go 的 stage 顺序严格一致；
//  2. 每 5 个 Unicode code point 一条 delta，且每条 send 从自己的起点重新计 5，
//     并在 target 边界截断（跨 send 不锁条数）；
//  3. 自动 ID 与时间戳来自 prepare 快照，分片之间不重新生成。

// deltaChunkSize 是流式 delta 固定切分的 code point 数（design §7）。
const deltaChunkSize = 5

// streamWriter 维护流式输出的游标与各内容块的推进状态。
type streamWriter struct {
	prepared *PreparedOutput
	geometry streamGeometry
	ending   string

	preambleSent bool

	reasoningStarted bool
	reasoningClosed  bool
	reasoningOffset  int

	contentStarted bool
	contentClosed  bool
	contentOffset  int

	toolIndex    int
	toolNameSent bool
	toolArgPos   []int
	toolClosed   []bool

	generationDone bool
	streamDone     bool

	// 各内容块的协议下标（Anthropic content_block index / Responses output_index）。
	reasoningIndex int
	contentIndex   int
	toolIndexes    []int
	itemIDs        []string
	reasoningID    string
	contentID      string
}

// newStreamWriter 构造流式记录生成器。
func newStreamWriter(prepared *PreparedOutput) *streamWriter {
	writer := &streamWriter{prepared: prepared, geometry: geometryOf(prepared), ending: lineEndingBytes(prepared.LineEnding)}
	index := 0
	if prepared.HasReasoning {
		writer.reasoningIndex = index
		writer.reasoningID = newItemID("rs_")
		index++
	}
	if prepared.HasContent {
		writer.contentIndex = index
		writer.contentID = newItemID("msg_")
		index++
	}
	for range prepared.ToolCalls {
		writer.toolIndexes = append(writer.toolIndexes, index)
		writer.itemIDs = append(writer.itemIDs, newItemID("fc_"))
		writer.toolArgPos = append(writer.toolArgPos, 0)
		writer.toolClosed = append(writer.toolClosed, false)
		index++
	}
	return writer
}

// advanceTo 生成逻辑位置不超过 bound 的全部记录。
func (w *streamWriter) advanceTo(bound streamPos) ([][]byte, error) {
	var records [][]byte
	for {
		produced, ok, err := w.next(bound)
		if err != nil {
			return nil, err
		}
		if !ok {
			return records, nil
		}
		records = append(records, produced...)
	}
}

// next 生成下一条记录（可能为空字节，表示仅推进游标）。
func (w *streamWriter) next(bound streamPos) ([][]byte, bool, error) {
	p := w.prepared
	if !w.preambleSent {
		position := streamPos{stage: stagePayloadStart}
		if position.after(bound) {
			return nil, false, nil
		}
		w.preambleSent = true
		return [][]byte{w.preamble()}, true, nil
	}

	if p.HasReasoning {
		produced, ok, err := w.advanceBlock(bound, &w.reasoningStarted, &w.reasoningClosed, &w.reasoningOffset,
			w.geometry.reasoningLen, streamPos{stage: stageReasoning},
			func(text string) []byte { return w.reasoningDelta(text) },
			func() [][]byte { return w.reasoningClose() })
		if err != nil || ok {
			return produced, ok, err
		}
	}
	if p.HasContent {
		produced, ok, err := w.advanceBlock(bound, &w.contentStarted, &w.contentClosed, &w.contentOffset,
			w.geometry.contentLen, streamPos{stage: stageContent},
			func(text string) []byte { return w.contentDelta(text) },
			func() [][]byte { return w.blockStop(w.contentIndex) })
		if err != nil || ok {
			return produced, ok, err
		}
	}
	for w.toolIndex < len(p.ToolCalls) {
		index := w.toolIndex
		if !w.toolNameSent {
			position := streamPos{stage: toolNameStage(index)}
			if position.after(bound) {
				return nil, false, nil
			}
			w.toolNameSent = true
			return [][]byte{w.toolStart(index)}, true, nil
		}
		limit := w.geometry.toolArgs[index]
		if bound.stage == toolArgsStage(index) && int(bound.offset) < limit {
			limit = int(bound.offset)
		}
		if w.toolArgPos[index] < limit {
			start := w.toolArgPos[index]
			end := start + deltaChunkSize
			if end > limit {
				end = limit
			}
			// arguments 是 JSON 字节流，切分不能停在多字节 UTF-8 字符中间。
			end = utf8SafeCut(w.prepared.ToolCalls[index].Arguments, start, end)
			w.toolArgPos[index] = end
			return [][]byte{w.toolArgumentsDelta(index, start, end)}, true, nil
		}
		if !w.toolClosed[index] {
			position := streamPos{stage: toolArgsStage(index), offset: int64(w.geometry.toolArgs[index] + 1)}
			if position.after(bound) {
				return nil, false, nil
			}
			w.toolClosed[index] = true
			return w.toolClose(index), true, nil
		}
		w.toolIndex++
		w.toolNameSent = false
	}

	// payload.end 没有自己的记录：内容块都关闭后即可认为已到达该位置，
	// 因此这里只检查是否还能前进到 generation.end。
	if !w.generationDone {
		position := streamPos{stage: stageGenerationEnd}
		if position.after(bound) {
			return nil, false, nil
		}
		w.generationDone = true
		return w.generation(), true, nil
	}
	if !w.streamDone {
		position := streamPos{stage: stageStreamEnd}
		if position.after(bound) {
			return nil, false, nil
		}
		w.streamDone = true
		return w.streamEnd(), true, nil
	}
	return nil, false, nil
}

// advanceBlock 推进一个文本内容块：block start → delta（≤5 code point）→ block close。
func (w *streamWriter) advanceBlock(bound streamPos, started, closed *bool, offset *int, length int,
	base streamPos, delta func(string) []byte, closeRecords func() [][]byte,
) ([][]byte, bool, error) {
	if !*started {
		if base.after(bound) {
			return nil, false, nil
		}
		*started = true
		return [][]byte{w.blockStart(base.stage)}, true, nil
	}
	limit := length
	if bound.stage == base.stage && int(bound.offset) < limit {
		limit = int(bound.offset)
	}
	if *offset < limit {
		start := *offset
		end := start + deltaChunkSize
		if end > limit {
			end = limit
		}
		*offset = end
		return [][]byte{delta(runeSlice(w.blockText(base.stage), start, end))}, true, nil
	}
	if !*closed {
		position := streamPos{stage: base.stage, offset: int64(length + 1)}
		if position.after(bound) {
			return nil, false, nil
		}
		*closed = true
		return closeRecords(), true, nil
	}
	return nil, false, nil
}

// blockText 返回文本块的原文。
func (w *streamWriter) blockText(stage int64) string {
	if stage == stageReasoning {
		return w.prepared.Reasoning
	}
	return w.prepared.Content
}

// runeSlice 按 Unicode code point 取 [start, end)。
func runeSlice(text string, start, end int) string {
	runes := []rune(text)
	if start >= len(runes) {
		return ""
	}
	if end > len(runes) {
		end = len(runes)
	}
	return string(runes[start:end])
}

// ---------------------------------------------------------------------------
// SSE framing

// dataRecord 生成一条只有 data 的记录（OpenAI）。
func (w *streamWriter) dataRecord(payload any) []byte {
	raw, _ := encodeJSON(payload)
	out := make([]byte, 0, len(raw)+16)
	out = append(out, "data: "...)
	out = append(out, raw...)
	out = append(out, w.ending...)
	out = append(out, w.ending...)
	return out
}

// eventRecord 生成带 event 名的记录（Anthropic / Responses）。
func (w *streamWriter) eventRecord(event string, payload any) []byte {
	raw, _ := encodeJSON(payload)
	out := make([]byte, 0, len(raw)+len(event)+24)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, w.ending...)
	out = append(out, "data: "...)
	out = append(out, raw...)
	out = append(out, w.ending...)
	out = append(out, w.ending...)
	return out
}

// ---------------------------------------------------------------------------
// OpenAI Chat 流式记录

func (w *streamWriter) openAIChunk(delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id":      w.prepared.ResponseID,
		"object":  "chat.completion.chunk",
		"created": w.prepared.Created,
		"model":   w.prepared.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
}

func (w *streamWriter) preamble() []byte {
	switch w.prepared.Protocol {
	case constants.ProtocolAnthropic:
		message := map[string]any{
			"id":            w.prepared.ResponseID,
			"type":          "message",
			"role":          "assistant",
			"model":         w.prepared.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
		}
		if w.prepared.Usage != nil {
			message["usage"] = map[string]any{
				"input_tokens":  w.prepared.Usage.InputTokens,
				"output_tokens": 0,
			}
		}
		return w.eventRecord("message_start", map[string]any{"type": "message_start", "message": message})
	case constants.ProtocolResponses:
		return w.eventRecord("response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id":         w.prepared.ResponseID,
				"object":     "response",
				"created_at": w.prepared.Created,
				"status":     "in_progress",
				"model":      w.prepared.Model,
				"output":     []any{},
				"usage":      nil,
			},
		})
	default:
		return w.dataRecord(w.openAIChunk(map[string]any{"role": "assistant"}, nil))
	}
}

// blockStart 生成文本块的起始记录。
func (w *streamWriter) blockStart(stage int64) []byte {
	if stage == stageReasoning {
		return w.reasoningStart()
	}
	return w.contentStart()
}

func (w *streamWriter) reasoningStart() []byte {
	switch w.prepared.Protocol {
	case constants.ProtocolAnthropic:
		return w.eventRecord("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": w.reasoningIndex,
			"content_block": map[string]any{
				"type": "thinking", "thinking": "",
			},
		})
	case constants.ProtocolResponses:
		return w.eventRecord("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": w.reasoningIndex,
			"item": map[string]any{
				"id": w.reasoningID, "type": "reasoning", "summary": []any{},
			},
		})
	default:
		// OpenAI 没有独立的 thinking 块起始记录，reasoning 走 reasoning_content delta。
		return nil
	}
}

func (w *streamWriter) contentStart() []byte {
	switch w.prepared.Protocol {
	case constants.ProtocolAnthropic:
		return w.eventRecord("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": w.contentIndex,
			"content_block": map[string]any{
				"type": "text", "text": "",
			},
		})
	case constants.ProtocolResponses:
		return w.eventRecord("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": w.contentIndex,
			"item": map[string]any{
				"id": w.contentID, "type": "message", "role": "assistant",
				"status": "in_progress", "content": []any{},
			},
		})
	default:
		return nil
	}
}

func (w *streamWriter) reasoningDelta(text string) []byte {
	switch w.prepared.Protocol {
	case constants.ProtocolAnthropic:
		return w.eventRecord("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": w.reasoningIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": text},
		})
	case constants.ProtocolResponses:
		return w.eventRecord("response.reasoning_summary_text.delta", map[string]any{
			"type":          "response.reasoning_summary_text.delta",
			"item_id":       w.reasoningID,
			"output_index":  w.reasoningIndex,
			"summary_index": 0,
			"delta":         text,
		})
	default:
		return w.dataRecord(w.openAIChunk(map[string]any{"reasoning_content": text}, nil))
	}
}

func (w *streamWriter) contentDelta(text string) []byte {
	switch w.prepared.Protocol {
	case constants.ProtocolAnthropic:
		return w.eventRecord("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": w.contentIndex,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	case constants.ProtocolResponses:
		return w.eventRecord("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       w.contentID,
			"output_index":  w.contentIndex,
			"content_index": 0,
			"delta":         text,
		})
	default:
		return w.dataRecord(w.openAIChunk(map[string]any{"content": text}, nil))
	}
}

// reasoningClose 生成 reasoning 块的收尾记录（Anthropic 先发确定性 signature）。
func (w *streamWriter) reasoningClose() [][]byte {
	switch w.prepared.Protocol {
	case constants.ProtocolAnthropic:
		return [][]byte{
			w.eventRecord("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": w.reasoningIndex,
				"delta": map[string]any{"type": "signature_delta", "signature": anthropicThinkingSignature},
			}),
			w.eventRecord("content_block_stop", map[string]any{
				"type": "content_block_stop", "index": w.reasoningIndex,
			}),
		}
	case constants.ProtocolResponses:
		return [][]byte{
			w.eventRecord("response.reasoning_summary_text.done", map[string]any{
				"type": "response.reasoning_summary_text.done", "item_id": w.reasoningID,
				"output_index": w.reasoningIndex, "summary_index": 0, "text": w.prepared.Reasoning,
			}),
			w.eventRecord("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": w.reasoningIndex,
				"item": map[string]any{
					"id": w.reasoningID, "type": "reasoning",
					"summary": []any{map[string]any{"type": "summary_text", "text": w.prepared.Reasoning}},
				},
			}),
		}
	default:
		return nil
	}
}

// blockStop 生成文本块结束记录。
func (w *streamWriter) blockStop(index int) [][]byte {
	switch w.prepared.Protocol {
	case constants.ProtocolAnthropic:
		return [][]byte{w.eventRecord("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": index,
		})}
	case constants.ProtocolResponses:
		contentID := w.contentID
		return [][]byte{
			w.eventRecord("response.output_text.done", map[string]any{
				"type": "response.output_text.done", "item_id": contentID,
				"output_index": w.contentIndex, "content_index": 0, "text": w.prepared.Content,
			}),
			w.eventRecord("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": w.contentIndex,
				"item": map[string]any{
					"id": contentID, "type": "message", "role": "assistant", "status": "completed",
					"content": []any{map[string]any{
						"type": "output_text", "text": w.prepared.Content, "annotations": []any{},
					}},
				},
			}),
		}
	default:
		return nil
	}
}

func (w *streamWriter) toolStart(index int) []byte {
	call := w.prepared.ToolCalls[index]
	switch w.prepared.Protocol {
	case constants.ProtocolAnthropic:
		return w.eventRecord("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": w.toolIndexes[index],
			"content_block": map[string]any{
				"type": "tool_use", "id": call.ID, "name": call.Name, "input": map[string]any{},
			},
		})
	case constants.ProtocolResponses:
		return w.eventRecord("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": w.toolIndexes[index],
			"item": map[string]any{
				"id": w.itemIDs[index], "type": "function_call", "call_id": call.ID,
				"name": call.Name, "arguments": "", "status": "in_progress",
			},
		})
	default:
		return w.dataRecord(w.openAIChunk(map[string]any{
			"tool_calls": []any{map[string]any{
				"index": index, "id": call.ID, "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": ""},
			}},
		}, nil))
	}
}

func (w *streamWriter) toolArgumentsDelta(index, start, end int) []byte {
	call := w.prepared.ToolCalls[index]
	chunk := string(call.Arguments[start:end])
	switch w.prepared.Protocol {
	case constants.ProtocolAnthropic:
		return w.eventRecord("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": w.toolIndexes[index],
			"delta": map[string]any{"type": "input_json_delta", "partial_json": chunk},
		})
	case constants.ProtocolResponses:
		return w.eventRecord("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      w.itemIDs[index],
			"output_index": w.toolIndexes[index],
			"delta":        chunk,
		})
	default:
		return w.dataRecord(w.openAIChunk(map[string]any{
			"tool_calls": []any{map[string]any{
				"index":    index,
				"function": map[string]any{"arguments": chunk},
			}},
		}, nil))
	}
}

func (w *streamWriter) toolClose(index int) [][]byte {
	call := w.prepared.ToolCalls[index]
	switch w.prepared.Protocol {
	case constants.ProtocolAnthropic:
		return [][]byte{w.eventRecord("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": w.toolIndexes[index],
		})}
	case constants.ProtocolResponses:
		return [][]byte{
			w.eventRecord("response.function_call_arguments.done", map[string]any{
				"type": "response.function_call_arguments.done", "item_id": w.itemIDs[index],
				"output_index": w.toolIndexes[index], "arguments": string(call.Arguments),
			}),
			w.eventRecord("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": w.toolIndexes[index],
				"item": map[string]any{
					"id": w.itemIDs[index], "type": "function_call", "call_id": call.ID,
					"name": call.Name, "arguments": string(call.Arguments), "status": "completed",
				},
			}),
		}
	default:
		return nil
	}
}

// generation 生成 generation.end 阶段的记录（finish/done + 可选 usage）。
func (w *streamWriter) generation() [][]byte {
	stopReason := "stop"
	anthropicReason := "end_turn"
	if len(w.prepared.ToolCalls) > 0 {
		stopReason, anthropicReason = "tool_calls", "tool_use"
	}
	switch w.prepared.Protocol {
	case constants.ProtocolAnthropic:
		delta := map[string]any{"stop_reason": anthropicReason, "stop_sequence": nil}
		payload := map[string]any{"type": "message_delta", "delta": delta}
		if w.prepared.Usage != nil {
			payload["usage"] = map[string]any{"output_tokens": w.prepared.Usage.OutputTokens}
		}
		return [][]byte{w.eventRecord("message_delta", payload)}
	case constants.ProtocolResponses:
		return [][]byte{w.eventRecord("response.completed", map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":         w.prepared.ResponseID,
				"object":     "response",
				"created_at": w.prepared.Created,
				"status":     "completed",
				"model":      w.prepared.Model,
				"usage":      responsesUsageFor(w.prepared.Usage),
			},
		})}
	default:
		records := [][]byte{w.dataRecord(w.openAIChunk(map[string]any{}, stopReason))}
		if w.prepared.Usage != nil && w.includeUsage() {
			records = append(records, w.dataRecord(map[string]any{
				"id":      w.prepared.ResponseID,
				"object":  "chat.completion.chunk",
				"created": w.prepared.Created,
				"model":   w.prepared.Model,
				"choices": []any{},
				"usage":   openAIUsageFor(w.prepared.Usage),
			}))
		}
		return records
	}
}

// includeUsage 判断 OpenAI 流式是否发送独立 usage chunk：
// usage_mode: emit 强制发送；auto 只在请求 stream_options.include_usage 为 true 时发送。
func (w *streamWriter) includeUsage() bool {
	if w.prepared.UsageMode == "emit" {
		return true
	}
	return w.prepared.IncludeUsage
}

// streamEnd 生成 stream.end 记录；Responses 没有该里程碑，返回空。
func (w *streamWriter) streamEnd() [][]byte {
	switch w.prepared.Protocol {
	case constants.ProtocolAnthropic:
		return [][]byte{w.eventRecord("message_stop", map[string]any{"type": "message_stop"})}
	case constants.ProtocolResponses:
		return nil
	default:
		return [][]byte{[]byte("data: [DONE]" + w.ending + w.ending)}
	}
}

// utf8SafeCut 把字节切点回退到 UTF-8 字符起始，避免把多字节字符切成两半。
func utf8SafeCut(text []byte, start, end int) int {
	if end >= len(text) {
		return len(text)
	}
	for end > start && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	if end <= start {
		// 单个字符就超过 delta 长度时至少前进一个字符。
		_, size := utf8.DecodeRune(text[start:])
		if start+size <= len(text) {
			return start + size
		}
		return len(text)
	}
	return end
}

// heartbeatBytes 生成 heartbeat 的 SSE comment 行：": " + text + lineEnding，重复 count 次。
func heartbeatBytes(step *CompiledStep, ending string) []byte {
	count := step.Count
	if count <= 0 {
		count = 1
	}
	line := ": " + step.Text + ending
	out := make([]byte, 0, len(line)*count)
	for i := 0; i < count; i++ {
		out = append(out, line...)
	}
	return out
}

// streamErrorRecords 生成流式协议错误记录（source=error 或 prepare 之后的错误编码）。
func (w *streamWriter) streamErrorRecords() [][]byte {
	message := ""
	if w.prepared.Error != nil {
		message = w.prepared.Error.Message
	}
	switch w.prepared.Protocol {
	case constants.ProtocolAnthropic:
		return [][]byte{w.eventRecord("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": errorType(w.prepared), "message": message},
		})}
	case constants.ProtocolResponses:
		return [][]byte{w.eventRecord("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": w.prepared.ResponseID, "object": "response", "status": "failed",
				"model": w.prepared.Model, "error": map[string]any{"message": message},
			},
		})}
	default:
		return [][]byte{w.dataRecord(map[string]any{
			"error": map[string]any{"message": message, "type": errorType(w.prepared)},
		})}
	}
}
