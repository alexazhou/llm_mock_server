package model

import "net/http"

// 框架错误码（配置参考 §9）。B 阶段只产生其中的请求输入/选择类错误。
const (
	CodeInvalidJSON            = "mock_invalid_json"
	CodeScriptNotFound         = "mock_script_not_found"
	CodeMethodNotAllowed       = "mock_method_not_allowed"
	CodeBodyReadTimeout        = "mock_body_read_timeout"
	CodeBodyTooLarge           = "mock_body_too_large"
	CodeProtocolInvalid        = "mock_protocol_invalid"
	CodeOutputCountUnavailable = "mock_output_count_unavailable"
	CodeRoundNotFound          = "mock_round_not_found"
	CodeAssertionFailed        = "mock_assertion_failed"
	CodeOutputNotFound         = "mock_output_not_found"
	CodeOutputConditionError   = "mock_output_condition_error"
	CodeCapacityExceeded       = "mock_capacity_exceeded"
	CodeInternalError          = "mock_internal_error"
)

// FrameworkError 是响应头提交之前统一返回的结构化错误。
type FrameworkError struct {
	Status  int
	Code    string
	Message string
	// Details 使用稳定 JSON 字段名，按场景填充；不含 Go 类型名或调用栈。
	Details map[string]any
}

func (e *FrameworkError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// NewFrameworkError 构造一个带默认 HTTP 状态映射的框架错误。
func NewFrameworkError(code, message string) *FrameworkError {
	return &FrameworkError{Status: StatusForCode(code), Code: code, Message: message}
}

// WithDetails 追加诊断字段，返回自身便于链式构造。
func (e *FrameworkError) WithDetails(key string, value any) *FrameworkError {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = value
	return e
}

// StatusForCode 返回错误码到 HTTP 状态码的固定映射。
func StatusForCode(code string) int {
	switch code {
	case CodeInvalidJSON:
		return http.StatusBadRequest
	case CodeScriptNotFound:
		return http.StatusNotFound
	case CodeMethodNotAllowed:
		return http.StatusMethodNotAllowed
	case CodeBodyReadTimeout:
		return http.StatusRequestTimeout
	case CodeBodyTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeProtocolInvalid, CodeOutputCountUnavailable, CodeRoundNotFound, CodeAssertionFailed:
		return http.StatusUnprocessableEntity
	case CodeCapacityExceeded:
		return http.StatusServiceUnavailable
	case CodeOutputNotFound, CodeOutputConditionError, CodeInternalError:
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}
