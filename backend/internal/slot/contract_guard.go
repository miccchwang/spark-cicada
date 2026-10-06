// contract_guard.go —— 出站契约守卫：让 G2/G3 的判定函数获得**真实生产调用点**。
//
// docs/05 G2/G3：
//
//	G2 所有可折叠层级默认收起（仅 L0 展开）；
//	G3 缺失即 null，绝不补 0；skipped 字段值必须为 null。
//
// 与 G4/G5 同病：`gate.CheckDefaultCollapsed` 与 `gate.CheckNoZeroImputation`
// 此前也没有任何非测试调用点 ⇒ 「默认收起」与「不补 0」两条硬契约，
// 在 Go 侧的出站路径上从没被检查过（前端 TS 有渲染层断言，但后端不设防）。
//
// 本文件提供 **MustValidContract**：任何要出站（写接口 / 落桶 / 交付）的
// DataContract 都必须先过这两条闸门，违规即拒（fail-closed）。
//
// ★ 诚实说明（避免把「恒放行」包装成「有断言」）：
//
//	本守卫是**出站校验**，不是「拒绝路径」——它的价值在于：
//	  ① 给 G2/G3 判定函数一个真实的、在生产路径上的调用点；
//	  ② 一旦上游真的产出违规契约（例如有人把 L2 默认展开、或给缺失值补了 0），
//	     出站会被拦下并给出可读原因，而不是把脏数据渲染到用户面前。
package slot

import (
	"fmt"
	"strings"

	"github.com/miccchwang/spark-cicada/backend/internal/contracts"
	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

// ContractGuard 出站契约守卫。
//
// zeroWhitelist 收纳「真实计算为 0」的字段（docs/05 G3：真实 0 需白名单）。
type ContractGuard struct {
	ZeroWhitelist map[string]bool
}

// NewContractGuard 构造守卫。
func NewContractGuard(zeroWhitelist map[string]bool) *ContractGuard {
	return &ContractGuard{ZeroWhitelist: zeroWhitelist}
}

// Validate 用 G2/G3 两条闸门校验契约，返回全部违规（空 = 合法）。
//
// ★ 这是 gate.CheckDefaultCollapsed / gate.CheckNoZeroImputation 的
// **真实生产调用点**（对象是运行期组装的 DataContract，而非测试夹具）。
func (g *ContractGuard) Validate(dc *contracts.DataContract) []string {
	if dc == nil {
		return []string{"DataContract 为 nil（不得出站空契约）"}
	}
	var violations []string

	// G2：默认收起。levels 为空是**允许**的（部分查询不带层级汇总），
	// 但只要带层级，就必须满足「仅 L0 展开」。
	if len(dc.Levels) > 0 {
		violations = append(violations, gate.CheckDefaultCollapsed(dc.Levels)...)
	}

	// G3：不补 0 / skipped 必须为 null。
	violations = append(violations, gate.CheckNoZeroImputation(dc, g.ZeroWhitelist)...)

	return violations
}

// MustValidContract 校验契约，违规即返回 error。
//
// 返回的错误消息是**可读的**（出站被拦时能直接定位字段与原因）。
func (g *ContractGuard) MustValidContract(dc *contracts.DataContract) error {
	if v := g.Validate(dc); len(v) > 0 {
		return fmt.Errorf("出站契约违反 G2/G3：\n  - %s", strings.Join(v, "\n  - "))
	}
	return nil
}
