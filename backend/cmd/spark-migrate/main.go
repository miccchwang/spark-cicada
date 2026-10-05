// Command spark-migrate —— 数据库迁移执行器。
//
// 用法：
//
//	spark-migrate -dir ../../sql/migrations -dry-run   # 仅校验（CI 用）
//	spark-migrate -dir ../../sql/migrations -up       # 应用
//	spark-migrate -dir ../../sql/migrations -status   # 查看记账
//
// 连接串来源：SPARK_DB_DSN，或 SPARK_PG_HOST/USER/PASSWORD/DB/PORT/SSLMODE。
// **纪律**：任何输出都不得回显口令（G11）—— 统一走 db.RedactDSN。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/miccchwang/spark-cicada/backend/internal/db"
)

func main() {
	var (
		dir     = flag.String("dir", "sql/migrations", "迁移文件目录")
		doUp    = flag.Bool("up", false, "应用所有未执行迁移")
		doDry   = flag.Bool("dry-run", false, "仅校验迁移集（不写库）")
		doStat  = flag.Bool("status", false, "打印已应用迁移")
		timeout = flag.Duration("timeout", 60*time.Second, "总超时")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	migs, err := db.LoadMigrations(*dir)
	if err != nil {
		log.Fatalf("加载迁移失败：%v", err)
	}
	fmt.Printf("发现 %d 个迁移：\n", len(migs))
	for _, m := range migs {
		fmt.Printf("  %s  %s  (sha256:%s)\n", m.Version, m.Name, m.Hash[:12])
	}

	dsn, err := db.DSNFromEnv()
	if err != nil {
		log.Fatalf("连接配置失败：%v", err)
	}
	fmt.Printf("目标库：%s\n", db.RedactDSN(dsn))

	m, err := db.NewMigrator(ctx, dsn)
	if err != nil {
		log.Fatalf("无法连接数据库：%v", err)
	}
	defer m.Close()

	switch {
	case *doDry:
		res, err := m.DryRun(ctx, migs)
		if err != nil {
			log.Fatalf("校验失败：%v", err)
		}
		fmt.Printf("校验通过。待应用 %d 个，已应用 %d 个。\n", len(res.Applied), len(res.Skipped))
		for _, s := range res.Applied {
			fmt.Printf("  待应用：%s\n", s)
		}
	case *doUp:
		res, err := m.Up(ctx, migs)
		if err != nil {
			log.Fatalf("迁移失败（已回滚当前迁移）：%v", err)
		}
		fmt.Printf("完成。本次应用 %d 个，跳过 %d 个。\n", len(res.Applied), len(res.Skipped))
		for _, s := range res.Applied {
			fmt.Printf("  ✔ %s\n", s)
		}
		for _, s := range res.Skipped {
			fmt.Printf("  · %s（已应用）\n", s)
		}
	case *doStat:
		applied, err := m.Applied(ctx)
		if err != nil {
			log.Fatalf("读取状态失败：%v", err)
		}
		for _, mg := range migs {
			if a, ok := applied[mg.Version]; ok {
				fmt.Printf("  ✔ %s  applied_at=%s  hash=%s\n",
					a.Name, a.AppliedAt.Format(time.RFC3339), a.Hash[:12])
			} else {
				fmt.Printf("  ✗ %s  （未应用）\n", mg.Name)
			}
		}
	default:
		fmt.Fprintln(os.Stderr, "请指定 -up / -dry-run / -status 之一")
		flag.Usage()
		os.Exit(2)
	}
}
