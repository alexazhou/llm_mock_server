package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/model"
	"github.com/alexazhou/llm_mock_server/internal/utils"
)

// ParsedRequest 是协议解析器对单个请求的归一化结果。
type ParsedRequest struct {
	Model       string
	Stream      bool
	OutputCount int
	// RoundInput 是最后一个历史模型输出之后新增的输入项（首轮剥离前置 system/developer 后）。
	RoundInput any
	// JSONBody 是规范化后的请求 JSON（数字保留原始文本）。
	JSONBody any
}

// ParseProtocolRequest 按 URL 选定的协议解析请求；不做协议猜测或回退。
// 只做最小结构校验：JSON Object、model 非空字符串、stream 布尔、对话历史结构合法。
func ParseProtocolRequest(protocol string, body []byte) (*ParsedRequest, *model.FrameworkError) {
	decoder := json.NewDecoder(bytesReader(body))
	decoder.UseNumber()
	var raw any
	if err := decoder.Decode(&raw); err != nil {
		return nil, model.NewFrameworkError(model.CodeInvalidJSON, "request body is not valid JSON: "+err.Error())
	}
	if err := ensureTrailingWhitespace(decoder); err != nil {
		return nil, model.NewFrameworkError(model.CodeInvalidJSON, err.Error())
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, model.NewFrameworkError(model.CodeProtocolInvalid, "request body must be a JSON object")
	}
	normalized := utils.NormalizeJSON(obj).(map[string]any)

	modelName, err := requiredString(normalized, "model")
	if err != nil {
		return nil, model.NewFrameworkError(model.CodeProtocolInvalid, err.Error())
	}
	stream, err := optionalBool(normalized, "stream")
	if err != nil {
		return nil, model.NewFrameworkError(model.CodeProtocolInvalid, err.Error())
	}

	parsed := &ParsedRequest{Model: modelName, Stream: stream, JSONBody: normalized}
	switch protocol {
	case constants.ProtocolOpenAI, constants.ProtocolAnthropic:
		count, roundInput, ferr := parseMessagesProtocol(normalized, protocol)
		if ferr != nil {
			return nil, ferr
		}
		parsed.OutputCount = count
		parsed.RoundInput = roundInput
	case constants.ProtocolResponses:
		count, roundInput, ferr := parseResponsesProtocol(normalized)
		if ferr != nil {
			return nil, ferr
		}
		parsed.OutputCount = count
		parsed.RoundInput = roundInput
	default:
		return nil, model.NewFrameworkError(model.CodeInternalError, "unknown protocol "+protocol)
	}
	return parsed, nil
}

// parseMessagesProtocol 处理 OpenAI Chat 与 Anthropic Messages：
// 统计 role=assistant 的消息数作为 output_count。
func parseMessagesProtocol(body map[string]any, protocol string) (int, any, *model.FrameworkError) {
	rawMessages, ok := body["messages"]
	if !ok {
		return 0, nil, model.NewFrameworkError(model.CodeProtocolInvalid, "messages is required")
	}
	messages, ok := rawMessages.([]any)
	if !ok {
		return 0, nil, model.NewFrameworkError(model.CodeProtocolInvalid, "messages must be an array")
	}
	for i, item := range messages {
		obj, ok := item.(map[string]any)
		if !ok {
			return 0, nil, model.NewFrameworkError(model.CodeProtocolInvalid,
				fmt.Sprintf("messages[%d] must be an object", i))
		}
		if _, ok := obj["role"].(string); !ok {
			return 0, nil, model.NewFrameworkError(model.CodeProtocolInvalid,
				fmt.Sprintf("messages[%d].role must be a string", i))
		}
	}

	count := 0
	lastAssistant := -1
	for i, item := range messages {
		if role, _ := item.(map[string]any)["role"].(string); role == "assistant" {
			count++
			lastAssistant = i
		}
	}
	if count == 0 {
		start := 0
		if protocol == constants.ProtocolOpenAI {
			start = stripLeadingSystemMessages(messages)
		}
		return 0, append([]any{}, messages[start:]...), nil
	}
	return count, append([]any{}, messages[lastAssistant+1:]...), nil
}

// stripLeadingSystemMessages 返回首个既非 system 也非 developer 消息的下标。
func stripLeadingSystemMessages(messages []any) int {
	for i, item := range messages {
		role, _ := item.(map[string]any)["role"].(string)
		if role != "system" && role != "developer" {
			return i
		}
	}
	return len(messages)
}

// parseResponsesProtocol 处理 OpenAI Responses：
// 连续的 reasoning / assistant message / function_call item 归为一轮模型输出，
// 遇到 user/developer message 或 function_call_output 结束该组。
func parseResponsesProtocol(body map[string]any) (int, any, *model.FrameworkError) {
	rawInput, hasInput := body["input"]
	var input []any
	switch typed := rawInput.(type) {
	case string:
		input = []any{map[string]any{"role": "user", "content": typed}}
	case []any:
		input = typed
	case nil:
		if !hasInput {
			if _, hasPrevious := body["previous_response_id"]; hasPrevious {
				return 0, nil, model.NewFrameworkError(model.CodeOutputCountUnavailable,
					"request carries previous_response_id without countable input history")
			}
			return 0, nil, model.NewFrameworkError(model.CodeProtocolInvalid, "input is required")
		}
		return 0, nil, model.NewFrameworkError(model.CodeProtocolInvalid, "input must be a string or an array")
	default:
		return 0, nil, model.NewFrameworkError(model.CodeProtocolInvalid, "input must be a string or an array")
	}
	if len(input) == 0 {
		if _, hasPrevious := body["previous_response_id"]; hasPrevious {
			return 0, nil, model.NewFrameworkError(model.CodeOutputCountUnavailable,
				"request carries previous_response_id without countable input history")
		}
		return 0, nil, model.NewFrameworkError(model.CodeProtocolInvalid, "input must not be empty")
	}
	for i, item := range input {
		if _, ok := item.(map[string]any); !ok {
			// 纯字符串数组是合法输入形态，按 user 输入项处理。
			if _, isString := item.(string); isString {
				input[i] = map[string]any{"role": "user", "content": item}
				continue
			}
			return 0, nil, model.NewFrameworkError(model.CodeProtocolInvalid,
				fmt.Sprintf("input[%d] must be an object or string", i))
		}
	}

	count := 0
	lastModelOutput := -1
	inGroup := false
	for i, item := range input {
		if isResponsesModelOutput(item.(map[string]any)) {
			if !inGroup {
				count++
				inGroup = true
			}
			lastModelOutput = i
			continue
		}
		inGroup = false
	}
	if count == 0 {
		start := stripLeadingSystemMessages(input)
		if _, hasPrevious := body["previous_response_id"]; hasPrevious && start == len(input) {
			return 0, nil, model.NewFrameworkError(model.CodeOutputCountUnavailable,
				"request carries previous_response_id without countable input history")
		}
		return 0, append([]any{}, input[start:]...), nil
	}
	return count, append([]any{}, input[lastModelOutput+1:]...), nil
}

// isResponsesModelOutput 判断一个 Responses input item 是否由模型产生。
func isResponsesModelOutput(item map[string]any) bool {
	itemType, _ := item["type"].(string)
	switch itemType {
	case "reasoning", "function_call":
		return true
	case "message", "":
		role, _ := item["role"].(string)
		return role == "assistant"
	}
	return false
}

func requiredString(body map[string]any, field string) (string, error) {
	value, ok := body[field]
	if !ok {
		return "", fmt.Errorf("%s is required", field)
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return "", fmt.Errorf("%s must be a non-empty string", field)
	}
	return text, nil
}

func optionalBool(body map[string]any, field string) (bool, error) {
	value, ok := body[field]
	if !ok || value == nil {
		return false, nil
	}
	flag, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", field)
	}
	return flag, nil
}

func bytesReader(body []byte) *bytes.Reader {
	return bytes.NewReader(body)
}

// ensureTrailingWhitespace 确认首个 JSON 值之后只有空白（拒绝第二个 JSON 值或尾随内容）。
func ensureTrailingWhitespace(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("request body must contain exactly one JSON value")
	}
	return nil
}
