package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alexazhou/llm_mock_server/internal/model"
)

func testRegistry(ids ...string) *model.Registry {
	entries := make([]*model.Entry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, &model.Entry{RelPath: id + ".yaml", Script: &model.Script{Version: 1, ID: id}})
	}
	return model.NewRegistry(entries)
}

func TestRoutesCoverThreeProtocols(t *testing.T) {
	routes := Routes(testRegistry("b", "a"))
	if len(routes) != 6 {
		t.Fatalf("expected 6 routes, got %d", len(routes))
	}
	want := []struct {
		pattern  string
		protocol string
	}{
		{"POST /mock/a/v1/chat/completions", "openai"},
		{"POST /mock/a/v1/messages", "anthropic"},
		{"POST /mock/a/v1/responses", "responses"},
		{"POST /mock/b/v1/chat/completions", "openai"},
		{"POST /mock/b/v1/messages", "anthropic"},
		{"POST /mock/b/v1/responses", "responses"},
	}
	for i, w := range want {
		if routes[i].Pattern != w.pattern || routes[i].Protocol != w.protocol {
			t.Fatalf("route[%d] = %+v, want %s/%s", i, routes[i], w.pattern, w.protocol)
		}
	}
}

func TestRouterPlaceholdersAndMethodHandling(t *testing.T) {
	router := NewRouter(testRegistry("a"))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mock/a/v1/messages", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 placeholder, got %d", rec.Code)
	}
	var body errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body: %v", err)
	}
	if body.Error.Code != "mock_not_implemented" {
		t.Fatalf("unexpected error code: %+v", body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("unexpected content type %q", got)
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mock/a/v1/messages", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mock/missing/v1/messages", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown script, got %d", rec.Code)
	}
}
