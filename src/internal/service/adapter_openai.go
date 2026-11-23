package service

import "encoding/json"

// OpenAI Chat Completions 非流式容器。
// reasoning 走 reasoning_content（与 reasoning 字段的协议映射一致），
// tool_calls 的 arguments 是紧凑 JSON 字符串。

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

type openAIErrorEnvelope struct {
	Error openAIError `json:"error"`
}

type openAIFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIMessage struct {
	Role             string           `json:"role"`
	Content          *string          `json:"content"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAICachedTokens struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type openAIReasoningTokens struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type openAIUsage struct {
	PromptTokens            int64                  `json:"prompt_tokens"`
	CompletionTokens        int64                  `json:"completion_tokens"`
	TotalTokens             int64                  `json:"total_tokens"`
	PromptTokensDetails     *openAICachedTokens    `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *openAIReasoningTokens `json:"completion_tokens_details,omitempty"`
}

type openAIChoice struct {
	Index        int           `json:"index"`
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
	Logprobs     any           `json:"logprobs"`
}

type openAIChatCompletion struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

// encodeOpenAIChatCompletion 编码 OpenAI Chat 非流式响应。
func encodeOpenAIChatCompletion(prepared *PreparedOutput) ([]byte, error) {
	message := openAIMessage{Role: "assistant", ReasoningContent: prepared.Reasoning}
	if prepared.HasContent {
		content := prepared.Content
		message.Content = &content
	}
	for _, call := range prepared.ToolCalls {
		message.ToolCalls = append(message.ToolCalls, openAIToolCall{
			ID:   call.ID,
			Type: "function",
			Function: openAIFunction{
				Name:      call.Name,
				Arguments: string(call.Arguments),
			},
		})
	}
	finishReason := "stop"
	if len(prepared.ToolCalls) > 0 {
		finishReason = "tool_calls"
	}
	return encodeJSON(openAIChatCompletion{
		ID:      prepared.ResponseID,
		Object:  "chat.completion",
		Created: prepared.Created,
		Model:   prepared.Model,
		Choices: []openAIChoice{{
			Index:        0,
			Message:      message,
			FinishReason: finishReason,
			Logprobs:     nil,
		}},
		Usage: openAIUsageFor(prepared.Usage),
	})
}

// openAIUsageFor 把统一 usage 映射成 OpenAI 字段；nil 表示不输出 usage。
func openAIUsageFor(usage *PreparedUsage) *openAIUsage {
	if usage == nil {
		return nil
	}
	mapped := &openAIUsage{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.InputTokens + usage.OutputTokens,
	}
	if usage.CachedInputTokens > 0 {
		mapped.PromptTokensDetails = &openAICachedTokens{CachedTokens: usage.CachedInputTokens}
	}
	if usage.ReasoningTokens > 0 {
		mapped.CompletionTokensDetails = &openAIReasoningTokens{ReasoningTokens: usage.ReasoningTokens}
	}
	return mapped
}

// encodeOpenAIError 编码 OpenAI 风格 HTTP 错误体。
func encodeOpenAIError(prepared *PreparedOutput) ([]byte, error) {
	body := openAIError{Message: prepared.Error.Message, Type: errorType(prepared)}
	if prepared.Error.HasParam {
		body.Param = prepared.Error.Param
	}
	if prepared.Error.HasCode {
		body.Code = prepared.Error.Code
	}
	return encodeJSON(openAIErrorEnvelope{Error: body})
}

// rawJSON 用于把已序列化的 JSON 嵌入容器而不二次转义。
type rawJSON = json.RawMessage
