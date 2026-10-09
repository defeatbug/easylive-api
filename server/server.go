// Package server 负责装配 Fiber 应用：路由、中间件、优雅关闭。
package server

import (
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/limiter"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"

	"easylive-api/config"
	"easylive-api/sfu"
	"easylive-api/signaling"
)

type Server struct {
	cfg     *config.Config
	app     *fiber.App
	hub     *signaling.Hub
	manager *sfu.Manager
	started time.Time
}

func New(cfg *config.Config, manager *sfu.Manager, hub *signaling.Hub) *Server {
	s := &Server{cfg: cfg, hub: hub, manager: manager, started: time.Now()}
	s.app = s.build()
	return s
}

func (s *Server) build() *fiber.App {
	cfg := s.cfg

	app := fiber.New(fiber.Config{
		AppName:           cfg.AppName,
		EnablePrintRoutes: true,
		Concurrency:       cfg.FiberConcurrency,
		ReadTimeout:       cfg.FiberReadTimeout,
		WriteTimeout:      cfg.FiberWriteTimeout,
		// 信任反代（Nginx / Caddy）传来的真实客户端 IP
		EnableTrustedProxyCheck: true,
		TrustedProxies:          []string{"172.16.0.0/12", "127.0.0.1"},
		ProxyHeader:             fiber.HeaderXForwardedFor,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			log.Printf("server: error [%s %s]: %v", c.Method(), c.Path(), err)
			return c.Status(code).JSON(fiber.Map{"error": "服务暂时不可用，请稍后重试"})
		},
	})

	app.Use(recover.New())
	app.Use(logger.New(logger.Config{
		Format:     "[${time}] ${status} ${latency} ${method} ${path}\n",
		TimeFormat: "2006-01-02 15:04:05",
		Output:     os.Stdout,
	}))
	app.Use(cors.New(cors.Config{
		AllowOrigins:     strings.Join(cfg.WebOrigins, ","),
		AllowMethods:     "GET, POST, OPTIONS",
		AllowHeaders:     "Origin, Content-Type, Accept, Authorization",
		AllowCredentials: true,
		MaxAge:           86400,
	}))

	// 全局限流：WebSocket 路径有自己的连接级限制，跳过以免双重计数。
	wsPrefix := cfg.AppBasePath + "/ws"
	app.Use(limiter.New(limiter.Config{
		Max:        cfg.HTTPRateLimitPerIP,
		Expiration: time.Second,
		KeyGenerator: func(c *fiber.Ctx) string {
			return c.IP()
		},
		Next: func(c *fiber.Ctx) bool {
			return strings.HasPrefix(c.Path(), wsPrefix)
		},
	}))

	var router fiber.Router = app
	if cfg.AppBasePath != "" {
		router = app.Group(cfg.AppBasePath)
	}

	// ---- 健康检查与运行指标（评测脚本的采集入口）----
	router.Get("/healthz", func(c *fiber.Ctx) error {
		rooms, participants, tracks := s.manager.Stats()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		return c.JSON(fiber.Map{
			"status":       "ok",
			"uptimeSec":    int(time.Since(s.started).Seconds()),
			"rooms":        rooms,
			"participants": participants,
			"tracks":       tracks,
			"wsConns":      s.hub.Count(),
			"goroutines":   runtime.NumGoroutine(),
			"heapAllocMB":  float64(mem.HeapAlloc) / 1024 / 1024,
		})
	})

	router.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("easylive-api is running ...")
	})

	// ---- 调试快照：排查"订阅端黑屏"时看这里 ----
	// 关注点：receivedFromPublisher（服务端到底有没有收到发布端的媒体）
	//        published.packets（转发热路径读到了多少包）
	//        sentToSubscriber（有没有真的发给订阅端）
	if cfg.EnableDebugEndpoint {
		router.Get("/debug/stats", func(c *fiber.Ctx) error {
			return c.JSON(fiber.Map{
				"generatedAt": time.Now().Format(time.RFC3339),
				"rooms":       s.manager.DebugInfo(),
			})
		})
	}

	// ---- 媒体信令入口：<basePath>/ws ----
	signaling.Mount(router.Group("/ws"), s.hub)

	// ---- GraphQL（业务接口：创建房间 / 房间列表）----
	//
	// TODO(gqlgen)：接入 gqlgen 只需要替换下面的占位 handler，写法与 egg-api 完全一致：
	//
	//	r.All("", adaptor.HTTPHandler(gqlgenHandler.NewDefaultServer(graph.NewExecutableSchema(graph.Config{Resolvers: &graph.Resolver{}}))))
	//
	// 注意：gqlgen 的 handler 是 net/http 的 http.Handler，必须经 adaptor.HTTPHandler 桥接（Fiber 官方支持的方式）。
	router.All("/graphql", adaptor.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"errors":[{"message":"GraphQL 尚未接入，见 server/server.go 的 TODO(gqlgen)"}]}`))
	})))

	return app
}

// App 暴露 Fiber 实例：测试与自定义启动方式（如按随机端口监听）需要它。
func (s *Server) App() *fiber.App { return s.app }

// Run 启动服务并阻塞，直到收到退出信号后优雅关闭。
func (s *Server) Run() error {
	addr := ":" + s.cfg.AppPort
	log.Printf("server: 监听 %s（媒体 UDP 端口 %d）", addr, s.cfg.MediaUDPPort)

	errCh := make(chan error, 1)
	go func() {
		if err := s.app.Listen(addr); err != nil {
			errCh <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-errCh:
		return err
	case sig := <-quit:
		log.Printf("server: 收到信号 %s，开始优雅关闭", sig)
	}

	// 顺序很重要：先停 HTTP/WS（不再有新连接），再关 Pion 引擎释放 UDP 端口。
	if err := s.app.ShutdownWithTimeout(10 * time.Second); err != nil {
		log.Printf("server: 关闭 Fiber 时出错: %v", err)
	}
	return nil
}
