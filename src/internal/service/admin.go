package service

import (
	"sort"

	"github.com/alexazhou/llm_mock_server/internal/model"
)

// 本文件实现 `GET /health.json` 与 `/_test/*` 管理接口的业务逻辑（技术设计 §8）。
// HTTP 编码由 controller 完成，这里只返回数据或框架错误。
// 管理接口不占用 max_active_requests，也不参与任何剧本的 request.count。

// Admin 聚合管理接口需要的只读视图与可变状态。Counters 与 Capture 必须与
// Executor 使用同一实例，否则查询到的不是同一份进程状态。
type Admin struct {
	Loaded   *Loaded
	Counters *CounterStore
	Capture  *Capture
}

// NewAdmin 用与执行器共享的计数状态和 capture 窗口构建管理接口。
func NewAdmin(loaded *Loaded, counters *CounterStore, capture *Capture) *Admin {
	return &Admin{Loaded: loaded, Counters: counters, Capture: capture}
}

// ScriptURLs 是剧本固定生成的三条协议路径。
type ScriptURLs struct {
	OpenAI    string `json:"openai"`
	Anthropic string `json:"anthropic"`
	Responses string `json:"responses"`
}

// ScriptEntry 是 scripts 命令与 `GET /_test/scripts.json` 共用的结构。
type ScriptEntry struct {
	ID   string     `json:"id"`
	URLs ScriptURLs `json:"urls"`
}

// Health 返回就绪状态、实例标识与剧本数。
func (a *Admin) Health() map[string]any {
	return map[string]any{
		"status":      "ready",
		"instance_id": a.Capture.InstanceID(),
		"scripts":     len(a.Loaded.Registry.Entries()),
	}
}

// Scripts 返回按 id 字典序排列的剧本与三条协议路径。
func (a *Admin) Scripts() []ScriptEntry {
	entries := a.Loaded.Registry.Entries()
	out := make([]ScriptEntry, 0, len(entries))
	for _, entry := range entries {
		urls := model.URLsFor(entry.Script.ID)
		out = append(out, ScriptEntry{ID: entry.Script.ID, URLs: ScriptURLs{
			OpenAI:    urls.OpenAI,
			Anthropic: urls.Anthropic,
			Responses: urls.Responses,
		}})
	}
	return out
}

// Requests 返回当前 capture 窗口内的记录数组（按 sequence 升序）。
func (a *Admin) Requests() []CaptureRecord {
	records := a.Capture.List()
	if records == nil {
		records = []CaptureRecord{}
	}
	return records
}

// Snapshot 返回 instance_id 与当前 sequence cursor。
func (a *Admin) Snapshot() map[string]any {
	return map[string]any{
		"instance_id": a.Capture.InstanceID(),
		"cursor":      a.Capture.Cursor(),
	}
}

// Clear 清空 capture 窗口，只返回被清理的记录数。
func (a *Admin) Clear() map[string]any {
	return map[string]any{"cleared": a.Capture.Clear()}
}

// Verify 校验 cursor 之后新增记录里的 Mock 框架错误。
func (a *Admin) Verify(instanceID string, cursor uint64, scriptIDs []string, allowFrameworkErrors bool) (*CaptureVerifyReport, *model.FrameworkError) {
	return a.Capture.Verify(instanceID, cursor, scriptIDs, allowFrameworkErrors)
}

// CounterView 是 counter 查询与单剧本 reset 的响应结构（技术设计 §8 固定口径）。
type CounterView struct {
	Counts         map[string]uint64 `json:"counts"`
	ActiveRequests int64             `json:"active_requests"`
}

// Counter 返回指定剧本三个协议的 count 与 active_requests。
func (a *Admin) Counter(scriptID string) (*CounterView, *model.FrameworkError) {
	counters := a.Counters.For(scriptID)
	if counters == nil {
		return nil, unknownScriptError(scriptID)
	}
	return &CounterView{Counts: counters.Counts(), ActiveRequests: counters.Active()}, nil
}

// ResetCounter 在无活动请求时重置指定剧本的三个协议 counter。
func (a *Admin) ResetCounter(scriptID string) (*CounterView, *model.FrameworkError) {
	counters := a.Counters.For(scriptID)
	if counters == nil {
		return nil, unknownScriptError(scriptID)
	}
	if !counters.Reset() {
		return nil, resetConflictError([]string{scriptID})
	}
	return &CounterView{Counts: counters.Counts(), ActiveRequests: counters.Active()}, nil
}

// ResetCounters 批量重置；scriptIDs 为空表示全部剧本。任一目标剧本存在活动请求时
// 返回 409 且不部分重置——所有目标剧本先按 id 顺序加锁，再统一检查与归零。
func (a *Admin) ResetCounters(scriptIDs []string) (map[string]any, *model.FrameworkError) {
	reset, conflicts, unknown := a.Counters.ResetMany(scriptIDs)
	if len(unknown) > 0 {
		ferr := model.NewFrameworkError(model.CodeCounterUnknownScript, "unknown script id")
		ferr.WithDetails("script_ids", unknown)
		return nil, ferr
	}
	if len(conflicts) > 0 {
		return nil, resetConflictError(conflicts)
	}
	return map[string]any{"reset_script_ids": reset}, nil
}

// unknownScriptError 构造未知剧本 id 的错误。
func unknownScriptError(scriptID string) *model.FrameworkError {
	ferr := model.NewFrameworkError(model.CodeCounterUnknownScript, "script "+scriptID+" is not registered")
	ferr.WithDetails("script_id", scriptID)
	return ferr
}

// resetConflictError 构造「有活动请求，拒绝 reset」的 409 错误。
func resetConflictError(scriptIDs []string) *model.FrameworkError {
	ferr := model.NewFrameworkError(model.CodeCounterResetConflict,
		"counter reset is not allowed while the script has active requests")
	ferr.WithDetails("script_ids", scriptIDs)
	return ferr
}

// ResetMany 批量重置剧本 counter：任一目标剧本存在活动请求时返回 conflicts 且
// 不做部分重置；未知 id 通过 unknown 返回。scriptIDs 为空表示全部剧本。
// 所有目标剧本按 id 字典序依次加锁，避免批量与准入之间插入新请求，也避免死锁。
func (s *CounterStore) ResetMany(scriptIDs []string) (reset []string, conflicts []string, unknown []string) {
	targets := s.targets(scriptIDs)
	for _, target := range targets {
		if target.counters == nil {
			unknown = append(unknown, target.id)
		}
	}
	if len(unknown) > 0 {
		return nil, nil, unknown
	}

	locked := make([]*ScriptCounters, 0, len(targets))
	for _, target := range targets {
		target.counters.mu.Lock()
		locked = append(locked, target.counters)
	}
	defer func() {
		for i := len(locked) - 1; i >= 0; i-- {
			locked[i].mu.Unlock()
		}
	}()

	for _, target := range targets {
		if target.counters.active > 0 {
			conflicts = append(conflicts, target.id)
		}
	}
	if len(conflicts) > 0 {
		return nil, conflicts, nil
	}
	for _, target := range targets {
		for protocol := range target.counters.counts {
			target.counters.counts[protocol] = 0
		}
		reset = append(reset, target.id)
	}
	return reset, nil, nil
}

// counterTarget 是批量 reset 的目标（id 与计数状态）。
type counterTarget struct {
	id       string
	counters *ScriptCounters
}

// targets 解析批量 reset 的目标列表，按 id 字典序返回（未知 id 的 counters 为 nil）。
func (s *CounterStore) targets(scriptIDs []string) []counterTarget {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(scriptIDs) == 0 {
		out := make([]counterTarget, 0, len(s.scripts))
		for id, counters := range s.scripts {
			out = append(out, counterTarget{id: id, counters: counters})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
		return out
	}
	seen := map[string]bool{}
	out := make([]counterTarget, 0, len(scriptIDs))
	for _, id := range scriptIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, counterTarget{id: id, counters: s.scripts[id]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}
