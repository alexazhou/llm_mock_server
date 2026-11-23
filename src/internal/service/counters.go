package service

import (
	"sync"

	"github.com/alexazhou/llm_mock_server/internal/constants"
	"github.com/alexazhou/llm_mock_server/internal/model"
)

// ScriptCounters 是一个剧本的运行期计数状态：三个协议各自独立的 request.count，
// 以及活动请求数。准入与 reset 共用一把短锁，保证 active==0 的检查与归零之间
// 不会插入新请求；锁内不做任何 IO 或等待。
type ScriptCounters struct {
	mu     sync.Mutex
	counts map[string]uint64
	active int64
}

// NewScriptCounters 创建一个三个协议都从 0 开始的计数状态。
func NewScriptCounters() *ScriptCounters {
	return &ScriptCounters{counts: map[string]uint64{
		constants.ProtocolOpenAI:    0,
		constants.ProtocolAnthropic: 0,
		constants.ProtocolResponses: 0,
	}}
}

// Acquire 在准入临界区内递增活动请求数。
func (c *ScriptCounters) Acquire() {
	c.mu.Lock()
	c.active++
	c.mu.Unlock()
}

// Release 在请求所有退出路径上递减活动请求数。
func (c *ScriptCounters) Release() {
	c.mu.Lock()
	if c.active > 0 {
		c.active--
	}
	c.mu.Unlock()
}

// Active 返回当前活动请求数。
func (c *ScriptCounters) Active() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

// Next 原子分配并返回该协议的下一个 request.count（从 1 开始）。
func (c *ScriptCounters) Next(protocol string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[protocol]++
	return c.counts[protocol]
}

// Counts 返回三个协议的当前计数副本。
func (c *ScriptCounters) Counts() map[string]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]uint64, len(c.counts))
	for protocol, count := range c.counts {
		out[protocol] = count
	}
	return out
}

// Reset 在没有活动请求时把三个协议的计数一起归零；有活动请求时返回 false 且不部分重置。
func (c *ScriptCounters) Reset() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active > 0 {
		return false
	}
	for protocol := range c.counts {
		c.counts[protocol] = 0
	}
	return true
}

// CounterStore 按剧本 id 保存计数状态。
type CounterStore struct {
	mu      sync.RWMutex
	scripts map[string]*ScriptCounters
}

// NewCounterStore 为注册表中的每个剧本预建计数状态。
func NewCounterStore(reg *model.Registry) *CounterStore {
	store := &CounterStore{scripts: map[string]*ScriptCounters{}}
	for _, entry := range reg.Entries() {
		store.scripts[entry.Script.ID] = NewScriptCounters()
	}
	return store
}

// For 返回指定剧本的计数状态；未知剧本返回 nil。
func (s *CounterStore) For(id string) *ScriptCounters {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.scripts[id]
}

// Capacity 是全局 max_active_requests 的有界信号量，在读取 body 之前尝试获取。
type Capacity struct {
	sem chan struct{}
}

// NewCapacity 创建容量为 max 的信号量；max <= 0 视为 1，避免配置错误导致永久拒绝。
func NewCapacity(max int64) *Capacity {
	if max <= 0 {
		max = 1
	}
	return &Capacity{sem: make(chan struct{}, int(max))}
}

// TryAcquire 非阻塞地获取一个容量；已满返回 false。
func (c *Capacity) TryAcquire() bool {
	select {
	case c.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release 归还容量。
func (c *Capacity) Release() {
	select {
	case <-c.sem:
	default:
	}
}

// Active 返回已占用的容量数。
func (c *Capacity) Active() int {
	return len(c.sem)
}
