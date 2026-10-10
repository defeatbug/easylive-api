// easylive-api：轻量级 WebRTC 实时音视频会议系统（服务端）
//
// 启动装配顺序（与 egg-api 的组织方式保持一致）：
//
//	配置 → 数据库（可选）→ Pion 引擎 → 房间管理器 → 信令 Hub → Fiber 服务 → 优雅关闭
package main

import (
	"context"
	"log"
	"os"

	"github.com/joho/godotenv"

	"easylive-api/config"
	"easylive-api/server"
	"easylive-api/sfu"
	"easylive-api/signaling"
	"easylive-api/store"
)

func main() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// 宿主机 `go run .` 时读取 .env；容器内由 docker compose 的 env_file 注入，
	// 此时 .env 可能不存在，忽略错误即可。
	if err := godotenv.Load(); err != nil {
		log.Printf("main: 未加载 .env（容器内属正常）: %v", err)
	}

	cfg := config.Load()
	cfg.LogSummary()

	ctx := context.Background()

	// 数据库是"可选依赖"：连不上只告警，不影响开会（媒体面与会话状态都在内存里）。
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		log.Printf("main: 数据库不可用，跳过持久化: %v", err)
		st = nil
	} else {
		log.Printf("main: 数据库已连接")
		defer st.Close()
	}

	engine, err := sfu.NewEngine(cfg)
	if err != nil {
		log.Fatalf("main: 初始化媒体引擎失败: %v", err)
	}
	defer func() {
		if err := engine.Close(); err != nil {
			log.Printf("main: 关闭媒体引擎出错: %v", err)
		}
	}()

	manager := sfu.NewManager(engine, cfg.RoomMaxParticipants, cfg.ResumeGrace)
	hub := signaling.NewHub(manager, cfg)

	srv := server.New(cfg, manager, hub, st)
	if err := srv.Run(); err != nil {
		log.Fatalf("main: 服务异常退出: %v", err)
	}
	log.Printf("main: 已安全退出")
}
