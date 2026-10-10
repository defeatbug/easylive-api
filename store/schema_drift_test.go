// 迁移与 Ent schema 的一致性校验（无需数据库）。
//
// 为什么需要它：本项目的 DDL 有两个消费者——Ent 用它生成查询、sqlc 用它生成类型。
// "改了 Ent schema 忘了写迁移"这类漂移在本地编译期完全看不出来，
// 一直到线上第一条 SQL 才炸（column does not exist）。这个测试把它提前到 go test。
package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"easylive-api/ent/migrate"
)

// sqlcOwnedTables 是由 sqlc 负责、**故意不在 Ent 里定义**的表。
// 混合方案的边界写在 store.go 的包注释里，这里用代码再钉一遍：
// 出现在迁移里却不属于 Ent 的表，必须是显式登记的 sqlc 表。
var sqlcOwnedTables = map[string]bool{
	// 指标时序表：高写入 + 强聚合，由 sqlc 用显式 SQL 处理（下一步接入）
	"metric_samples": true,
}

var (
	createTableRe = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["']?(\w+)["']?\s*\((.*?)\)\s*;`)
	alterAddRe    = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+["']?(\w+)["']?\s+ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?["']?(\w+)["']?`)
	constraintRe  = regexp.MustCompile(`(?i)^\s*(PRIMARY\s+KEY|FOREIGN\s+KEY|UNIQUE|CONSTRAINT|CHECK)\b`)
	commentRe     = regexp.MustCompile(`--[^\n]*`)
)

// migrationColumns 解析 migrations/*.sql，返回 表名 → 列名集合。
//
// 只做必要的语法理解（CREATE TABLE 主体 + ALTER TABLE ADD COLUMN），
// 不引第三方 SQL 解析器：迁移文件是我们自己写的，风格稳定。
func migrationColumns(t *testing.T) map[string]map[string]bool {
	t.Helper()
	files, err := filepath.Glob("../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到迁移文件（err=%v, 数量=%d）", err, len(files))
	}

	out := map[string]map[string]bool{}
	add := func(table, col string) {
		table = strings.ToLower(strings.Trim(table, `"'`))
		col = strings.ToLower(strings.Trim(col, `"'`))
		if out[table] == nil {
			out[table] = map[string]bool{}
		}
		out[table][col] = true
	}

	for _, f := range files {
		raw, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("读取迁移 %s 失败: %v", f, rerr)
		}
		sqlText := commentRe.ReplaceAllString(string(raw), "")
		for _, m := range createTableRe.FindAllStringSubmatch(sqlText, -1) {
			table, body := m[1], m[2]
			for _, line := range strings.Split(body, ",") {
				line = strings.TrimSpace(line)
				if line == "" || constraintRe.MatchString(line) {
					continue
				}
				fields := strings.Fields(line)
				if len(fields) > 0 {
					add(table, fields[0])
				}
			}
		}
		for _, m := range alterAddRe.FindAllStringSubmatch(sqlText, -1) {
			add(m[1], m[2])
		}
	}
	return out
}

func TestMigrationMatchesEntSchema(t *testing.T) {
	fromSQL := migrationColumns(t)

	fromEnt := map[string]map[string]bool{}
	for _, table := range migrate.Tables {
		cols := map[string]bool{}
		for _, c := range table.Columns {
			cols[c.Name] = true
		}
		fromEnt[table.Name] = cols
	}

	// 方向一：Ent 有、迁移没有 → 迁移落后于 schema，运行时必然报列不存在
	for table, cols := range fromEnt {
		sqlCols, ok := fromSQL[table]
		if !ok {
			t.Errorf("Ent 表 %q 在 migrations/*.sql 里没有 CREATE TABLE", table)
			continue
		}
		for col := range cols {
			if !sqlCols[col] {
				t.Errorf("表 %s：Ent 定义了列 %q，但迁移里没有（补一个 ALTER/CREATE）", table, col)
			}
		}
	}

	// 方向二：迁移有、Ent 没有 → 要么漏写 schema，要么是废弃列没清理
	for table, cols := range fromSQL {
		if sqlcOwnedTables[table] {
			continue // 该表由 sqlc 负责，故意不在 Ent 里
		}
		entCols, ok := fromEnt[table]
		if !ok {
			t.Errorf("迁移里有表 %q，但既不在 Ent schema 里也没登记为 sqlc 表（漏写还是废弃？）", table)
			continue
		}
		for col := range cols {
			if !entCols[col] {
				t.Errorf("表 %s：迁移有列 %q，Ent schema 没有", table, col)
			}
		}
	}
}
