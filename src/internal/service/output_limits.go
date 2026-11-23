package service

import (
	"fmt"

	"github.com/alexazhou/llm_mock_server/internal/model"
)

// 本文件实现 max_output_bytes 的两道校验：
//   - 加载期：全部为静态字面量时直接算出总字节数并拒绝超限剧本；
//   - prepare 阶段：含系统 key 动态引用时，解析完动态值后立即校验（design §8 规则 4）。
// 统计口径是「正常输出 + error + heartbeat + raw 的有限发送字节」，
// 不含协议容器与 SSE framing 的额外字节。

// staticOutputBytes 返回 output 全部静态可算部分的总字节数；static=false 表示
// 含动态引用，只能留到 prepare 阶段。
func staticOutputBytes(output *CompiledOutput) (int64, bool) {
	var total int64
	static := true
	count := func(text *model.TextValue) {
		if text == nil {
			return
		}
		if text.Ref != nil {
			static = false
			return
		}
		total += int64(len(text.Literal))
	}
	count(output.Output.Content)
	count(output.Output.Reasoning)
	if output.Output.Error != nil {
		count(output.Output.Error.Message)
		count(output.Output.Error.Param)
	}
	for i := range output.ToolCalls {
		call := &output.ToolCalls[i]
		total += int64(len(call.Name))
		if !call.Arguments.IsStatic() {
			static = false
			continue
		}
		raw, err := call.Arguments.ResolveJSON(nil)
		if err != nil {
			static = false
			continue
		}
		total += int64(len(raw))
	}
	for i := range output.Steps {
		step := &output.Steps[i]
		if step.Op != OpSend {
			continue
		}
		switch step.Source {
		case SourceRaw:
			total += int64(len(step.Raw))
		case SourceHeartbeat:
			// heartbeat 写入 count 行，每行是 ": " + text + lineEnding。
			text := step.Text
			if output.Effective != nil {
				total += int64(step.Count * (2 + len(text) + len(lineEndingBytes(output.Effective.LineEnding))))
			} else {
				static = false
			}
		}
	}
	return total, static
}

// lineEndingBytes 返回 line_ending 对应的字节序列。
func lineEndingBytes(lineEnding string) string {
	if lineEnding == "crlf" {
		return "\r\n"
	}
	return "\n"
}

// validateStaticOutputBytes 在加载期拒绝静态超限的 output。
func validateStaticOutputBytes(output *CompiledOutput, limit int64) error {
	if limit <= 0 {
		return nil
	}
	total, static := staticOutputBytes(output)
	if !static || total <= limit {
		return nil
	}
	return fmt.Errorf("output produces %d bytes, which exceeds max_output_bytes %d", total, limit)
}

// loadOutputBytesDiagnostics 便于测试与诊断（未导出时由单测直接调用静态版本）。
//
// resolvedOutputBytes 统计 prepare 之后实际要发送的正常输出字节数。
func resolvedOutputBytes(prepared *PreparedOutput) int64 {
	var total int64
	total += int64(len(prepared.Content))
	total += int64(len(prepared.Reasoning))
	for _, call := range prepared.ToolCalls {
		total += int64(len(call.Name)) + int64(len(call.Arguments))
	}
	if prepared.Error != nil {
		total += int64(len(prepared.Error.Message)) + int64(len(prepared.Error.Param))
	}
	for i := range prepared.Steps {
		step := &prepared.Steps[i]
		if step.Op != OpSend {
			continue
		}
		switch step.Source {
		case SourceRaw:
			total += int64(len(step.Raw))
		case SourceHeartbeat:
			total += int64(step.Count * (2 + len(step.Text) + len(lineEndingBytes(prepared.LineEnding))))
		}
	}
	return total
}

// validateResolvedOutputBytes 在 prepare 阶段（提交响应头之前）校验解析后的总字节数。
func validateResolvedOutputBytes(prepared *PreparedOutput, limit int64) *model.FrameworkError {
	if limit <= 0 {
		return nil
	}
	if total := resolvedOutputBytes(prepared); total > limit {
		return prepareError(fmt.Sprintf(
			"output produces %d bytes, which exceeds max_output_bytes %d", total, limit), nil)
	}
	return nil
}
