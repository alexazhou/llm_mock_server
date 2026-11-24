// Package constants 集中定义全项目共享的常量：schema 版本、协议名、
// 固定路由模板、内置默认值和退出码。业务与协议无关的字面量都应放在这里。
package constants

import "time"

// SchemaVersion 是 server.yaml 与剧本 YAML 唯一支持的 schema 版本。
const SchemaVersion = 1

// 三个协议名。协议由请求 URL 唯一决定，不从请求内容猜测。
const (
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"
	ProtocolResponses = "responses"
)

// 每个剧本固定生成的三条协议路由模板（URL 模板参数为剧本 id）。
// 文件名与目录层级不参与寻址，也不提供可配置 URL prefix。
const (
	RouteChatCompletions = "/mock/%s/v1/chat/completions"
	RouteMessages        = "/mock/%s/v1/messages"
	RouteResponses       = "/mock/%s/v1/responses"
)

// 配置与扫描相关的路径默认值。
const (
	DefaultConfigFile = "server.yaml"
	DefaultScriptsDir = "scripts"
	DefaultListen     = "127.0.0.1:9999"
)

// ScriptIDPattern 是剧本 id 的合法形式。
const ScriptIDPattern = `^[a-z0-9][a-z0-9_-]{0,63}$`

// 剧本与 server 配置的内置默认值（省略字段时使用）。
const (
	DefaultMaxBodyBytes         int64 = 8388608
	DefaultReadBodyTimeout            = 15 * time.Second
	DefaultStatus                     = 200
	DefaultReadHeaderTimeout          = 5 * time.Second
	DefaultMaxActiveRequests    int64 = 256
	DefaultMaxOutputBytes       int64 = 67108864
	DefaultMaxFixtureBytes      int64 = 8388608
	DefaultMaxGeneratedEvents   int64 = 10000
	DefaultMaxWriteUnits        int64 = 100000
	DefaultShutdownTimeout            = 3 * time.Second
	DefaultCaptureCapacity      int64 = 500
	DefaultCaptureMaxBodyBytes  int64 = 8388608
	DefaultCaptureMaxTotalBytes int64 = 67108864
)

// 布尔/枚举类默认值。
const (
	DefaultStream     = StreamAuto
	DefaultLineEnding = LineEndingLF
	DefaultUsageMode  = UsageModeAuto
)

// config.stream 取值。
const (
	StreamAuto  = "auto"
	StreamTrue  = "true"
	StreamFalse = "false"
)

// config.line_ending 取值。
const (
	LineEndingLF   = "lf"
	LineEndingCRLF = "crlf"
)

// config.usage_mode 取值。
const (
	UsageModeAuto = "auto"
	UsageModeEmit = "emit"
	UsageModeOmit = "omit"
)

// 进程退出码。
const (
	ExitOK          = 0
	ExitConfigError = 1
	ExitUsageError  = 2
)

// 内置 Models 端点（技术设计 §8 / step3 E）：固定列表，不进入剧本 DSL，
// 不参与任何剧本的 request.count，也不占用 max_active_requests。
const (
	ModelsPath       = "/models"
	ModelsV1Path     = "/v1/models"
	ModelsListObject = "list"
	ModelsOwner      = "llm-mock-server"
)

// BuiltinModelIDs 是 Models 端点返回的固定 model id 列表（顺序稳定，便于判据比对）。
var BuiltinModelIDs = []string{
	"gpt-4o",
	"gpt-4o-mini",
	"claude-3-5-sonnet-latest",
	"mock-tool-model",
}
