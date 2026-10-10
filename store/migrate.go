package store

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// migrateLockKey 是迁移用的 PostgreSQL advisory lock 键（"easylive" 的十六进制）。
// 多实例同时启动时，只有拿到锁的那个执行迁移，其余等待后继续——
// 迁移本身是幂等的，所以"重复执行"无害，但不能真的并发跑（DDL 会互相阻塞）。
const migrateLockKey int64 = 0x656173796c6976

// Migrate 按文件名顺序执行 dir 下的 *.sql。
//
// 为什么需要它：全新部署如果没人手动跑迁移，业务接口会以"表不存在"告终，
// 而媒体功能完全正常——这种组合很容易被误判成"接口代码写错了"。
// 让服务自己把迁移跑掉，部署就真的只有一条命令。
//
// 前提：迁移文件必须幂等（IF NOT EXISTS / ADD COLUMN IF NOT EXISTS），
// 因为每次启动都会重跑一遍（没有版本表，也就不需要"迁移历史"这套机制）。
func (s *Store) Migrate(ctx context.Context, dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return fmt.Errorf("扫描迁移目录失败: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("迁移目录 %s 下没有 .sql 文件", dir)
	}
	sort.Strings(files) // 文件名前缀编号决定顺序（001_, 002_, ...）

	// 迁移可能被 ctx 取消（例如启动超时），但绝不能一直持锁
	lockCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	conn, err := s.Pool.Acquire(lockCtx)
	if err != nil {
		return fmt.Errorf("获取数据库连接失败: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(lockCtx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("获取迁移锁失败: %w", err)
	}
	defer func() {
		// 释放锁失败只记日志：连接归还后锁会自动释放
		if _, err := conn.Exec(context.WithoutCancel(lockCtx), "SELECT pg_advisory_unlock($1)", migrateLockKey); err != nil {
			log.Printf("store: 释放迁移锁失败（连接关闭时会自动释放）: %v", err)
		}
	}()

	applied := 0
	for _, f := range files {
		raw, rerr := os.ReadFile(f)
		if rerr != nil {
			return fmt.Errorf("读取迁移 %s 失败: %w", filepath.Base(f), rerr)
		}
		// 关键：**必须用简单协议**。pgx 默认走扩展协议（预编译语句），
		// 而"一个文件里多条 SQL"在预编译语句下会直接报
		// "cannot insert multiple commands into a prepared statement"。
		if _, eerr := conn.Exec(lockCtx, string(raw), pgx.QueryExecModeSimpleProtocol); eerr != nil {
			return fmt.Errorf("执行迁移 %s 失败: %w", filepath.Base(f), eerr)
		}
		applied++
	}
	log.Printf("store: 迁移完成，共执行 %d 个文件（目录 %s）", applied, dir)
	return nil
}
