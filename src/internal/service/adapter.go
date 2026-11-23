package service

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/alexazhou/llm_mock_server/internal/constants"
)

// 本文件是协议适配器的公共部分：响应 ID 生成、错误 type 默认值、以及三个
// 协议共用的 JSON 编码入口。协议专属结构在 adapter_openai.go /
// adapter_anthropic.go / adapter_responses.go。

// ContentTypeJSON 是三个协议的默认响应 Content-Type；显式 headers 可以覆盖。
const ContentTypeJSON = "application/json"

// NewResponseID 生成与协议形态一致的响应 ID。
func NewResponseID(protocol string) string {
	switch protocol {
	case constants.ProtocolAnthropic:
		return "msg_" + randomHex(12)
	case constants.ProtocolResponses:
		return "resp_" + randomHex(12)
	default:
		return "chatcmpl-" + randomHex(12)
	}
}

// newItemID 生成 Responses output item 的 ID（前缀区分 reasoning / message / function_call）。
func newItemID(prefix string) string {
	return prefix + randomHex(12)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "0"
	}
	return hex.EncodeToString(buf)
}

// defaultErrorType 是按 HTTP 状态推导的协议默认错误 type。
func defaultErrorType(status int) string {
	if status >= 500 {
		return "server_error"
	}
	return "invalid_request_error"
}

// errorType 返回生效的错误 type：显式配置优先，否则按状态推导。
func errorType(prepared *PreparedOutput) string {
	if prepared.Error != nil && prepared.Error.HasType {
		return prepared.Error.Type
	}
	return defaultErrorType(prepared.Status)
}

// encodeNonStream 把准备好的输出编码成非流式响应 body。
// Empty 返回 nil（0 字节 body）；错误按状态与协议选择 error 体或 failed response。
func encodeNonStream(prepared *PreparedOutput) ([]byte, error) {
	if prepared.Error != nil {
		return encodeError(prepared)
	}
	if prepared.Empty {
		return nil, nil
	}
	switch prepared.Protocol {
	case constants.ProtocolAnthropic:
		return encodeAnthropicMessage(prepared)
	case constants.ProtocolResponses:
		return encodeResponsesObject(prepared)
	default:
		return encodeOpenAIChatCompletion(prepared)
	}
}

// encodeError 按「status 是否 4xx/5xx」与协议选择错误编码。
func encodeError(prepared *PreparedOutput) ([]byte, error) {
	if isErrorStatus(prepared.Status) {
		switch prepared.Protocol {
		case constants.ProtocolAnthropic:
			return encodeAnthropicError(prepared)
		case constants.ProtocolResponses:
			return encodeResponsesHTTPError(prepared)
		default:
			return encodeOpenAIError(prepared)
		}
	}
	// 只有 Responses 存在 2xx 内的 failed response。
	return encodeResponsesFailed(prepared)
}

// encodeJSON 统一用紧凑 JSON，保证响应字节稳定。
func encodeJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode response: %v", err)
	}
	return raw, nil
}
