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

	"github.com/alexazhou/llm_mock_server/internal/model"
)

// Executor 执行 B 阶段的完整决策流水线：
// 容量控制 → 读取 body → 严格 JSON/UTF-8 → 协议解析 → round 定位 →
// 输入断言 → 分配 request.count → 匹配 output。
type Executor struct {
	Loaded   *Loaded
	Counters *CounterStore
	Capacity *Capacity
	// Capture 是 capture 窗口；nil 表示不记录（单元测试可以省略）。
	Capture *Capture
	// InstanceID 是本进程的实例标识，同时用于 capture cursor 校验。
	InstanceID string
}

// NewExecutor 基于加载结果构建执行器，并创建本进程的 capture 窗口。
func NewExecutor(loaded *Loaded) *Executor {
	instanceID := newInstanceID()
	return &Executor{
		Loaded:     loaded,
		Counters:   NewCounterStore(loaded.Registry),
		Capacity:   NewCapacity(loaded.Resolved.MaxActiveRequests),
		Capture:    NewCapture(instanceID, loaded.Resolved.CaptureCapacity, loaded.Resolved.CaptureMaxBodyBytes, loaded.Resolved.CaptureMaxTotalBytes),
		InstanceID: instanceID,
	}
}

// Execution 持有一次请求的活动租约：全局容量与剧本 active_requests 一直持有到
// 响应写完（含 wait 挂起与 abort 断连），调用方必须在 WriteOutput 之后 Release。
// 技术设计 §5.2 要求「body 读取、输入校验、有限/无限 wait 和 Hijack 连接存活
// 期间都算活动请求」，因此不能在 Execute 返回时就释放。
type Execution struct {
	*ExecutionResult
	recorder *requestRecorder
	release  func()
	released bool
}

// Release 释放容量与 active_requests；重复调用无副作用。
func (x *Execution) Release() {
	if x == nil || x.released {
		return
	}
	x.released = true
	if x.release != nil {
		x.release()
	}
}

// Finish 在响应写完后补齐 capture 记录的写入字节、结果分类与结束原因。
func (x *Execution) Finish(outcome WriteOutcome) {
	if x == nil || x.recorder == nil {
		return
	}
	result := "output"
	endReason := "completed"
	switch {
	case outcome.Aborted:
		result = "aborted"
		endReason = "abort"
	case outcome.Err != nil:
		result = "client_cancel"
		endReason = "client_disconnect"
	}
	x.recorder.finish(result, nil, outcome.Written, endReason)
}

// ExecutionResult 是 B 阶段成功决策的结果，用于组装响应。
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
	// Prepared 是 prepare 阶段解析出的输出快照，交给 WriteOutput 编码。
	Prepared *PreparedOutput
}

// Execute 处理一次剧本请求。成功时返回持有活动租约的 Execution，调用方必须写完
// 响应后 Release；失败时返回框架错误，capture 记录已结束且租约已释放。
func (e *Executor) Execute(w http.ResponseWriter, r *http.Request, scriptID, protocol string) (*Execution, *model.FrameworkError) {
	requestID := newRequestID()
	recorder := newRequestRecorder(e.Capture, r, requestID, scriptID, protocol)

	script, ok := e.Loaded.Compiled[scriptID]
	if !ok {
		ferr := model.NewFrameworkError(model.CodeScriptNotFound, "script "+scriptID+" is not registered")
		recorder.finish("framework_error", ferr, 0, "script_not_found")
		return nil, ferr
	}

	// 容量控制必须在读取 body 之前：满则直接 503，不分配 request.count。
	// 容量失败同样记入 capture 并标记为 framework error（技术设计 §5.3）。
	if !e.Capacity.TryAcquire() {
		ferr := model.NewFrameworkError(model.CodeCapacityExceeded, "too many active requests")
		ferr.WithDetails("max_active_requests", e.Loaded.Resolved.MaxActiveRequests)
		ferr.WithDetails("script_id", scriptID)
		ferr.WithDetails("protocol", protocol)
		ferr.WithDetails("request_id", requestID)
		recorder.finish("framework_error", ferr, 0, "capacity_exceeded")
		return nil, ferr
	}

	counters := e.Counters.For(scriptID)
	if counters == nil {
		counters = NewScriptCounters()
	}
	counters.Acquire()

	execution := &Execution{recorder: recorder}
	execution.release = func() {
		counters.Release()
		e.Capacity.Release()
	}
	// 失败路径统一收口：结束 capture 记录、释放租约后再返回。
	fail := func(ferr *model.FrameworkError) (*Execution, *model.FrameworkError) {
		recorder.finish("framework_error", ferr, 0, ferr.Code)
		execution.Release()
		return nil, ferr
	}

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
		return fail(withBase(ferr, ctx))
	}
	recorder.setBody(body, nil)
	ctx.BodyText = string(body)
	if !utf8.Valid(body) {
		return fail(withBase(model.NewFrameworkError(model.CodeInvalidJSON,
			"request body must be valid UTF-8"), ctx))
	}

	parsed, ferr := ParseProtocolRequest(protocol, body)
	if ferr != nil {
		return fail(withBase(ferr, ctx))
	}
	recorder.setBody(body, parsed.JSONBody)
	ctx.JSONBody = parsed.JSONBody
	ctx.Model = parsed.Model
	ctx.Stream = parsed.Stream
	ctx.Parsed = true
	ctx.OutputCount = parsed.OutputCount
	ctx.RoundInput = parsed.RoundInput

	round, ferr := LocateRound(script, ctx.OutputCount)
	if ferr != nil {
		recorder.setOutputCount(ctx.OutputCount, 0)
		return fail(withBase(ferr, ctx))
	}
	ctx.RoundIndex = round.Index
	recorder.setOutputCount(ctx.OutputCount, round.Index)

	results, ferr := runAssertions(script.Assert, ctx, "assert")
	recorder.setAssertions(results)
	if ferr != nil {
		return fail(ferr)
	}
	results, ferr = runAssertions(round.InputAssert, ctx, "round.input.assert")
	recorder.setAssertions(results)
	if ferr != nil {
		return fail(ferr)
	}

	// 输入断言全部通过后才分配 request.count。
	count := counters.Next(protocol)
	ctx.Count = &count
	recorder.setRequestCount(count)

	output, _, ferr := MatchOutput(ctx, round)
	if ferr != nil {
		return fail(withBase(ferr, ctx))
	}
	recorder.setOutputIndex(output.Index)

	prepared, ferr := PrepareOutput(ctx, output, e.Loaded.Resolved.MaxOutputBytes)
	if ferr != nil {
		return fail(ferr)
	}
	execution.ExecutionResult = &ExecutionResult{
		ScriptID:     scriptID,
		Protocol:     protocol,
		RequestID:    requestID,
		RequestCount: count,
		OutputCount:  ctx.OutputCount,
		RoundIndex:   round.Index,
		OutputIndex:  output.Index,
		Status:       prepared.Status,
		Stream:       prepared.Stream,
		Prepared:     prepared,
	}
	return execution, nil
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

// runAssertions 顺序执行一个断言列表，返回逐条结果；失败时同时返回带诊断的
// mock_assertion_failed（结果仍可用于 capture 记录）。
func runAssertions(assertions []*CompiledAssertion, ctx *model.RequestContext, scope string) ([]AssertionResult, *model.FrameworkError) {
	results, failed := EvaluateList(assertions, ctx)
	if failed == nil {
		return results, nil
	}
	ferr := model.NewFrameworkError(model.CodeAssertionFailed, "input assertion failed")
	details := BaseDetails(ctx)
	details["assertion"] = AssertionDetails(*failed)
	details["assertions"] = assertResultsDetails(results)
	ferr.Details = details
	return results, ferr
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

func newRequestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "req_unknown"
	}
	return "req_" + hex.EncodeToString(buf)
}

// newInstanceID 生成本进程的实例标识；capture cursor 只在同一实例内有效。
func newInstanceID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "ins_unknown"
	}
	return "ins_" + hex.EncodeToString(buf)
}
