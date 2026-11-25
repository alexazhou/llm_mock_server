package controller

import (
	"net/http"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/model"
)

// 内置 Models 端点（step3 E）：返回固定的 OpenAI model list，结构固定、不依赖
// 任何剧本配置。该端点不参与剧本 request.count，也不占用 max_active_requests。

// modelObject 是 OpenAI model list 里的单个条目。
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// modelList 是 `GET /models` 与 `GET /v1/models` 的响应。
type modelList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

// registerModels 注册 `/models` 与 `/v1/models`（不带 `/mock/{id}` 前缀）。
func registerModels(mux *http.ServeMux) {
	payload := modelList{Object: constants.ModelsListObject, Data: make([]modelObject, 0, len(constants.BuiltinModelIDs))}
	for _, id := range constants.BuiltinModelIDs {
		payload.Data = append(payload.Data, modelObject{
			ID:      id,
			Object:  "model",
			Created: 0,
			OwnedBy: constants.ModelsOwner,
		})
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, model.NewFrameworkError(model.CodeMethodNotAllowed,
				"method "+r.Method+" is not allowed; models endpoints only accept GET"))
			return
		}
		writeJSON(w, http.StatusOK, payload)
	}
	mux.HandleFunc(constants.ModelsPath, handler)
	mux.HandleFunc(constants.ModelsV1Path, handler)
}
