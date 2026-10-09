// mem_failover.go —— 见证者 / LSN 对账账本的**进程内实现**。
//
// 用途与 memstore.go 相同：本地开发、单元测试、以及**没有真实 etcd/Redis
// 见证者与 LSN 对账源时也能跑通的链路闸门**。生产装配须替换为
// 第三可用区的见证者（docs/11 F13 脑裂防护）与真实 LSN 对账源。
//
// ★ 诚实边界：这两个实现的「已取得 / 已对账」状态**必须被显式置位**
//
//	（Acquire / MarkReconciled），默认一律为 false。
//	若把默认写成 true，`CheckFailoverFencing` 的三项里有两项会退化为装饰 ——
//	那正是本仓反复踩的「恒真闸门」。
package dr

import "sync"

// MemWitness 进程内见证者（开发/单测用）。
type MemWitness struct {
	mu   sync.Mutex
	held map[Region]bool
}

// NewMemWitness 构造进程内见证者（默认**未取得**）。
func NewMemWitness() *MemWitness {
	return &MemWitness{held: map[Region]bool{}}
}

// Acquired 实现 Witness；未显式 Acquire 过一律 false（fail-closed）。
func (w *MemWitness) Acquired(r Region) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.held[r]
}

// Acquire 取得某地域的见证租约（仅由隔离/切换流程调用）。
func (w *MemWitness) Acquire(r Region) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.held[r] = true
}

// Release 释放见证租约。
func (w *MemWitness) Release(r Region) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.held, r)
}

// MemLSNLedger 进程内 LSN 对账账本（开发/单测用）。
type MemLSNLedger struct {
	mu   sync.Mutex
	done map[Region]bool
}

// NewMemLSNLedger 构造进程内 LSN 账本（默认**未对账**）。
func NewMemLSNLedger() *MemLSNLedger {
	return &MemLSNLedger{done: map[Region]bool{}}
}

// Reconciled 实现 LSNLedger；未显式 MarkReconciled 过一律 false（fail-closed）。
func (l *MemLSNLedger) Reconciled(r Region) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.done[r]
}

// MarkReconciled 标记某地域已按 LSN/提交序号对账完毕。
func (l *MemLSNLedger) MarkReconciled(r Region) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.done[r] = true
}
