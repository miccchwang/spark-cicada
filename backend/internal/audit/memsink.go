// memsink.go —— 内存实现。
//
// 用途：本地开发、单元测试、以及**没有数据库时也要能跑的链路闸门**。
// 生产装配接 Postgres（`store.Admin` 的 `pgAuditSink`），
// 但本实现的**语义**与生产一致：只增不改、可计数。
//
// ★ 与生产一致的一点很关键：它**也**只提供 Append/Count，
//   没有任何 Update/Delete —— 链路闸门若在内存实现上通过，接生产只需换 Sink。
package audit

import (
	"context"
	"sync"
)

// MemSink 内存版 Sink（并发安全）。
type MemSink struct {
	mu      sync.Mutex
	entries []Entry
}

// NewMemSink 构造内存审计存储。
func NewMemSink() *MemSink { return &MemSink{} }

// Append 追加一条审计。
func (s *MemSink) Append(_ context.Context, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// ★ 拷贝一份 Detail：调用方随后改自己的 map 不应改到已落库的审计。
	cp := e
	if e.Detail != nil {
		d := make(map[string]any, len(e.Detail))
		for k, v := range e.Detail {
			d[k] = v
		}
		cp.Detail = d
	}
	s.entries = append(s.entries, cp)
	return nil
}

// Count 返回已追加的审计行数。
func (s *MemSink) Count(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries), nil
}

// Entries 返回已落库审计的副本（只读；测试用）。
func (s *MemSink) Entries() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.entries))
	copy(out, s.entries)
	return out
}
