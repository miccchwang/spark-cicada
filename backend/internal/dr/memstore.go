// memstore.go —— 内存实现。
//
// 用途：本地开发、单元测试、以及**没有数据库时也要能跑的链路闸门**。
// 生产装配接 Postgres（用 `ON CONFLICT ... WHERE used < limit` 保证原子），
// 但本实现的**语义**与生产一致：分地域 + 分账号 + 按自然月计数。
package dr

import (
	"context"
	"sync"
)

// MemQuotaStore 内存版 QuotaStore。
type MemQuotaStore struct {
	mu       sync.Mutex
	used     map[string]int
	archives map[string]Archive // key = archiveKey(region, id)
}

// NewMemQuotaStore 构造内存配额存储。
func NewMemQuotaStore() *MemQuotaStore {
	return &MemQuotaStore{
		used:     map[string]int{},
		archives: map[string]Archive{},
	}
}

func archiveKey(region Region, id string) string { return string(region) + "/" + id }

// PutArchive 登记一份归档（测试/开发用）。
func (m *MemQuotaStore) PutArchive(a Archive) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.archives[archiveKey(a.Region, a.ID)] = a
}

// SetUsed 直接设置已用次数（测试用）。
func (m *MemQuotaStore) SetUsed(account string, region Region, period string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.used[quotaKey(account, region, period)] = n
}

// Used 实现 QuotaStore。
func (m *MemQuotaStore) Used(_ context.Context, account string, region Region, period string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used[quotaKey(account, region, period)], nil
}

// ConsumeOnce 实现 QuotaStore（互斥锁保证原子）。
func (m *MemQuotaStore) ConsumeOnce(_ context.Context, account string, region Region,
	period string, limit int) (bool, int, error) {

	m.mu.Lock()
	defer m.mu.Unlock()
	k := quotaKey(account, region, period)
	cur := m.used[k]
	if cur >= limit {
		return false, cur, nil
	}
	m.used[k] = cur + 1
	return true, cur + 1, nil
}

// Archive 实现 QuotaStore。
func (m *MemQuotaStore) Archive(_ context.Context, region Region, id string) (Archive, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.archives[archiveKey(region, id)]
	return a, ok, nil
}
