package chain_test

import (
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/chain"
)

// 构造一棵测试组织树：
//
//	T1 (ceo)   dept=总公司
//	 ├─ T2 (alice) dept=运营部
//	 │   ├─ T3 (bob)   dept=运营部   ← 申请人
//	 │   └─ T3 (carol) dept=运营部
//	 └─ T2 (dave) dept=财务部
//	     └─ T3 (erin) dept=财务部
func buildOrg() *chain.OrgDirectory {
	d := chain.NewOrgDirectory()
	d.Put(&chain.OrgNode{Account: "ceo", Tier: "T1", PrimaryDept: "总公司", Active: true, CanApprove: true})
	d.Put(&chain.OrgNode{Account: "alice", Tier: "T2", Supervisor: "ceo", PrimaryDept: "运营部",
		DeptSource: chain.Derived, Active: true, CanApprove: true})
	d.Put(&chain.OrgNode{Account: "bob", Tier: "T3", Supervisor: "alice", PrimaryDept: "运营部",
		DeptSource: chain.Derived, Active: true, CanApprove: true,
		// F9：虚线汇报给财务 dave（仅抄送）
		DottedLineSupervisors: []string{"dave"}})
	d.Put(&chain.OrgNode{Account: "carol", Tier: "T3", Supervisor: "alice", PrimaryDept: "运营部",
		DeptSource: chain.Derived, Active: true, CanApprove: true})
	d.Put(&chain.OrgNode{Account: "dave", Tier: "T2", Supervisor: "ceo", PrimaryDept: "财务部",
		DeptSource: chain.Derived, Active: true, CanApprove: true})
	d.Put(&chain.OrgNode{Account: "erin", Tier: "T3", Supervisor: "dave", PrimaryDept: "财务部",
		DeptSource: chain.Derived, Active: true, CanApprove: true})
	return d
}

// F9：审批链只走主属（+1 = alice），虚线上级（dave）仅进抄送。
func TestF9_ApprovalFollowsPrimaryOnly(t *testing.T) {
	d := buildOrg()
	ac, err := d.Route(chain.RouteInput{Applicant: "bob"}, chain.DefaultCcPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if ac.Approver != "alice" {
		t.Fatalf("F9: approver must be primary supervisor alice, got %s", ac.Approver)
	}
	for _, cc := range ac.CcList {
		if cc == "dave" {
			t.Fatalf("F9: dotted-line (dave) must NOT be in approver/cc decision list")
		}
	}
	foundDotted := false
	for _, cc := range ac.DottedLineCc {
		if cc == "dave" {
			foundDotted = true
		}
	}
	if !foundDotted {
		t.Fatalf("F9: dotted-line supervisor should appear in DottedLineCc, got %v", ac.DottedLineCc)
	}
	// 虚线抄送必须标记为 notify（不入审批）
	for _, rec := range ac.CcRecords {
		if rec.Cc == "dave" && rec.Mode != chain.CcNotify {
			t.Fatalf("F9: dotted-line cc must be notify-only, got %s", rec.Mode)
		}
	}
}

// A2：+1 审批；+2 抄送（approver 的上级 = ceo）。
func TestA2_PlusTwoCc(t *testing.T) {
	d := buildOrg()
	ac, err := d.Route(chain.RouteInput{Applicant: "bob"}, chain.DefaultCcPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if ac.CcRule != "PLUS_TWO" {
		t.Fatalf("expected PLUS_TWO, got %s", ac.CcRule)
	}
	hasCeo := false
	for _, cc := range ac.CcList {
		if cc == "ceo" {
			hasCeo = true
		}
	}
	if !hasCeo {
		t.Fatalf("+2 (ceo) should be cc'd, got %v", ac.CcList)
	}
}

// F8：L4 触发会签（有否决权），模式应为 cosign。
func TestF8_L4TriggersCosign(t *testing.T) {
	d := buildOrg()
	ac, err := d.Route(chain.RouteInput{Applicant: "bob", DraftLevel: "L4"}, chain.DefaultCcPolicy())
	if err != nil {
		t.Fatal(err)
	}
	sawCosign := false
	for _, rec := range ac.CcRecords {
		if rec.Reason == "L4" && rec.Mode == chain.CcCosign {
			sawCosign = true
		}
	}
	if !sawCosign {
		t.Fatalf("L4 should trigger cosign, records=%+v", ac.CcRecords)
	}
}

// F8：含 grp.roi 触发会签。
func TestF8_ROIGroupTriggersCosign(t *testing.T) {
	d := buildOrg()
	ac, err := d.Route(chain.RouteInput{
		Applicant:   "bob",
		DraftGroups: []string{"grp.roi"},
	}, chain.DefaultCcPolicy())
	if err != nil {
		t.Fatal(err)
	}
	saw := false
	for _, rec := range ac.CcRecords {
		if rec.Mode == chain.CcCosign {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("grp.roi should trigger cosign")
	}
}

// F8：默认知会（普通申请，无高风险触发）。
func TestF8_DefaultNotify(t *testing.T) {
	d := buildOrg()
	ac, err := d.Route(chain.RouteInput{Applicant: "bob", DraftLevel: "L2"}, chain.DefaultCcPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range ac.CcRecords {
		if rec.Mode != chain.CcNotify {
			t.Fatalf("normal request should be notify-only, got %s", rec.Mode)
		}
	}
}

// F9/A1：跨部门 ⇒ approver 强制 T1，且不可自助申请。
func TestA1_CrossDeptRequiresT1AndBlocksSelfService(t *testing.T) {
	d := buildOrg()
	// bob(运营部) + erin(财务部) 共同负责同一条链 ⇒ 跨部门
	dc, err := d.ResolveDataChain(map[string]string{
		"channel:TK-TH": "bob",
		"channel:SH-TH": "erin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !dc.CrossDept {
		t.Fatalf("should be cross-dept, depts=%v", dc.Depts)
	}
	// 自助申请必须被拒
	if err := chain.ValidateCrossDeptSelfService(dc, chain.DefaultCrossDeptPolicy()); err == nil {
		t.Fatalf("cross-dept must NOT be self-serviceable (A1)")
	}
	// 路由应强制 T1
	ac, err := d.Route(chain.RouteInput{Applicant: "bob", CrossDept: true}, chain.DefaultCcPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if ac.Approver != "ceo" || ac.ApproverTier != "T1" {
		t.Fatalf("cross-dept approver must be T1 (ceo), got %s/%s", ac.Approver, ac.ApproverTier)
	}
}

// F10：人工覆盖优先；离职回退派生。
func TestF10_ManualOverrideWins(t *testing.T) {
	d := buildOrg()
	n, _ := d.Get("bob")
	n.PrimaryDept = "增长部"        // 人工覆盖
	n.DeptSource = chain.Manual
	dept, src, err := d.ResolveDept("bob")
	if err != nil {
		t.Fatal(err)
	}
	if dept != "增长部" || src != chain.Manual {
		t.Fatalf("manual override should win, got %s/%s", dept, src)
	}
	// 回退：改回派生
	n.DeptSource = chain.Derived
	n.PrimaryDept = "运营部"
	dept, src, _ = d.ResolveDept("bob")
	if dept != "运营部" || src != chain.Derived {
		t.Fatalf("derived fallback failed, got %s/%s", dept, src)
	}
}

// 顶层申请人：+1 取 T1 兜底。
func TestTopLevelFallback(t *testing.T) {
	d := buildOrg()
	ac, err := d.Route(chain.RouteInput{Applicant: "ceo"}, chain.DefaultCcPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !ac.Fallback || ac.Approver != "ceo" {
		t.Fatalf("top-level should fallback to T1 self, got fallback=%v approver=%s", ac.Fallback, ac.Approver)
	}
}

// 环检测：配置成环必须显式报错，不得死循环。
func TestCycleDetection(t *testing.T) {
	d := chain.NewOrgDirectory()
	d.Put(&chain.OrgNode{Account: "x", Tier: "T3", Supervisor: "y", PrimaryDept: "d"})
	d.Put(&chain.OrgNode{Account: "y", Tier: "T3", Supervisor: "x", PrimaryDept: "d"})
	if _, err := d.SupervisorChain("x"); err == nil {
		t.Fatalf("cycle must be detected")
	}
}
