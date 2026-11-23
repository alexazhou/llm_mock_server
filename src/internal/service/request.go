package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/model"
)

// Executor 执行 B 阶段的完整决策流水线：
// 容量控制 → 读取 body → 严格 JSON/UTF-8 → 协议解析 → round 定位 →
// 输入断言 → 分配 request.count → 匹配 output。
type Executor struct {
	Loaded   *Loaded
	Counters *CounterStore
	Capacity *Capacity
}

// NewExecutor 基于加载结果构建执行器。
func NewExecutor(loaded *Loaded) *Executor {
	return &Executor{
		Loaded:   loaded,
		Counters: NewCounterStore(loaded.Registry),
		Capacity: NewCapacity(loaded.Resolved.MaxActiveRequests),
	}
}

// ExecutionResult 是 B 阶段成功决策的结果，用于组装占位响应。
type ExecutionResult struct {
	ScriptID     string
	Protocol     string
	RequestID    string
	RequestCount uint64
	OutputCount  int
	RoundIndex   int
	OutputIndex  int
	Status       int
	Stream       bool
}

// Execute 处理一次剧本请求。返回的 FrameworkError 由调用方按统一 envelope 输出。
func (e *Executor) Execute(w http.ResponseWriter, r *http.Request, scriptID, protocol string) (*ExecutionResult, *model.FrameworkError) {
	script, ok := e.Loaded.Compiled[scriptID]
	if !ok {
		return nil, model.NewFrameworkError(model.CodeScriptNotFound, "script "+scriptID+" is not registered")
	}
	requestID := newRequestID()

	// 容量控制必须在读取 body 之前：满则直接 503，不分配 request.count。
	if !e.Capacity.TryAcquire() {
		ferr := model.NewFrameworkError(model.CodeCapacityExceeded, "too many active requests")
		ferr.WithDetails("max_active_requests", e.Loaded.Resolved.MaxActiveRequests)
		ferr.WithDetails("script_id", scriptID)
		ferr.WithDetails("protocol", protocol)
		ferr.WithDetails("request_id", requestID)
		return nil, ferr
	}
	defer e.Capacity.Release()

	counters := e.Counters.For(scriptID)
	if counters == nil {
		counters = NewScriptCounters()
	}
	counters.Acquire()
	defer counters.Release()

	ctx := &model.RequestContext{
		RequestID: requestID,
		ScriptID:  scriptID,
		Protocol:  protocol,
		Method:    r.Method,
		Path:      r.URL.Path,
		Header:    r.Header,
		Query:     r.URL.Query(),
	}

	limit, timeout := e.bodyLimits(script.Script)
	body, ferr := readRequestBody(w, r, limit, timeout)
	if ferr != nil {
		return nil, withBase(ferr, ctx)
	}
	ctx.BodyText = string(body)
	if !utf8.Valid(body) {
		return nil, withBase(model.NewFrameworkError(model.CodeInvalidJSON,
			"request body must be valid UTF-8"), ctx)
	}

	parsed, ferr := ParseProtocolRequest(protocol, body)
	if ferr != nil {
		return nil, withBase(ferr, ctx)
	}
	ctx.JSONBody = parsed.JSONBody
	ctx.Model = parsed.Model
	ctx.Stream = parsed.Stream
	ctx.Parsed = true
	ctx.OutputCount = parsed.OutputCount
	ctx.RoundInput = parsed.RoundInput

	round, ferr := LocateRound(script, ctx.OutputCount)
	if ferr != nil {
		return nil, withBase(ferr, ctx)
	}
	ctx.RoundIndex = round.Index

	if ferr := runAssertions(script.Assert, ctx, "assert"); ferr != nil {
		return nil, ferr
	}
	if ferr := runAssertions(round.InputAssert, ctx, "round.input.assert"); ferr != nil {
		return nil, ferr
	}

	// 输入断言全部通过后才分配 request.count。
	count := counters.Next(protocol)
	ctx.Count = &count

	output, _, ferr := MatchOutput(ctx, round)
	if ferr != nil {
		return nil, withBase(ferr, ctx)
	}

	status, stream := resolveOutputSettings(script.Script, output.Output, e.Loaded.Resolved.ScriptConfig, ctx.Stream)
	return &ExecutionResult{
		ScriptID:     scriptID,
		Protocol:     protocol,
		RequestID:    requestID,
		RequestCount: count,
		OutputCount:  ctx.OutputCount,
		RoundIndex:   round.Index,
		OutputIndex:  output.Index,
		Status:       status,
		Stream:       stream,
	}, nil
}

// bodyLimits 按剧本 config → server.config 默认值解析 body 上限与读取时限。
func (e *Executor) bodyLimits(script *model.Script) (int64, time.Duration) {
	limit := e.Loaded.Resolved.ScriptConfig.MaxBodyBytes
	timeout := e.Loaded.Resolved.ScriptConfig.ReadBodyTimeout
	if script != nil && script.Config != nil {
		if script.Config.MaxBodyBytes != nil {
			limit = *script.Config.MaxBodyBytes
		}
		if script.Config.ReadBodyTimeout != nil {
			timeout = script.Config.ReadBodyTimeout.D
		}
	}
	return limit, timeout
}

// readRequestBody 读取受 max_body_bytes 与 read_body_timeout 限制的 body。
// 读取在独立 goroutine 中执行，超时或客户端取消时主动关闭 body 让读取立即返回，
// 避免依赖底层连接的 read deadline 语义。
func readRequestBody(w http.ResponseWriter, r *http.Request, limit int64, timeout time.Duration) ([]byte, *model.FrameworkError) {
	type readResult struct {
		data []byte
		err  error
	}
	results := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		results <- readResult{data: data, err: err}
	}()

	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case result := <-results:
		if result.err != nil {
			var netErr net.Error
			if errors.As(result.err, &netErr) && netErr.Timeout() {
				return nil, model.NewFrameworkError(model.CodeBodyReadTimeout, "request body read timed out")
			}
			return nil, model.NewFrameworkError(model.CodeInvalidJSON, "cannot read request body: "+result.err.Error())
		}
		if int64(len(result.data)) > limit {
			return nil, model.NewFrameworkError(model.CodeBodyTooLarge, "request body exceeds max_body_bytes")
		}
		return result.data, nil
	case <-expired:
		// 不能用同步 Close：http.body.Close 会尝试把未到达的剩余 body 读空并阻塞。
		// 用过去的 read deadline 让挂起的 Read 立即返回，读取协程自行退出。
		abortRead(w)
		return nil, model.NewFrameworkError(model.CodeBodyReadTimeout, "request body read timed out")
	case <-r.Context().Done():
		abortRead(w)
		return nil, model.NewFrameworkError(model.CodeBodyReadTimeout, "request body read aborted")
	}
}

// abortRead 把连接的 read deadline 设为过去，让阻塞中的 body 读取立即失败。
func abortRead(w http.ResponseWriter) {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(-time.Second))
}

// runAssertions 顺序执行一个断言列表，失败时返回带诊断的 mock_assertion_failed。
func runAssertions(assertions []*CompiledAssertion, ctx *model.RequestContext, scope string) *model.FrameworkError {
	results, failed := EvaluateList(assertions, ctx)
	if failed == nil {
		return nil
	}
	ferr := model.NewFrameworkError(model.CodeAssertionFailed, "input assertion failed")
	details := BaseDetails(ctx)
	details["assertion"] = AssertionDetails(*failed)
	details["assertions"] = assertResultsDetails(results)
	ferr.Details = details
	return ferr
}

func assertResultsDetails(results []AssertionResult) []any {
	out := make([]any, 0, len(results))
	for _, result := range results {
		out = append(out, AssertionDetails(result))
	}
	return out
}

// withBase 把公共诊断字段合并进框架错误（不覆盖已有的同名诊断）。
func withBase(ferr *model.FrameworkError, ctx *model.RequestContext) *model.FrameworkError {
	if ferr.Details == nil {
		ferr.Details = map[string]any{}
	}
	for key, value := range BaseDetails(ctx) {
		if _, exists := ferr.Details[key]; !exists {
			ferr.Details[key] = value
		}
	}
	if _, exists := ferr.Details["request_id"]; !exists {
		ferr.Details["request_id"] = ctx.RequestID
	}
	return ferr
}

// resolveOutputSettings 按 output.config → 剧本 config → server.config → 内置默认值解析
// 期望的状态码与流式模式；B 只回显，不据此改变传输行为。
func resolveOutputSettings(script *model.Script, output *model.RoundOutput, defaults model.ScriptDefaults, requestStream bool) (int, bool) {
	status := defaults.Status
	streamMode := defaults.Stream
	if script != nil && script.Config != nil {
		if script.Config.Status != nil {
			status = *script.Config.Status
		}
		if script.Config.Stream != nil {
			streamMode = script.Config.Stream.Value
		}
	}
	if output != nil && output.Config != nil {
		if output.Config.Status != nil {
			status = *output.Config.Status
		}
		if output.Config.Stream != nil {
			streamMode = output.Config.Stream.Value
		}
	}
	switch streamMode {
	case constants.StreamTrue:
		return status, true
	case constants.StreamFalse:
		return status, false
	default:
		// auto：跟随请求的 stream 值。
		return status, requestStream
	}
}

func newRequestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "req_unknown"
	}
	return "req_" + hex.EncodeToString(buf)
}
