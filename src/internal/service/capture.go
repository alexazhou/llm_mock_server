package service

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/alexazhou/llm_mock_server/internal/model"
)

// 本文件实现 capture 有界内存窗口（技术设计 §8）。
//
// 每条记录在请求进入流水线时分配进程内单调递增的 sequence；窗口同时受
// capacity 和 max_total_bytes 限制，加入记录后任一把上限超出时从最旧记录
// 开始淘汰。clear 只清空窗口，不回退 sequence，也不影响剧本 counter。
// 总字节只统计保存的 body、header 名称与值以及诊断预览，不含 Go 对象开销。

// capturePreviewBytes 是单条记录诊断预览的 UTF-8 字节上限（沿用 B 阶段诊断规则）。
const capturePreviewBytes = 4096

// CaptureError 是 capture 记录里的框架错误视图，不含 Go 类型名与调用栈。
type CaptureError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// CaptureRecord 是单次剧本请求的捕获记录，同时是 `GET /_test/requests` 的数组元素。
// 字段名按技术设计 §8 的清单：receivedAt 保持设计中的驼峰写法，其余为下划线。
type CaptureRecord struct {
	Sequence       uint64              `json:"sequence"`
	RequestID      string              `json:"request_id"`
	Method         string              `json:"method"`
	URL            string              `json:"url"`
	Headers        map[string]string   `json:"headers"`
	HeaderValues   map[string][]string `json:"header_values"`
	Body           string              `json:"body"`
	BodyTruncated  bool                `json:"body_truncated"`
	JSON           any                 `json:"json"`
	ReceivedAt     string              `json:"receivedAt"`
	ScriptID       string              `json:"script_id"`
	Protocol       string              `json:"protocol,omitempty"`
	RequestCount   *uint64             `json:"request_count"`
	OutputCount    *int                `json:"output_count"`
	RoundIndex     *int                `json:"round_index"`
	OutputIndex    *int                `json:"output_index"`
	Result         string              `json:"result"`
	FrameworkError *CaptureError       `json:"framework_error"`

	// 诊断面：断言/条件/步骤结果、实际写入字节数与结束原因。
	Assertions   []any  `json:"assertions"`
	Conditions   []any  `json:"conditions"`
	Steps        []any  `json:"steps"`
	WrittenBytes int64  `json:"written_bytes"`
	EndReason    string `json:"end_reason,omitempty"`
}

// Capture 是有界内存窗口。所有字段变更都在窗口锁内完成，调用方只持有记录指针，
// 不直接改写已入窗记录，避免并发读写。
type Capture struct {
	instanceID    string
	capacity      int
	maxBodyBytes  int64
	maxTotalBytes int64

	mu      sync.Mutex
	seq     uint64
	records []*CaptureRecord
	// evictedThrough 是已被淘汰的最大 sequence。cursor 落在它之前说明窗口缺过记录，
	// verify 必须 fail closed 返回 capture_window_lost。
	evictedThrough uint64
	// finishHook 在记录结束时收到一份快照（锁外调用），用于 JSONL 诊断。
	finishHook func(CaptureRecord)
}

// SetFinishHook 注册记录结束回调（单次启动期调用，之后只读）。
func (c *Capture) SetFinishHook(hook func(CaptureRecord)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finishHook = hook
}

// NewCapture 创建一个空窗口。配置校验已保证 capacity 与 max_total_bytes 为正整数，
// max_body_bytes 可以为 0（表示不保存请求 body）。
func NewCapture(instanceID string, capacity, maxBodyBytes, maxTotalBytes int64) *Capture {
	if capacity <= 0 {
		capacity = 1
	}
	if maxTotalBytes <= 0 {
		maxTotalBytes = 1
	}
	if maxBodyBytes < 0 {
		maxBodyBytes = 0
	}
	return &Capture{
		instanceID:    instanceID,
		capacity:      int(capacity),
		maxBodyBytes:  maxBodyBytes,
		maxTotalBytes: maxTotalBytes,
	}
}

// InstanceID 返回本进程的实例标识；cursor 只能用于同一 instance_id。
func (c *Capture) InstanceID() string {
	return c.instanceID
}

// MaxBodyBytes 返回单条记录保存 body 的字节上限。
func (c *Capture) MaxBodyBytes() int64 {
	return c.maxBodyBytes
}

// Begin 分配 sequence 并把记录放入窗口；此时只有请求面字段，body 与结果随后补齐。
func (c *Capture) Begin(rec *CaptureRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	rec.Sequence = c.seq
	if rec.ReceivedAt == "" {
		rec.ReceivedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	c.records = append(c.records, rec)
	c.trimLocked()
}

// Update 在窗口锁内改写一条记录，并重新做预算淘汰。所有对已入窗记录的写入都必须
// 走这里，否则 List/Verify 会在锁内读到并发写入的字段（race）。
func (c *Capture) Update(rec *CaptureRecord, update func(*CaptureRecord)) {
	if rec == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	update(rec)
	if c.containsLocked(rec) {
		c.trimLocked()
	}
}

// SetBody 记录请求 body（按 max_body_bytes 截断）与解析后的 JSON 预览。
func (c *Capture) SetBody(rec *CaptureRecord, body []byte, parsed any) {
	c.Update(rec, func(r *CaptureRecord) {
		if c.maxBodyBytes == 0 {
			r.Body = ""
		} else if int64(len(body)) > c.maxBodyBytes {
			r.Body = truncateUTF8(string(body), c.maxBodyBytes)
			r.BodyTruncated = true
		} else {
			r.Body = string(body)
		}
		r.JSON = previewValue(parsed)
	})
}

// Finish 补齐请求结果面：result、框架错误、实际写入字节与结束原因。
// 结束后在锁外把快照交给 finishHook（JSONL 诊断），避免持锁做 IO。
func (c *Capture) Finish(rec *CaptureRecord, result string, ferr *model.FrameworkError, written int64, endReason string) {
	if rec == nil {
		return
	}
	var snapshot CaptureRecord
	haveSnapshot := false
	c.Update(rec, func(r *CaptureRecord) {
		r.Result = result
		r.FrameworkError = captureErrorOf(ferr)
		r.WrittenBytes = written
		r.EndReason = endReason
		snapshot = cloneRecord(r)
		haveSnapshot = true
	})
	if !haveSnapshot {
		return
	}
	c.mu.Lock()
	hook := c.finishHook
	c.mu.Unlock()
	if hook != nil {
		hook(snapshot)
	}
}

// Cursor 返回当前窗口的 sequence 游标（最近分配的 sequence）。
func (c *Capture) Cursor() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq
}

// List 返回窗口内记录的副本，按 sequence 升序。
func (c *Capture) List() []CaptureRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CaptureRecord, 0, len(c.records))
	for _, rec := range c.records {
		out = append(out, cloneRecord(rec))
	}
	return out
}

// Clear 只清空窗口，不回退 sequence，也不影响剧本 counter。
func (c *Capture) Clear() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.records)
	c.records = nil
	return n
}

// CaptureVerifyReport 是 `POST /_test/capture/verify.json` 的响应。
type CaptureVerifyReport struct {
	InstanceID           string               `json:"instance_id"`
	Cursor               uint64               `json:"cursor"`
	NextCursor           uint64               `json:"next_cursor"`
	Records              int                  `json:"records"`
	FrameworkErrors      []CaptureVerifyError `json:"framework_errors"`
	AllowFrameworkErrors bool                 `json:"allow_framework_errors"`
	OK                   bool                 `json:"ok"`
}

// CaptureVerifyError 是 verify 报告里的一条框架错误摘要。
type CaptureVerifyError struct {
	Sequence  uint64 `json:"sequence"`
	RequestID string `json:"request_id"`
	ScriptID  string `json:"script_id"`
	Protocol  string `json:"protocol,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// Verify 校验 cursor 之后新增记录里的 Mock 框架错误。
//
// instanceID 必须是本进程的实例标识；cursor 落在已淘汰区间时 fail closed 返回
// capture_window_lost，避免把不完整窗口误判为完整。script_ids 为空表示全部剧本；
// allow_framework_errors 为 true 时豁免框架错误判定，仅用于负向用例。
func (c *Capture) Verify(instanceID string, cursor uint64, scriptIDs []string, allowFrameworkErrors bool) (*CaptureVerifyReport, *model.FrameworkError) {
	if instanceID != "" && instanceID != c.instanceID {
		ferr := model.NewFrameworkError(model.CodeCaptureInstanceMismatch,
			"capture cursor belongs to instance "+instanceID+", current instance is "+c.instanceID)
		ferr.WithDetails("instance_id", c.instanceID)
		ferr.WithDetails("cursor_instance_id", instanceID)
		return nil, ferr
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if cursor < c.evictedThrough {
		ferr := model.NewFrameworkError(model.CodeCaptureWindowLost,
			"capture records after cursor were evicted; the window is incomplete")
		ferr.WithDetails("instance_id", c.instanceID)
		ferr.WithDetails("cursor", cursor)
		ferr.WithDetails("evicted_through", c.evictedThrough)
		return nil, ferr
	}

	filter := map[string]bool{}
	for _, id := range scriptIDs {
		filter[id] = true
	}
	report := &CaptureVerifyReport{
		InstanceID:           c.instanceID,
		Cursor:               cursor,
		NextCursor:           c.seq,
		FrameworkErrors:      []CaptureVerifyError{},
		AllowFrameworkErrors: allowFrameworkErrors,
	}
	for _, rec := range c.records {
		if rec.Sequence <= cursor {
			continue
		}
		if len(filter) > 0 && !filter[rec.ScriptID] {
			continue
		}
		report.Records++
		if rec.FrameworkError == nil {
			continue
		}
		report.FrameworkErrors = append(report.FrameworkErrors, CaptureVerifyError{
			Sequence:  rec.Sequence,
			RequestID: rec.RequestID,
			ScriptID:  rec.ScriptID,
			Protocol:  rec.Protocol,
			Code:      rec.FrameworkError.Code,
			Message:   rec.FrameworkError.Message,
		})
	}
	report.OK = allowFrameworkErrors || len(report.FrameworkErrors) == 0
	return report, nil
}

// containsLocked 判断记录是否仍在窗口内（可能因淘汰已离开）。
func (c *Capture) containsLocked(rec *CaptureRecord) bool {
	for _, exist := range c.records {
		if exist == rec {
			return true
		}
	}
	return false
}

// totalLocked 重新统计窗口占用字节。窗口很小，逐条重算比增量维护更不容易出错。
func (c *Capture) totalLocked() int64 {
	var total int64
	for _, rec := range c.records {
		total += recordSize(rec)
	}
	return total
}

// trimLocked 按 capacity 与 max_total_bytes 淘汰最旧记录；单条记录在截断后仍超预算时，
// 改为保存一条小型 capture_record_too_large 记录，避免大 body 挤掉整个窗口。
func (c *Capture) trimLocked() {
	for len(c.records) > c.capacity {
		c.evictOldestLocked()
	}
	for len(c.records) > 0 && c.totalLocked() > c.maxTotalBytes {
		if len(c.records) == 1 {
			c.markTooLargeLocked(c.records[0])
			return
		}
		c.evictOldestLocked()
	}
}

// evictOldestLocked 淘汰最旧记录并推进 evictedThrough。
func (c *Capture) evictOldestLocked() {
	oldest := c.records[0]
	c.records = c.records[1:]
	if oldest.Sequence > c.evictedThrough {
		c.evictedThrough = oldest.Sequence
	}
}

// markTooLargeLocked 把单条超预算记录替换为小型框架错误记录。
// 若替换后仍超预算（max_total_bytes 极小），只能丢弃并继续 fail closed。
func (c *Capture) markTooLargeLocked(rec *CaptureRecord) {
	c.records[0] = &CaptureRecord{
		Sequence:   rec.Sequence,
		RequestID:  rec.RequestID,
		Method:     rec.Method,
		URL:        rec.URL,
		ReceivedAt: rec.ReceivedAt,
		ScriptID:   rec.ScriptID,
		Protocol:   rec.Protocol,
		Result:     "framework_error",
		FrameworkError: &CaptureError{
			Code:    model.CodeCaptureRecordTooLarge,
			Message: "capture record exceeds max_total_bytes after truncation",
		},
		Headers:      map[string]string{},
		HeaderValues: map[string][]string{},
		Assertions:   []any{},
		Conditions:   []any{},
		Steps:        []any{},
	}
	if c.totalLocked() > c.maxTotalBytes {
		c.evictOldestLocked()
	}
}

// recordSize 统计一条记录保存的 UTF-8 字节（body、header 名称与值、诊断预览）。
func recordSize(rec *CaptureRecord) int64 {
	if rec == nil {
		return 0
	}
	total := int64(len(rec.Body) + len(rec.Result) + len(rec.EndReason) + len(rec.URL) + len(rec.RequestID))
	for name, value := range rec.Headers {
		total += int64(len(name) + len(value))
	}
	for name, values := range rec.HeaderValues {
		total += int64(len(name))
		for _, value := range values {
			total += int64(len(value))
		}
	}
	if preview, err := json.Marshal(rec.JSON); err == nil {
		total += int64(len(preview))
	}
	if rec.FrameworkError != nil {
		if preview, err := json.Marshal(rec.FrameworkError); err == nil {
			total += int64(len(preview))
		}
	}
	for _, list := range [][]any{rec.Assertions, rec.Conditions, rec.Steps} {
		if preview, err := json.Marshal(list); err == nil {
			total += int64(len(preview))
		}
	}
	return total
}

// previewValue 把诊断值限制在 capturePreviewBytes 内，超限时保留截断标记与文本前缀。
func previewValue(value any) any {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return map[string]any{"truncated": true, "preview": err.Error()}
	}
	if len(encoded) <= capturePreviewBytes {
		return value
	}
	return map[string]any{"truncated": true, "preview": truncateUTF8(string(encoded), capturePreviewBytes)}
}

// captureErrorOf 把框架错误转成记录里的稳定视图。
func captureErrorOf(ferr *model.FrameworkError) *CaptureError {
	if ferr == nil {
		return nil
	}
	return &CaptureError{Code: ferr.Code, Message: ferr.Message, Details: previewDetails(ferr.Details)}
}

// previewDetails 限制诊断详情大小，避免单条记录把窗口挤满。
func previewDetails(details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	encoded, err := json.Marshal(details)
	if err != nil || len(encoded) <= capturePreviewBytes {
		return details
	}
	return map[string]any{"truncated": true, "preview": truncateUTF8(string(encoded), capturePreviewBytes)}
}

// cloneRecord 复制记录，避免调用方在锁外读到并发修改。
func cloneRecord(rec *CaptureRecord) CaptureRecord {
	out := *rec
	out.Headers = cloneStringMap(rec.Headers)
	out.HeaderValues = cloneStringSliceMap(rec.HeaderValues)
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneStringSliceMap(in map[string][]string) map[string][]string {
	if in == nil {
		return nil
	}
	out := make(map[string][]string, len(in))
	for key, values := range in {
		copied := make([]string, len(values))
		copy(copied, values)
		out[key] = copied
	}
	return out
}

// truncateUTF8 按 UTF-8 字节上限截断，保证不切断多字节字符。
func truncateUTF8(text string, limit int64) string {
	if int64(len(text)) <= limit {
		return text
	}
	cut := int(limit)
	for cut > 0 && !utf8Start(text[cut]) {
		cut--
	}
	return text[:cut]
}

// utf8Start 判断字节是否是 UTF-8 字符的首字节（续字节形如 10xxxxxx）。
func utf8Start(b byte) bool {
	return b&0xC0 != 0x80
}
