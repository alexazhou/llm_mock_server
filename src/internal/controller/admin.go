package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/alexazhou/llm_mock_server/internal/model"
	"github.com/alexazhou/llm_mock_server/internal/service"
)

// 本文件实现 `GET /health.json` 与 `/_test/*` 管理接口的 HTTP 层。
// 业务逻辑在 service.Admin；这里只做方法校验、请求体解码与 JSON 编码。
// 管理接口不占用 max_active_requests，也不参与任何剧本的 request.count。

// adminBodyLimit 是管理接口请求体上限：只有 verify 与批量 reset 需要请求体。
const adminBodyLimit = 64 << 10

// HealthPath 是就绪探测路径（F 阶段会用它做子进程就绪判断）。
const HealthPath = "/health.json"

// registerAdmin 注册全部管理接口。
func registerAdmin(mux *http.ServeMux, admin *service.Admin) {
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, r *http.Request) {
		if !allowMethod(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, http.StatusOK, admin.Health())
	})
	mux.HandleFunc("/_test/scripts.json", func(w http.ResponseWriter, r *http.Request) {
		if !allowMethod(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, http.StatusOK, admin.Scripts())
	})
	mux.HandleFunc("/_test/requests", func(w http.ResponseWriter, r *http.Request) {
		if !allowMethod(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, http.StatusOK, admin.Requests())
	})
	mux.HandleFunc("/_test/capture/snapshot.json", func(w http.ResponseWriter, r *http.Request) {
		if !allowMethod(w, r, http.MethodGet) {
			return
		}
		writeJSON(w, http.StatusOK, admin.Snapshot())
	})
	mux.HandleFunc("/_test/capture/verify.json", func(w http.ResponseWriter, r *http.Request) {
		if !allowMethod(w, r, http.MethodPost) {
			return
		}
		var body verifyRequest
		if ferr := decodeAdminBody(w, r, &body); ferr != nil {
			writeError(w, ferr)
			return
		}
		report, ferr := admin.Verify(body.InstanceID, body.Cursor, body.ScriptIDs, body.AllowFrameworkErrors)
		if ferr != nil {
			writeError(w, ferr)
			return
		}
		writeJSON(w, http.StatusOK, report)
	})
	mux.HandleFunc("/_test/capture/clear.json", func(w http.ResponseWriter, r *http.Request) {
		if !allowMethod(w, r, http.MethodPost) {
			return
		}
		writeJSON(w, http.StatusOK, admin.Clear())
	})
	mux.HandleFunc("/_test/counters/reset.json", func(w http.ResponseWriter, r *http.Request) {
		if !allowMethod(w, r, http.MethodPost) {
			return
		}
		var body resetManyRequest
		if ferr := decodeAdminBody(w, r, &body); ferr != nil {
			writeError(w, ferr)
			return
		}
		result, ferr := admin.ResetCounters(body.ScriptIDs)
		if ferr != nil {
			writeError(w, ferr)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("/_test/scripts/", handleScriptCounter(admin))
}

// verifyRequest 是 `POST /_test/capture/verify.json` 的请求体。
type verifyRequest struct {
	InstanceID           string   `json:"instance_id"`
	Cursor               uint64   `json:"cursor"`
	ScriptIDs            []string `json:"script_ids"`
	AllowFrameworkErrors bool     `json:"allow_framework_errors"`
}

// resetManyRequest 是 `POST /_test/counters/reset.json` 的请求体；省略 script_ids 表示全部剧本。
type resetManyRequest struct {
	ScriptIDs []string `json:"script_ids"`
}

// handleScriptCounter 处理 `/_test/scripts/{id}/counter.json` 与
// `/_test/scripts/{id}/counter/reset.json` 两条带剧本 id 的路径。
func handleScriptCounter(admin *service.Admin) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/_test/scripts/")
		switch {
		case strings.HasSuffix(rest, "/counter/reset.json"):
			id := strings.TrimSuffix(rest, "/counter/reset.json")
			if id == "" || strings.Contains(id, "/") {
				writeError(w, unknownAdminPath(r.URL.Path))
				return
			}
			if !allowMethod(w, r, http.MethodPost) {
				return
			}
			view, ferr := admin.ResetCounter(id)
			if ferr != nil {
				writeError(w, ferr)
				return
			}
			writeJSON(w, http.StatusOK, view)
		case strings.HasSuffix(rest, "/counter.json"):
			id := strings.TrimSuffix(rest, "/counter.json")
			if id == "" || strings.Contains(id, "/") {
				writeError(w, unknownAdminPath(r.URL.Path))
				return
			}
			if !allowMethod(w, r, http.MethodGet) {
				return
			}
			view, ferr := admin.Counter(id)
			if ferr != nil {
				writeError(w, ferr)
				return
			}
			writeJSON(w, http.StatusOK, view)
		default:
			writeError(w, unknownAdminPath(r.URL.Path))
		}
	}
}

// allowMethod 校验方法；不允许时写 405 并返回 false。
func allowMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeError(w, model.NewFrameworkError(model.CodeMethodNotAllowed,
		"method "+r.Method+" is not allowed; this endpoint accepts "+method))
	return false
}

// decodeAdminBody 严格解码管理接口的 JSON 请求体；空 body 视为全默认值。
func decodeAdminBody(w http.ResponseWriter, r *http.Request, target any) *model.FrameworkError {
	decoder := json.NewDecoder(io.LimitReader(r.Body, adminBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		ferr := model.NewFrameworkError(model.CodeInvalidJSON, "invalid admin request body: "+err.Error())
		return ferr
	}
	return nil
}

// unknownAdminPath 构造管理接口的未知路径错误。
func unknownAdminPath(path string) *model.FrameworkError {
	return model.NewFrameworkError(model.CodeScriptNotFound, "no admin route matches "+path)
}
