package model

import (
	"fmt"
	"sort"

	"github.com/alexazhou/llm_mock_server/internal/constants"
)

// Entry 是注册表中的一个剧本条目：保留来源文件便于诊断。
type Entry struct {
	// RelPath 是剧本文件相对 scripts_dir 的路径（使用 / 分隔）。
	RelPath string
	// AbsPath 是剧本文件的绝对路径。
	AbsPath string
	Script  *Script
}

// Registry 是加载完成后的只读剧本注册表。按 id 字典序保存。
type Registry struct {
	entries []*Entry
	byID    map[string]*Entry
}

// NewRegistry 用给定条目构建注册表；调用方保证 id 唯一。
func NewRegistry(entries []*Entry) *Registry {
	sorted := make([]*Entry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Script.ID < sorted[j].Script.ID })
	byID := make(map[string]*Entry, len(sorted))
	for _, e := range sorted {
		byID[e.Script.ID] = e
	}
	return &Registry{entries: sorted, byID: byID}
}

// Len 返回剧本数量。
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.entries)
}

// Entries 返回按 id 字典序排列的剧本条目副本。
func (r *Registry) Entries() []*Entry {
	if r == nil {
		return nil
	}
	out := make([]*Entry, len(r.entries))
	copy(out, r.entries)
	return out
}

// Lookup 按 id 查找剧本条目。
func (r *Registry) Lookup(id string) (*Entry, bool) {
	if r == nil {
		return nil, false
	}
	e, ok := r.byID[id]
	return e, ok
}

// ProtocolURLs 是一个剧本自动生成的三个协议 URL 路径。
type ProtocolURLs struct {
	OpenAI    string `json:"openai"`
	Anthropic string `json:"anthropic"`
	Responses string `json:"responses"`
}

// URLsFor 按剧本 id 生成三个固定协议 URL 路径。文件名与目录层级不参与寻址。
func URLsFor(id string) ProtocolURLs {
	return ProtocolURLs{
		OpenAI:    fmt.Sprintf(constants.RouteChatCompletions, id),
		Anthropic: fmt.Sprintf(constants.RouteMessages, id),
		Responses: fmt.Sprintf(constants.RouteResponses, id),
	}
}

// ScriptSummary 是 `scripts` 子命令与 GET /_test/scripts.json 共用的输出结构。
type ScriptSummary struct {
	ID   string       `json:"id"`
	URLs ProtocolURLs `json:"urls"`
}

// Summaries 返回按 id 字典序排列的剧本 id 与三个协议 URL。
func (r *Registry) Summaries() []ScriptSummary {
	entries := r.Entries()
	out := make([]ScriptSummary, 0, len(entries))
	for _, e := range entries {
		out = append(out, ScriptSummary{ID: e.Script.ID, URLs: URLsFor(e.Script.ID)})
	}
	return out
}
