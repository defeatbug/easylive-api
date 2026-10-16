// easylive-api：轻量级 WebRTC 实时音视频会议系统（服务端）
//
// 启动装配顺序：
//
//	配置 → 数据库（可选）→ Pion 引擎 → 房间管理器 → 信令 Hub
//	     → 迁移与指标采样 → Fiber 服务 → 优雅关闭（采样器 → 连接池 → 引擎）
//
// 一条贯穿始终的原则：**数据库是可选依赖，且绝不出现在媒体热路径上**。
// 连不上库时会议照常进行，只是业务接口（房间/会议/指标）不可用。
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/joho/godotenv"

	"easylive-api/config"
	"easylive-api/mediarec"
	"easylive-api/recorder"
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
	// 安全配置错误必须在**启动前**拦下：见 config.Validate 的说明
	// （AUTH_MODE=required 漏配密钥以前会静默降级为"不校验"，等于全员全权限）。
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config: %v", err)
	}
	cfg.LogSummary()

	ctx := context.Background()

	// 数据库是"可选依赖"：连不上只告警，不影响开会（媒体面与会话状态都在内存里）。
	// 启动阶段等一下数据库：容器编排里 postgres 常常比 api 晚就绪
	// （首次启动还要恢复数据目录），没有这段等待就会永久降级到"无数据层"。
	st, err := store.OpenWithRetry(ctx, cfg.PostgresDSN, 45*time.Second)
	if err != nil {
		log.Printf("main: ❌ 数据库不可用: %v", err)
		log.Printf("main: 数据层未启用 —— 房间/会议（Ent）与指标样本（sqlc）都不会写入；" +
			"/graphql 会返回\"数据库不可用\"，/healthz 的 dataLayer 显示 unavailable。媒体功能不受影响。")
		if hint := cfg.ComposeHostHint(); hint != "" {
			log.Printf("main: 提示：%s", hint)
		}
		st = nil
	} else {
		log.Printf("main: ✅ 数据库已连接（数据层：Ent 管 rooms/meetings，sqlc 管 metric_samples）")
	}

	engine, err := sfu.NewEngine(cfg)
	if err != nil {
		log.Fatalf("main: 初始化媒体引擎失败: %v", err)
	}

	manager := sfu.NewManager(engine, cfg.RoomMaxParticipants, cfg.ResumeGrace)
	hub := signaling.NewHub(manager, cfg)

	// ---- 会议录制（可选）----
	// 零转码：把已经在转发的视频 RTP 多写一份到磁盘（每轨一个 IVF 文件）。
	var mediaRec *mediarec.Manager
	if cfg.RecordEnabled {
		rec, rerr := mediarec.New(cfg.RecordDir)
		if rerr != nil {
			log.Printf("main: 会议录制未能开启: %v", rerr)
		} else {
			mediaRec = rec
			if cfg.RecordRawDump {
				rec.EnableRawDump()
				log.Printf("main: 原始 RTP 转储已开启（.rtp，供离线分析录制缺陷）")
			}
			manager.EnableRecording(rec)
			log.Printf("main: ✅ 会议录制已开启，输出目录 %s", rec.Dir())
		}
	}

	// ---- 迁移 + 指标采样（都需要数据库与房间管理器）----
	var (
		stopSampler context.CancelFunc
		samplerDone chan struct{}
	)
	if st != nil {
		if cfg.AutoMigrate {
			mctx, mcancel := context.WithTimeout(ctx, 60*time.Second)
			if err := st.Migrate(mctx, cfg.MigrationsDir); err != nil {
				// 迁移失败不致命：媒体功能不受影响，业务接口会在调用时明确报"表不存在"
				log.Printf("main: 自动迁移失败（业务接口可能不可用）: %v", err)
			}
			mcancel()
			// 引导第一个主持人账号：没有它，启用鉴权后**没有任何账号能登录**（先有鸡还是先有蛋）。
			// 只在用户不存在时创建，绝不覆盖已有口令——否则每次重启都会把人工改过的密码打回去。
			if cfg.AdminUsername != "" && cfg.AdminPassword != "" {
				// 必须用**独立** context：上面的 mctx 在迁移结束时已经被 cancel，
				// 拿它去建账号只会拿到 "context canceled"（第一版就是这么写的，
				// 表现为"迁移成功、表也在，但登录始终 401"）。
				bctx, bcancel := context.WithTimeout(ctx, 30*time.Second)
				created, err := st.EnsureUser(bctx, cfg.AdminUsername, cfg.AdminUsername, cfg.AdminPassword, "admin")
				bcancel()
				switch {
				case err != nil:
					log.Printf("main: 引导管理员账号失败（不影响启动，可稍后手动处理）: %v", err)
				case created:
					log.Printf("main: 已创建主持人账号 username=%s（口令来自 ADMIN_PASSWORD，未记录明文）", cfg.AdminUsername)
				default:
					log.Printf("main: 主持人账号已存在 username=%s（未改动其口令）", cfg.AdminUsername)
				}
			}
		}
		if cfg.MetricsInterval > 0 {
			log.Printf("main: 指标采样已启用（每 %s 一次 → metric_samples；会议记录 → meetings）", cfg.MetricsInterval)
			sampler := recorder.New(st, manager, recorder.Config{
				Interval:  cfg.MetricsInterval,
				Retention: cfg.MetricsRetention,
			})
			var sctx context.Context
			sctx, stopSampler = context.WithCancel(ctx)
			samplerDone = make(chan struct{})
			go func() {
				defer close(samplerDone)
				sampler.Run(sctx)
			}()
		}
	}

	srv := server.New(cfg, manager, hub, st)
	runErr := srv.Run()

	// ---- 优雅关闭：顺序很重要 ----
	// 1) 先停采样器（它还要往库里写"收尾会议记录"，必须先于连接池关闭）
	// 2) 再关连接池
	// 3) 最后关媒体引擎（释放 UDP/TCP 端口）
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if mediaRec != nil {
		// 先收尾录制：把每条轨道的写队列 flush 并回填 IVF 帧数，否则文件头里的帧数不准
		mediaRec.CloseAll()
	}
	if stopSampler != nil {
		stopSampler()
		select {
		case <-samplerDone:
		case <-shutdownCtx.Done():
			log.Printf("main: 采样器未在 10s 内退出，继续关闭")
		}
	}
	if st != nil {
		if err := st.Close(); err != nil {
			log.Printf("main: 关闭数据库连接池出错: %v", err)
		}
	}
	if err := engine.Close(); err != nil {
		log.Printf("main: 关闭媒体引擎出错: %v", err)
	}

	if runErr != nil {
		log.Fatalf("main: 服务异常退出: %v", runErr)
	}
	log.Printf("main: 已安全退出")
}
