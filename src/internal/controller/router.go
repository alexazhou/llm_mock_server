// Package controller 负责路由注册与 HTTP 层。A 阶段只注册每个剧本固定
// 生成的三条协议路由；请求执行流水线在 B/C 阶段接入。
package controller

import (
	"encoding/json"
	"net/http"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/model"
)

// Route 描述一条已注册的剧本协议路由。
type Route struct {
	Pattern  string
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
			Route{Pattern: "POST " + urls.OpenAI, ScriptID: entry.Script.ID, Protocol: constants.ProtocolOpenAI},
			Route{Pattern: "POST " + urls.Anthropic, ScriptID: entry.Script.ID, Protocol: constants.ProtocolAnthropic},
			Route{Pattern: "POST " + urls.Responses, ScriptID: entry.Script.ID, Protocol: constants.ProtocolResponses},
		)
	}
	return routes
}

// NewRouter 注册全部剧本协议路由。A 阶段使用占位 handler：真实请求流水线
// 由 B（解析/定位/计数）与 C（输出/传输）阶段替换。
func NewRouter(reg *model.Registry) *http.ServeMux {
	mux := http.NewServeMux()
	for _, route := range Routes(reg) {
		route := route
		mux.HandleFunc(route.Pattern, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotImplemented, notImplementedBody(route))
		})
	}
	return mux
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func notImplementedBody(route Route) errorEnvelope {
	return errorEnvelope{Error: errorBody{
		Code:    "mock_not_implemented",
		Message: "phase A scaffold: request execution is not implemented for " + route.ScriptID + " (" + route.Protocol + ")",
	}}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
