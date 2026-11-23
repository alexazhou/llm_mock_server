package service

import (
	"fmt"
	"time"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/model"
)

// 本文件是 output prepare 阶段：在提交响应头之前把协议无关声明解析成可直接
// 编码的协议无关快照（动态值、usage、tool ID、响应 ID、时间戳），并做依赖
// 请求模式与协议的静态校验。失败一律返回 mock_output_prepare_error（500），
// 此时不提交任何响应头、不执行任何 step、但 request.count 已经消耗。

// PreparedToolCall 是一个解析完成的工具调用；Arguments 是确定性紧凑 JSON。
type PreparedToolCall struct {
	ID        string
	Name      string
	Arguments []byte
}

// PreparedError 是解析完成的协议错误。
type PreparedError struct {
	Type       string
	HasType    bool
	Code       string
	HasCode    bool
	Message    string
	HasMessage bool
	Param      string
	HasParam   bool
}

// PreparedUsage 是解析完成的 usage 四数字。
type PreparedUsage struct {
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64
	ReasoningTokens   int64
}

// PreparedOutput 是一次请求的全部输出快照。同一份快照既供非流式编码，也供
// 流式事件序列使用，保证自动 ID 与时间戳在分片之间不变。
type PreparedOutput struct {
	Protocol string
	Status   int
	// Headers 是用户配置的有效 headers（已按层原子覆盖）。
	Headers    map[string]string
	Flush      bool
	LineEnding string
	UsageMode  string
	Stream     bool

	// Empty 表示四类输出字段全部省略，响应 body 必须是 0 字节。
	Empty bool

	ResponseID string
	Created    int64
	Model      string

	Content      string
	HasContent   bool
	Reasoning    string
	HasReasoning bool
	ToolCalls    []PreparedToolCall
	Error        *PreparedError
	// Usage 为 nil 表示不输出任何 usage（usage_mode: omit）。
	Usage *PreparedUsage

	// Steps 是默认过程或显式 steps（加载期已编译）。
	Steps []CompiledStep
}

// PrepareOutput 解析一份已编译 output 的全部动态内容。
func PrepareOutput(ctx *model.RequestContext, output *CompiledOutput, maxOutputBytes int64) (*PreparedOutput, *model.FrameworkError) {
	effective := output.Effective
	prepared := &PreparedOutput{
		Protocol:   ctx.Protocol,
		Status:     effective.Status,
		Headers:    effective.Headers,
		Flush:      effective.Flush,
		LineEnding: effective.LineEnding,
		UsageMode:  effective.UsageMode,
		Stream:     resolveStreamMode(effective.Stream, ctx.Stream),
		Model:      ctx.Model,
		Steps:      output.Steps,
	}
	if prepared.Status == 0 {
		prepared.Status = constants.DefaultStatus
	}

	if output.Content != nil {
		text, err := output.Content.ResolveString("content", ctx)
		if err != nil {
			return nil, prepareFailure(err.Error(), ctx)
		}
		if text == "" {
			return nil, prepareFailure("content must resolve to a non-empty string", ctx)
		}
		prepared.Content, prepared.HasContent = text, true
	}
	if output.Reasoning != nil {
		text, err := output.Reasoning.ResolveString("reasoning", ctx)
		if err != nil {
			return nil, prepareFailure(err.Error(), ctx)
		}
		if text == "" {
			return nil, prepareFailure("reasoning must resolve to a non-empty string", ctx)
		}
		prepared.Reasoning, prepared.HasReasoning = text, true
	}
	for i := range output.ToolCalls {
		call := &output.ToolCalls[i]
		raw, err := call.Arguments.ResolveJSON(ctx)
		if err != nil {
			return nil, prepareFailure(fmt.Sprintf("tool_calls[%d].arguments: %v", i, err), ctx)
		}
		prepared.ToolCalls = append(prepared.ToolCalls, PreparedToolCall{
			ID:        call.ResolveID(ctx.Protocol),
			Name:      call.Name,
			Arguments: raw,
		})
	}
	if output.Error != nil {
		protoErr := &PreparedError{
			Type: output.Error.Type, HasType: output.Error.Type != "",
			Code: output.Error.Code, HasCode: output.Error.Code != "",
		}
		message, ok, err := output.Error.ResolveMessage(ctx)
		if err != nil {
			return nil, prepareFailure(err.Error(), ctx)
		}
		if !ok || message == "" {
			return nil, prepareFailure("error.message must resolve to a non-empty string", ctx)
		}
		protoErr.Message, protoErr.HasMessage = message, true
		param, ok, err := output.Error.ResolveParam(ctx)
		if err != nil {
			return nil, prepareFailure(err.Error(), ctx)
		}
		protoErr.Param, protoErr.HasParam = param, ok
		prepared.Error = protoErr
	}
	prepared.Empty = !prepared.HasContent && !prepared.HasReasoning &&
		len(prepared.ToolCalls) == 0 && prepared.Error == nil

	if prepared.UsageMode != "omit" {
		usage := &PreparedUsage{}
		var err error
		if usage.InputTokens, err = ResolveUsageNumber(ctx, "usage.input_tokens", effective.Usage.InputTokens); err != nil {
			return nil, prepareFailure(err.Error(), ctx)
		}
		if usage.OutputTokens, err = ResolveUsageNumber(ctx, "usage.output_tokens", effective.Usage.OutputTokens); err != nil {
			return nil, prepareFailure(err.Error(), ctx)
		}
		if usage.CachedInputTokens, err = ResolveUsageNumber(ctx, "usage.cached_input_tokens", effective.Usage.CachedInputTokens); err != nil {
			return nil, prepareFailure(err.Error(), ctx)
		}
		if usage.ReasoningTokens, err = ResolveUsageNumber(ctx, "usage.reasoning_tokens", effective.Usage.ReasoningTokens); err != nil {
			return nil, prepareFailure(err.Error(), ctx)
		}
		prepared.Usage = usage
	}

	if ferr := validateResolvedOutputBytes(prepared, maxOutputBytes); ferr != nil {
		return nil, prepareFailureOf(ferr, ctx)
	}
	if ferr := validateProtocolCapabilities(prepared); ferr != nil {
		return nil, prepareFailureOf(ferr, ctx)
	}

	prepared.ResponseID = NewResponseID(ctx.Protocol)
	prepared.Created = time.Now().Unix()
	return prepared, nil
}

// resolveStreamMode 把 auto / true / false 落成具体模式。
func resolveStreamMode(mode string, requestStream bool) bool {
	switch mode {
	case constants.StreamTrue:
		return true
	case constants.StreamFalse:
		return false
	}
	return requestStream
}

// validateProtocolCapabilities 校验依赖请求模式与协议的组合（design §8 规则 6/7/8/11）。
func validateProtocolCapabilities(prepared *PreparedOutput) *model.FrameworkError {
	// 非流式：内容 target 与 heartbeat 都无意义。
	if !prepared.Stream {
		for i := range prepared.Steps {
			step := &prepared.Steps[i]
			if step.Op != OpSend {
				continue
			}
			if step.Target != nil && !(step.Target.Block == BlockHeaders && step.Target.Milestone == MilestoneEnd) {
				return prepareError(fmt.Sprintf(
					"steps[%d].target: non-streaming output only accepts headers.end as an explicit send target", step.Index),
					nil)
			}
			if step.Source == SourceHeartbeat {
				return prepareError(fmt.Sprintf(
					"steps[%d].source: heartbeat requires streaming output", step.Index), nil)
			}
		}
	}
	// Responses 没有 stream.end 里程碑。
	if prepared.Stream && prepared.Protocol == constants.ProtocolResponses {
		for i := range prepared.Steps {
			step := &prepared.Steps[i]
			if step.Op == OpSend && step.Target != nil && step.Target.Block == BlockStream &&
				step.Target.Milestone == MilestoneEnd {
				return prepareError(fmt.Sprintf(
					"steps[%d].target: protocol responses has no stream.end milestone", step.Index), nil)
			}
		}
	}
	// 错误编码组合：status 不是 4xx/5xx 时，非流式 OpenAI/Anthropic 没有 200 内嵌 error 容器。
	if prepared.Error != nil && !isErrorStatus(prepared.Status) {
		switch prepared.Protocol {
		case constants.ProtocolResponses:
			// 允许：非流式 failed response / 流式 response.failed。
		default:
			if !prepared.Stream {
				return prepareError(fmt.Sprintf(
					"output.error with status %d is not supported for non-streaming %s output; "+
						"use a 4xx/5xx status or streaming", prepared.Status, prepared.Protocol), nil)
			}
		}
	}
	return nil
}

// isErrorStatus 判断是否为 HTTP 错误状态（4xx/5xx）。
func isErrorStatus(status int) bool { return status >= 400 && status <= 599 }

// prepareError 构造 mock_output_prepare_error（不含请求上下文字段）。
// streamingNotImplemented 等内部边界也用它。
func prepareError(message string, _ *model.RequestContext) *model.FrameworkError {
	return model.NewFrameworkError(model.CodeOutputPrepareError, message)
}

// prepareFailure / prepareFailureOf 给 already-built 的 prepare 错误补上请求上下文
// （script_id、protocol、request_count 等），保证 500 envelope 与其它错误一致。
func prepareFailure(message string, ctx *model.RequestContext) *model.FrameworkError {
	return prepareFailureOf(prepareError(message, ctx), ctx)
}

func prepareFailureOf(ferr *model.FrameworkError, ctx *model.RequestContext) *model.FrameworkError {
	if ctx == nil {
		return ferr
	}
	return withBase(ferr, ctx)
}
