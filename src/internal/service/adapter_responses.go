package service

// Responses 非流式容器：output 由 reasoning item、message item 和 function_call
// item 组成；reasoning item 使用最小合法结构（step2_technical §4）。

type responsesCachedTokens struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type responsesReasoningTokens struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type responsesUsage struct {
	InputTokens         int64                     `json:"input_tokens"`
	OutputTokens        int64                     `json:"output_tokens"`
	TotalTokens         int64                     `json:"total_tokens"`
	InputTokensDetails  *responsesCachedTokens    `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *responsesReasoningTokens `json:"output_tokens_details,omitempty"`
}

type responsesObject struct {
	ID        string          `json:"id"`
	Object    string          `json:"object"`
	CreatedAt int64           `json:"created_at"`
	Status    string          `json:"status"`
	Model     string          `json:"model"`
	Output    []any           `json:"output"`
	Usage     *responsesUsage `json:"usage,omitempty"`
}

type responsesErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

type responsesErrorEnvelope struct {
	Error responsesErrorBody `json:"error"`
}

type responsesFailedObject struct {
	ID        string             `json:"id"`
	Object    string             `json:"object"`
	CreatedAt int64              `json:"created_at"`
	Status    string             `json:"status"`
	Model     string             `json:"model"`
	Output    []any              `json:"output"`
	Error     responsesErrorBody `json:"error"`
	Usage     *responsesUsage    `json:"usage,omitempty"`
}

type responsesSummaryText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesReasoningItem struct {
	ID      string                 `json:"id"`
	Type    string                 `json:"type"`
	Summary []responsesSummaryText `json:"summary"`
}

type responsesOutputText struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type responsesMessageItem struct {
	ID      string                `json:"id"`
	Type    string                `json:"type"`
	Role    string                `json:"role"`
	Status  string                `json:"status"`
	Content []responsesOutputText `json:"content"`
}

type responsesFunctionCallItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

// encodeResponsesObject 编码 Responses 非流式响应。
func encodeResponsesObject(prepared *PreparedOutput) ([]byte, error) {
	items := make([]any, 0, 2+len(prepared.ToolCalls))
	if prepared.HasReasoning {
		items = append(items, responsesReasoningItem{
			ID:      newItemID("rs_"),
			Type:    "reasoning",
			Summary: []responsesSummaryText{{Type: "summary_text", Text: prepared.Reasoning}},
		})
	}
	if prepared.HasContent {
		items = append(items, responsesMessageItem{
			ID:     newItemID("msg_"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []responsesOutputText{{
				Type: "output_text", Text: prepared.Content, Annotations: []any{},
			}},
		})
	}
	for _, call := range prepared.ToolCalls {
		items = append(items, responsesFunctionCallItem{
			ID:        newItemID("fc_"),
			Type:      "function_call",
			CallID:    call.ID,
			Name:      call.Name,
			Arguments: string(call.Arguments),
			Status:    "completed",
		})
	}
	return encodeJSON(responsesObject{
		ID:        prepared.ResponseID,
		Object:    "response",
		CreatedAt: prepared.Created,
		Status:    "completed",
		Model:     prepared.Model,
		Output:    items,
		Usage:     responsesUsageFor(prepared.Usage),
	})
}

// responsesUsageFor 映射 usage；显式 nil 表示不输出（Responses 用 null 表达）。
func responsesUsageFor(usage *PreparedUsage) *responsesUsage {
	if usage == nil {
		return nil
	}
	mapped := &responsesUsage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.InputTokens + usage.OutputTokens,
	}
	if usage.CachedInputTokens > 0 {
		mapped.InputTokensDetails = &responsesCachedTokens{CachedTokens: usage.CachedInputTokens}
	}
	if usage.ReasoningTokens > 0 {
		mapped.OutputTokensDetails = &responsesReasoningTokens{ReasoningTokens: usage.ReasoningTokens}
	}
	return mapped
}

// encodeResponsesHTTPError 编码 4xx/5xx 的 Responses 错误体（与 OpenAI 同形）。
func encodeResponsesHTTPError(prepared *PreparedOutput) ([]byte, error) {
	body := responsesErrorBody{Message: prepared.Error.Message, Type: errorType(prepared)}
	if prepared.Error.HasParam {
		body.Param = prepared.Error.Param
	}
	if prepared.Error.HasCode {
		body.Code = prepared.Error.Code
	}
	return encodeJSON(responsesErrorEnvelope{Error: body})
}

// encodeResponsesFailed 编码 2xx 内的 status=failed response。
func encodeResponsesFailed(prepared *PreparedOutput) ([]byte, error) {
	body := responsesErrorBody{Message: prepared.Error.Message, Type: errorType(prepared)}
	if prepared.Error.HasCode {
		body.Code = prepared.Error.Code
	}
	return encodeJSON(responsesFailedObject{
		ID:        prepared.ResponseID,
		Object:    "response",
		CreatedAt: prepared.Created,
		Status:    "failed",
		Model:     prepared.Model,
		Output:    []any{},
		Error:     body,
		Usage:     nil,
	})
}
