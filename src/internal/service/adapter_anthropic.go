package service

// Anthropic Messages 非流式容器：thinking / text / tool_use 三类 content block，
// thinking block 固定附带确定性占位 signature（step2_technical §4）。

const anthropicThinkingSignature = "mock_signature"

type anthropicUsage struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens,omitempty"`
}

type anthropicMessage struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Model        string          `json:"model"`
	Content      []any           `json:"content"`
	StopReason   string          `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	Usage        *anthropicUsage `json:"usage,omitempty"`
}

type anthropicThinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicToolUseBlock struct {
	Type  string  `json:"type"`
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Input rawJSON `json:"input"`
}

type anthropicErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type anthropicErrorEnvelope struct {
	Type  string             `json:"type"`
	Error anthropicErrorBody `json:"error"`
}

// encodeAnthropicMessage 编码 Anthropic 非流式响应。
func encodeAnthropicMessage(prepared *PreparedOutput) ([]byte, error) {
	var blocks []any
	if prepared.HasReasoning {
		blocks = append(blocks, anthropicThinkingBlock{
			Type:      "thinking",
			Thinking:  prepared.Reasoning,
			Signature: anthropicThinkingSignature,
		})
	}
	if prepared.HasContent {
		blocks = append(blocks, anthropicTextBlock{Type: "text", Text: prepared.Content})
	}
	for _, call := range prepared.ToolCalls {
		blocks = append(blocks, anthropicToolUseBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Name,
			Input: rawJSON(call.Arguments),
		})
	}
	if blocks == nil {
		// 空输出在 prepare 阶段已标记为空 body；这里只是防御。
		blocks = []any{}
	}
	stopReason := "end_turn"
	if len(prepared.ToolCalls) > 0 {
		stopReason = "tool_use"
	}
	return encodeJSON(anthropicMessage{
		ID:           prepared.ResponseID,
		Type:         "message",
		Role:         "assistant",
		Model:        prepared.Model,
		Content:      blocks,
		StopReason:   stopReason,
		StopSequence: nil,
		Usage:        anthropicUsageFor(prepared.Usage),
	})
}

// anthropicUsageFor 映射 usage；Anthropic 没有 reasoning token 位置。
func anthropicUsageFor(usage *PreparedUsage) *anthropicUsage {
	if usage == nil {
		return nil
	}
	return &anthropicUsage{
		InputTokens:          usage.InputTokens,
		OutputTokens:         usage.OutputTokens,
		CacheReadInputTokens: usage.CachedInputTokens,
	}
}

// encodeAnthropicError 编码 Anthropic HTTP 错误体（type/param/code 中只有 type 有位置）。
func encodeAnthropicError(prepared *PreparedOutput) ([]byte, error) {
	return encodeJSON(anthropicErrorEnvelope{
		Type: "error",
		Error: anthropicErrorBody{
			Type:    errorType(prepared),
			Message: prepared.Error.Message,
		},
	})
}
