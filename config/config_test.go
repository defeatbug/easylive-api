package config

import (
	"os"
	"testing"
)

// TestBuildDSNPreferences DATABASE_URL 优先，POSTGRES_* 作为回退。
//
// 为什么值得单测：这个函数以前**只按 POSTGRES_* 拼 DSN**，而配置里的提示信息
// 却写着"或直接设 DATABASE_URL"——典型的"文档承诺、代码不读"。
// 后果很隐蔽：服务照样启动（连到另一台/另一个库）、迁移照样报成功，
// 只是表建在了没人看的库里。实测踩到一次（迁移 3 个文件成功，目标库 0 张表）。
func TestBuildDSNPreferences(t *testing.T) {
	keys := []string{"DATABASE_URL", "POSTGRES_HOST", "POSTGRES_PORT", "POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB_NAME"}
	saved := map[string]string{}
	for _, k := range keys {
		saved[k] = os.Getenv(k)
	}
	defer func() {
		for k, v := range saved {
			if v == "" {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, v)
			}
		}
	}()

	for _, k := range keys {
		_ = os.Unsetenv(k)
	}

	// 只设 DATABASE_URL：必须原样采用
	want := "postgres://u:p@127.0.0.1:5433/db?sslmode=disable"
	_ = os.Setenv("DATABASE_URL", want)
	if got := buildDSN(); got != want {
		t.Fatalf("DATABASE_URL 应当优先，实际 %q", got)
	}

	// 没设 DATABASE_URL：回退到 POSTGRES_* 组合
	_ = os.Unsetenv("DATABASE_URL")
	_ = os.Setenv("POSTGRES_HOST", "db.internal")
	_ = os.Setenv("POSTGRES_PORT", "6543")
	_ = os.Setenv("POSTGRES_USER", "u2")
	_ = os.Setenv("POSTGRES_PASSWORD", "p2")
	_ = os.Setenv("POSTGRES_DB_NAME", "appdb")
	got := buildDSN()
	if got != "postgres://u2:p2@db.internal:6543/appdb?sslmode=disable" {
		t.Fatalf("回退 DSN 不符，实际 %q", got)
	}
}
