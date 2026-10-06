package query

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/compute"
	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
)

// ══════════════════════════════════════════════════════════════════════════
// M-QUERY 缓存的租户隔离（Task #57）
//
// ★★ 本文件的主题只有一个：**缓存键必须含租户**。
//
//	这是多租户里最容易被漏掉的一层，因为：
//	  · 它不涉及 SQL，评审时没人会想到去查缓存；
//	  · 它的失效形态是「命中别人的条目」，而命中是**正常路径**，
//	    没有任何异常、日志或指标会显示异常；
//	  · 单租户测试永远绿 —— 只有「两家租户发同样查询」才暴露。
//
// ★ 因此本文件用一个**记录型假缓存**（recordingCache）把「键长什么样」
//   变成可断言的事实，而不是靠人去读实现。
// ══════════════════════════════════════════════════════════════════════════

const (
	tidAlpha = "aaaa1111-1111-4111-8111-111111111111"
	tidBeta  = "bbbb2222-2222-4222-8222-222222222222"
)

// recordingCache 是记录所有键的假缓存。
//
// ★ 用 map 而不是「真 Redis」：本文件要断言的是**键的形状**，
//	用一个能回放的假实现比连真 Redis 更能直接证明问题。
//	（真 Redis 的连通性测试属于集成测试，不在本文件。）
type recordingCache struct {
	mu      sync.Mutex
	data    map[string]*ResultSet
	getKeys []string
	setKeys []string
}

func newRecordingCache() *recordingCache {
	return &recordingCache{data: map[string]*ResultSet{}}
}

func (c *recordingCache) Get(_ context.Context, key string) (*ResultSet, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getKeys = append(c.getKeys, key)
	rs, ok := c.data[key]
	return rs, ok
}

func (c *recordingCache) Set(_ context.Context, key string, rs *ResultSet, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setKeys = append(c.setKeys, key)
	c.data[key] = rs
	return nil
}

func (c *recordingCache) lastGet() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.getKeys) == 0 {
		return ""
	}
	return c.getKeys[len(c.getKeys)-1]
}

func (c *recordingCache) lastSet() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.setKeys) == 0 {
		return ""
	}
	return c.setKeys[len(c.setKeys)-1]
}

// stubStore 提供最小的 Store 实现，让 Run 能走完。
type stubStore struct{}

func (stubStore) SelectBucket(context.Context, string, contracts.QueryState) ([]BucketRow, error) {
	return nil, nil
}
func (stubStore) BucketState(context.Context, string) (BucketMeta, error) {
	return BucketMeta{ID: "pnl_month", State: "FRESH"}, nil
}
func (stubStore) SlotHealth(context.Context, []string) (map[string]SlotStatus, error) {
	return map[string]SlotStatus{}, nil
}

func testOrch(c Cache) *Orchestrator {
	return &Orchestrator{
		Store:  stubStore{},
		Cache:  c,
		Kernel: compute.Kernel(nil),
		Policy: Policy{
			CacheTTL:      time.Minute,
			BucketAlgoMap: map[string]map[string]string{},
		},
	}
}

// monthQuery 是与租户无关的查询（同租户/跨租户内容完全相同）。
func monthQuery() contracts.QueryState {
	return contracts.QueryState{
		Time: contracts.TimeRange{Grain: "month", From: "2026-01-01", To: "2026-01-31"},
	}
}

// TestRunInTenant_缓存键必须含租户 是最核心的一条。
func TestRunInTenant_缓存键必须含租户(t *testing.T) {
	c := newRecordingCache()
	o := testOrch(c)

	if _, err := o.RunInTenant(context.Background(), tidAlpha, monthQuery()); err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	key := c.lastSet()
	if key == "" {
		t.Fatal("应在缓存中写入结果")
	}
	if !hasPrefix(key, tidAlpha) {
		t.Fatalf("★ 缓存键应以租户 ID 开头，实际 %q", key)
	}
}

// TestRunInTenant_两租户同查询不同键 钉住「缓存串租」这一缺陷类别。
//
// ★ 这条测试是 Task #57 的验收核心：
//
//	两家租户发**完全相同**的 QueryState。
//	若键只由查询哈希构成 ⇒ 第二次查询会**命中第一次的条目**
//	⇒ Beta 读到 Alpha 的结果。本测试断言两键不同，且第二个租户
//	**没有命中**（缓存里那份是别人的，不算命中）。
func TestRunInTenant_两租户同查询不同键(t *testing.T) {
	c := newRecordingCache()
	o := testOrch(c)
	ctx := context.Background()

	if _, err := o.RunInTenant(ctx, tidAlpha, monthQuery()); err != nil {
		t.Fatal(err)
	}
	alphaKey := c.lastSet()

	if _, err := o.RunInTenant(ctx, tidBeta, monthQuery()); err != nil {
		t.Fatal(err)
	}
	betaGet := c.lastGet()
	betaKey := c.lastSet()

	if alphaKey == betaKey {
		t.Fatalf("★ 两租户对同一查询产生了同一缓存键 %q —— 会串租", alphaKey)
	}
	if betaGet == alphaKey {
		t.Fatalf("★ Beta 去查了 Alpha 的缓存键 %q —— 跨租户命中", betaGet)
	}
	if !hasPrefix(betaKey, tidBeta) {
		t.Errorf("Beta 的键应以 Beta 租户开头，实际 %q", betaKey)
	}
}

// TestRunInTenant_同租户重复查询命中缓存 保证隔离没有把缓存一起废掉。
//
// ★ 作用：防止「为了安全把缓存键加上随机数」这种过度修复 ——
//	那会让隔离「看起来对了」，代价是缓存命中率归零。
//	安全的做法是「按租户分区」，不是「禁用缓存」。
func TestRunInTenant_同租户重复查询命中缓存(t *testing.T) {
	c := newRecordingCache()
	o := testOrch(c)
	ctx := context.Background()

	if _, err := o.RunInTenant(ctx, tidAlpha, monthQuery()); err != nil {
		t.Fatal(err)
	}
	first := c.lastSet()

	rs, err := o.RunInTenant(ctx, tidAlpha, monthQuery())
	if err != nil {
		t.Fatal(err)
	}
	if rs == nil {
		t.Fatal("应返回结果")
	}
	// 第二次应命中第一次写入的键，且不再写新键
	if got := c.lastGet(); got != first {
		t.Errorf("同租户重查应命中同一键：get=%q set=%q", got, first)
	}
	if setCount(c) != 1 {
		t.Errorf("同租户重查不应产生新的写入，实际写入 %d 次", setCount(c))
	}
}

// TestRun_无租户时不得使用缓存 钉住 fail-closed。
//
// ★ 若 Run（非租户版）也读写缓存，则「平台级调用」会与租户查询
//	共享命名空间 —— 一条未标租户的键可能与某租户的键碰撞。
//	正确行为：无租户 ⇒ **完全不碰缓存**（每次穿透数据源）。
func TestRun_无租户时不得使用缓存(t *testing.T) {
	c := newRecordingCache()
	o := testOrch(c)

	if _, err := o.Run(context.Background(), monthQuery()); err != nil {
		t.Fatal(err)
	}
	if getCount(c) != 0 || setCount(c) != 0 {
		t.Fatalf("★ 无租户查询不得触碰缓存，实际 get=%d set=%d",
			getCount(c), setCount(c))
	}
}

// TestRunInTenant_空租户等同无租户 钉住「空串不被当作合法租户」。
func TestRunInTenant_空租户等同无租户(t *testing.T) {
	c := newRecordingCache()
	o := testOrch(c)

	if _, err := o.RunInTenant(context.Background(), "", monthQuery()); err != nil {
		t.Fatal(err)
	}
	if getCount(c) != 0 || setCount(c) != 0 {
		t.Fatalf("★ 空租户不得触碰缓存，实际 get=%d set=%d", getCount(c), setCount(c))
	}
}

// TestRunInTenant_queryHash仍与租户无关 钉住「租户不进哈希」的纪律。
//
// ★ 为什么租户不能进 QueryState 的哈希：
//	QueryState 是前端契约，哈希是「查询内容的指纹」。
//	若租户进了哈希，则「相同 QueryState ⇒ 相同 queryHash」这个
//	G1 闸门不变量会被破坏（同内容不同租户会有不同哈希），
//	且契约会通过镜像机制把「租户字段」泄漏到前端。
//	本测试断言同内容跨租户的 QueryHash 相同 —— 隔离靠**键前缀**，不靠哈希。
func TestRunInTenant_queryHash仍与租户无关(t *testing.T) {
	c := newRecordingCache()
	o := testOrch(c)
	ctx := context.Background()

	rsA, err := o.RunInTenant(ctx, tidAlpha, monthQuery())
	if err != nil {
		t.Fatal(err)
	}
	rsB, err := o.RunInTenant(ctx, tidBeta, monthQuery())
	if err != nil {
		t.Fatal(err)
	}
	if rsA.QueryHash != rsB.QueryHash {
		t.Errorf("★ 同内容跨租户的 QueryHash 应相同（隔离靠键前缀，不靠哈希）：%q vs %q",
			rsA.QueryHash, rsB.QueryHash)
	}
	if rsA.QueryHash == "" {
		t.Error("QueryHash 不应为空")
	}
}

// ───────────────────────────── 辅助 ─────────────────────────────

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func getCount(c *recordingCache) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.getKeys)
}

func setCount(c *recordingCache) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.setKeys)
}
