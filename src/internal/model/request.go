package model

import (
	"net/http"
	"net/url"
)

// RequestContext 是单次请求的只读视图，供系统 key、断言与条件求值读取。
// 除 Count 之外的字段在协议解析后即固定，Count 在输入断言全部通过后才写入。
type RequestContext struct {
	RequestID string
	Protocol  string
	// ScriptID 是命中的剧本 id（注册表 key）。
	ScriptID string
	Method   string
	Path     string

	// BodyText 是通过严格 UTF-8 校验后的原始请求 body 文本。
	BodyText string
	// JSONBody 是解析后的请求 JSON（数字保留原始十进制文本）。
	JSONBody any
	// Header 与 Query 保留完整请求信息。
	Header http.Header
	Query  url.Values

	// Parsed 表示协议解析已成功；为 false 时 OutputCount 无意义，不进诊断。
	Parsed bool
	// Model / Stream 来自协议解析器。
	Model  string
	Stream bool
	// OutputCount 是请求历史中已包含的模型输出轮数。
	OutputCount int
	// RoundInput 是最后一个历史模型输出之后新增的输入项列表。
	RoundInput any
	// RoundIndex 是定位到的 round 序号（1 起）；0 表示尚未定位。
	RoundIndex int
	// Count 是 request.count；nil 表示尚未分配。
	Count *uint64
}
