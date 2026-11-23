// Package controller 负责路由注册与 HTTP 响应编码。请求流水线、输出准备与
// 传输都在 service 中实现，这里只做路径分发与框架错误 envelope。
package controller

import (
	"encoding/json"
	"net/http"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/model"
	"github.com/alexazhou/llm_mock_server/internal/service"
)

// Route 描述一条已注册的剧本协议路由。
type Route struct {
	Path     string
	ScriptID string
	Protocol string
}

// Routes 返回按剧本 id 字典序排列的全部协议路由。文件名与目录层级不参与寻址。
func Routes(reg *model.Registry) []Route {
	entries := reg.Entries()
	routes := make([]Route, 0, len(entries)*3)
	for _, entry := range entries {
		urls := model.URLsFor(entry.Script.ID)
		routes = append(routes,
			Route{Path: urls.OpenAI, ScriptID: entry.Script.ID, Protocol: constants.ProtocolOpenAI},
			Route{Path: urls.Anthropic, ScriptID: entry.Script.ID, Protocol: constants.ProtocolAnthropic},
			Route{Path: urls.Responses, ScriptID: entry.Script.ID, Protocol: constants.ProtocolResponses},
		)
	}
	return routes
}

// NewRouter 注册全部剧本协议路由与兜底 404。方法不支持时由 handler 返回统一 JSON 405，
// 不使用 Go 默认纯文本错误页。
func NewRouter(loaded *service.Loaded) *http.ServeMux {
	executor := service.NewExecutor(loaded)
	mux := http.NewServeMux()
	for _, route := range Routes(loaded.Registry) {
		route := route
		mux.HandleFunc(route.Path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				writeError(w, model.NewFrameworkError(model.CodeMethodNotAllowed,
					"method "+r.Method+" is not allowed; script routes only accept POST"))
				return
			}
			result, ferr := executor.Execute(w, r, route.ScriptID, route.Protocol)
			if ferr != nil {
				writeError(w, ferr)
				return
			}
			// 响应头可能已提交，写失败只可能是客户端断开，不再写错误 envelope。
			_ = service.WriteOutput(w, r, result.Prepared)
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, model.NewFrameworkError(model.CodeScriptNotFound,
			"no route matches "+r.URL.Path))
	})
	return mux
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	ScriptID  string         `json:"script_id,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Protocol  string         `json:"protocol,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

func writeError(w http.ResponseWriter, ferr *model.FrameworkError) {
	status := ferr.Status
	if status == 0 {
		status = model.StatusForCode(ferr.Code)
	}
	body := errorEnvelope{Error: errorBody{Code: ferr.Code, Message: ferr.Message, Details: ferr.Details}}
	if body.Error.Details != nil {
		// 剧本/请求/协议三个上下文字段提到 envelope 顶层，避免重复出现。
		body.Error.ScriptID = takeString(body.Error.Details, "script_id")
		body.Error.RequestID = takeString(body.Error.Details, "request_id")
		body.Error.Protocol = takeString(body.Error.Details, "protocol")
		if len(body.Error.Details) == 0 {
			body.Error.Details = nil
		}
	}
	writeJSON(w, status, body)
}

func takeString(details map[string]any, key string) string {
	value, ok := details[key]
	if !ok {
		return ""
	}
	delete(details, key)
	text, _ := value.(string)
	return text
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
