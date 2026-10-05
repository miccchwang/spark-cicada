// g1_layering_test.go —— G1 分层依赖闸门的单元测试。
package gate_test

import (
	"testing"

	"github.com/miccchwang/spark-cicada/backend/internal/gate"
)

func TestG1_FilterNoNetwork(t *testing.T) {
	good := []gate.SourceFile{
		{Path: "frontend/src/filter/slider.ts", Content: "export function state() { return {}; }"},
	}
	if v := gate.CheckFilterNoNetwork(good); len(v) != 0 {
		t.Fatalf("G1 干净的 M-FILTER 被误判: %v", v)
	}
	bad := []gate.SourceFile{
		{Path: "frontend/src/filter/data.ts", Content: "const r = await fetch('/api/x');"},
	}
	if v := gate.CheckFilterNoNetwork(bad); len(v) == 0 {
		t.Fatal("G1 应检出 M-FILTER 的网络调用")
	}
	// 豁免注释生效
	allowed := []gate.SourceFile{
		{Path: "frontend/src/filter/x.ts", Content: "await fetch('/x'); // gate:allow-filter-network"},
	}
	if v := gate.CheckFilterNoNetwork(allowed); len(v) != 0 {
		t.Fatalf("G1 豁免注释应生效: %v", v)
	}
}

func TestG1_RenderNoQueryState(t *testing.T) {
	good := []gate.SourceFile{
		{Path: "frontend/src/render/table.ts", Content: "function render(dc: DataContract) {}"},
	}
	if v := gate.CheckRenderNoQueryState(good); len(v) != 0 {
		t.Fatalf("G1 干净的 M-RENDER 被误判: %v", v)
	}
	bad := []gate.SourceFile{
		{Path: "frontend/src/render/table.ts", Content: "const q: QueryState = props.qs;"},
	}
	if v := gate.CheckRenderNoQueryState(bad); len(v) == 0 {
		t.Fatal("G1 应检出 M-RENDER 引用 QueryState")
	}
}

func TestG1_QueryNoBusinessFormula(t *testing.T) {
	good := []gate.SourceFile{
		{Path: "backend/internal/query/query.go", Content: `where := "month >= $1"`},
	}
	if v := gate.CheckQueryNoBusinessFormula(good); len(v) != 0 {
		t.Fatalf("G1 干净查询层被误判: %v", v)
	}
	// 在查询层里算公式 ⇒ 违规
	bad := []gate.SourceFile{
		{Path: "backend/internal/query/query.go", Content: `select rev - cogs as gp from t`},
	}
	if v := gate.CheckQueryNoBusinessFormula(bad); len(v) == 0 {
		t.Fatal("G1 应检出查询层内联业务公式")
	}
}
