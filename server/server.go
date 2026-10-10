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

	"easylive-api/auth"
	"easylive-api/config"
	"easylive-api/sfu"
	"easylive-api/signaling"
	"easylive-api/turn"
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

	// ---- 令牌发放：POST <basePath>/token ----
	//
	// 生产环境**必须**把这个端点换掉：它应当是你的登录系统的一部分（校验账号密码/SSO 后签发），
	// 而不是任何人传个 identity 就能拿到令牌。因此这里加了一道闸门：
	// 非 development 环境下必须带 X-API-Key（由 TOKEN_API_KEY 配置），否则拒绝签发。
	router.Post("/token", func(c *fiber.Ctx) error {
		a := s.hub.Authenticator()
		if !a.Enabled() {
			return c.Status(fiber.StatusNotImplemented).JSON(fiber.Map{
				"error": "鉴权未启用（AUTH_MODE=off 或未配置 JWT_SECRET），当前无需令牌即可入会",
			})
		}
		if cfg.AppEnv != "development" {
			if cfg.TokenAPIKey == "" {
				return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
					"error": "服务端未配置 TOKEN_API_KEY，拒绝签发令牌",
				})
			}
			if c.Get("X-API-Key") != cfg.TokenAPIKey {
				return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "X-API-Key 无效"})
			}
		}

		var req struct {
			RoomID         string `json:"roomId"`
			Identity       string `json:"identity"`
			Name           string `json:"name"`
			CanPublish     *bool  `json:"canPublish"`
			CanSubscribe   *bool  `json:"canSubscribe"`
			CanPublishData *bool  `json:"canPublishData"`
			RoomAdmin      *bool  `json:"roomAdmin"`
		}
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "请求体不是合法 JSON"})
		}
		if req.RoomID == "" || req.Identity == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "roomId 与 identity 必填"})
		}
		// 缺省授予发布/订阅/发数据；**主持人权限必须显式申请**（默认不给，避免演示配置手滑变全权）
		grant := auth.VideoGrant{
			RoomJoin:       true,
			Room:           req.RoomID,
			CanPublish:     req.CanPublish == nil || *req.CanPublish,
			CanSubscribe:   req.CanSubscribe == nil || *req.CanSubscribe,
			CanPublishData: req.CanPublishData == nil || *req.CanPublishData,
			RoomAdmin:      req.RoomAdmin != nil && *req.RoomAdmin,
		}
		token, exp, err := a.Issue(req.Identity, req.Name, grant)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		log.Printf("server: 签发令牌 room=%s identity=%s admin=%v", req.RoomID, req.Identity, grant.RoomAdmin)
		return c.JSON(fiber.Map{
			"token":     token,
			"identity":  req.Identity,
			"roomId":    req.RoomID,
			"expiresAt": exp.UnixMilli(),
			"video":     grant,
		})
	})

	// ---- 客户端 ICE 配置：GET <basePath>/ice ----
	//
	// 为什么必须由服务端下发：STUN 只能做地址发现，双方都在对称 NAT 后必须走中继，
	// 而把长期 TURN 密钥发给浏览器等于把中继开放成免费代理。这里按 coturn 的
	// use-auth-secret 方案现签**临时凭证**（有效期 TURN_CRED_TTL，默认 1 小时）。
	//
	// identity 由客户端自报：它只影响凭证的归属标注，不能借此获得额外权限；
	// 真正的授权边界是"密钥不下发 + 凭证会过期"。
	router.Get("/ice", func(c *fiber.Ctx) error {
		identity := c.Query("identity", "anonymous")
		servers := turn.Servers(cfg.StunURLs, cfg.TurnURLs, cfg.TurnSecret, identity, cfg.TurnCredTTL)
		return c.JSON(fiber.Map{
			"iceServers":       servers,
			"ttlSeconds":       turn.TTLSeconds(cfg.TurnCredTTL),
			"turnEnabled":      len(cfg.TurnURLs) > 0 && cfg.TurnSecret != "",
			"credentialScheme": "turn-rest-api", // coturn --use-auth-secret
		})
	})

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
