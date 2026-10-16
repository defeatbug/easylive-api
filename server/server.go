// Package server 负责装配 Fiber 应用：路由、中间件、优雅关闭。
package server

import (
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"fmt"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/limiter"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"

	"easylive-api/auth"
	"easylive-api/config"
	"easylive-api/graph"
	"easylive-api/sfu"
	"easylive-api/signaling"
	"easylive-api/store"
	"easylive-api/sysinfo"
	"easylive-api/turn"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
)

type Server struct {
	cfg     *config.Config
	app     *fiber.App
	hub     *signaling.Hub
	manager *sfu.Manager
	// st 可能为 nil：数据库是可选依赖（连不上不影响开会）
	st      *store.Store
	started time.Time
}

func New(cfg *config.Config, manager *sfu.Manager, hub *signaling.Hub, st *store.Store) *Server {
	s := &Server{cfg: cfg, hub: hub, manager: manager, st: st, started: time.Now()}
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
	// 逐轨道指标：字段命名对齐浏览器 getStats 的口径，便于把两端放进同一张表。
	// ?format=csv 导出稳定的列顺序，直接给评测脚本/论文图表用。
	// 指标总览：实时状态 + 数据库里的历史曲线，供报表页一次拉齐。
	// 数据库不可用时**不报错**，只把 dbAvailable 置为 false —— 媒体面永远优先，
	// 报表页应当显示"无历史数据"而不是整页崩掉。
	router.Get("/metrics/overview", func(c *fiber.Ctx) error {
		minutes := c.QueryInt("minutes", 30)
		if minutes < 1 {
			minutes = 1
		}
		if minutes > 1440 {
			minutes = 1440
		}
		bucketSec := c.QueryInt("bucket", 10)
		if bucketSec < 1 {
			bucketSec = 1
		}
		if bucketSec > 600 {
			bucketSec = 600
		}
		since := time.Now().Add(-time.Duration(minutes) * time.Minute)

		rooms, participants, tracks := s.manager.Stats()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		pliFwd, pliSup := sfu.FrameRequestStats()

		out := fiber.Map{
			"sampledAt":   time.Now().UTC().Format(time.RFC3339),
			"windowMin":   minutes,
			"bucketSec":   bucketSec,
			"dbAvailable": false,
			"history":     []any{},
			"live": fiber.Map{
				"rooms": rooms, "participants": participants, "tracks": tracks,
				"goroutines":      runtime.NumGoroutine(),
				"heapAllocMB":     float64(mem.HeapAlloc) / 1024 / 1024,
				"cpuSeconds":      sysinfo.CPUSeconds(),
				"goroutinePanics": sfu.GoroutinePanicCount(),
				"messageFloods":   signaling.MessageFloodCount(),
				"malformedRTCP":   sfu.MalformedRTCPCount(),
				"pliForwarded":    pliFwd,
				"pliSuppressed":   pliSup,
			},
		}

		if s.st != nil {
			if gm, err := s.st.GlobalMetrics(c.UserContext(), since); err == nil {
				out["global"] = gm
			}
			// 房间集合 = 当前存活的 ∪ **已落库的**。
			// 只用存活房间会有一个很容易漏掉的现象：重启后历史数据全部"消失"——
			// 因为历史样本属于已经结束的房间。实测正是这样（重启后曲线点数 0）。
			roomIDs := s.manager.RoomIDs()
			seen := make(map[string]bool, len(roomIDs))
			for _, id := range roomIDs {
				seen[id] = true
			}
			if rooms, rerr := s.st.ListRooms(c.UserContext(), 200, 0); rerr == nil {
				for _, r := range rooms {
					if !seen[r.ID] {
						seen[r.ID] = true
						roomIDs = append(roomIDs, r.ID)
					}
				}
			}
			// 历史曲线加 5 秒缓存：报表页按秒轮询，而这里每次都要把**每个房间**的桶查询
			// 跑一遍；数据本身每 METRICS_INTERVAL 才更新一次，同一窗口内的重复查询纯属浪费。
			cacheKey := fmt.Sprintf("%d/%d", minutes, bucketSec)
			hist, cached := historyCacheGet(cacheKey)
			if !cached {
				var herr error
				hist, herr = mergeBuckets(c.UserContext(), s.st, roomIDs, time.Duration(bucketSec)*time.Second, since)
				if herr == nil {
					historyCachePut(cacheKey, hist)
				}
			}
			if hist != nil {
				out["history"] = hist
				out["dbAvailable"] = true
			}
		}
		return c.JSON(out)
	})

	router.Get("/metrics/tracks", func(c *fiber.Ctx) error {
		rows := s.manager.AllTrackStats()
		if c.Query("format") != "csv" {
			return c.JSON(fiber.Map{"tracks": rows, "count": len(rows)})
		}
		cols := []string{"roomId", "id", "publisherId", "kind", "source", "codec", "payloadType",
			"muted", "recording", "packets", "bytes", "dropped", "writeFails", "subscribers",
			"pliForwarded", "pliSuppressed", "keyframes"}
		var b strings.Builder
		b.WriteString(strings.Join(cols, ","))
		b.WriteString("\n")
		for _, row := range rows {
			vals := make([]string, 0, len(cols))
			for _, col := range cols {
				vals = append(vals, fmt.Sprint(row[col]))
			}
			b.WriteString(strings.Join(vals, ","))
			b.WriteString("\n")
		}
		c.Set("Content-Type", "text/csv; charset=utf-8")
		return c.SendString(b.String())
	})

	router.Get("/healthz", func(c *fiber.Ctx) error {
		rooms, participants, tracks := s.manager.Stats()

		// 数据层状态（Ent + sqlc）。刻意**不在健康检查里 ping 数据库**：
		// 那会让健康检查的延迟被数据库绑架，而它要反映的是"进程是否活着"。
		// 这里报告的是启动时确定的状态，够用于回答"我的数据层到底有没有生效"。
		dataLayer, metricsSampling := "unavailable", false
		if s.st != nil {
			dataLayer, metricsSampling = "ok", s.cfg.MetricsInterval > 0
		}
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		pliForwarded, pliSuppressed := sfu.FrameRequestStats()
		return c.JSON(fiber.Map{
			"status":       "ok",
			"uptimeSec":    int(time.Since(s.started).Seconds()),
			"rooms":        rooms,
			"participants": participants,
			"tracks":       tracks,
			"wsConns":      s.hub.Count(),
			"goroutines":   runtime.NumGoroutine(),
			"heapAllocMB":  float64(mem.HeapAlloc) / 1024 / 1024,
			// cpuSeconds 是**累计值**：评测脚本按两个时刻的差值 ÷ 墙上时间算占用率。
			// 用它而不是让服务端自己算百分比，是为了让采样方掌握采样窗口
			// （服务端自己算会引入它自己的定时器误差）。
			"cpuSeconds": sysinfo.CPUSeconds(),
			// 这两个字段是给"我到底接没接上数据库"这个问题准备的：
			// 数据层不可用时整层会被跳过（媒体优先的设计），没有这两个字段的话
			// 从外部完全看不出来，只能靠翻启动日志猜。
			// 防护机制的计数：这些机制**必须能被看见**，否则等于不可证伪。
			// 三个数字的期望值都是 0（或恒为 0 增长）：长期增长说明有客户端异常或有 bug。
			"goroutinePanics": sfu.GoroutinePanicCount(),
			"messageFloods":   signaling.MessageFloodCount(),
			"malformedRTCP":   sfu.MalformedRTCPCount(),
			// 订阅端关键帧请求的转发量与被限流量：转发量说明弱网恢复在起作用，
			// 被限流量说明限流挡下了多少"无意义的关键帧风暴"。
			"pliForwarded":    pliForwarded,
			"pliSuppressed":   pliSuppressed,
			"dataLayer":       dataLayer,
			"metricsSampling": metricsSampling,
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
	// ---- 登录：POST /auth/login、POST /auth/logout、GET /auth/me ----
	//
	// 这三条是"谁在开会"的入口：前端拿到的会话 Cookie 是 HttpOnly 的，
	// 令牌签名密钥永远不下发到浏览器。
	router.Post("/auth/login", s.handleLogin)
	router.Post("/auth/logout", s.handleLogout)
	router.Get("/auth/me", s.handleMe)
	// 账号资料：改昵称/用户名、改密码、头像。全部要求当前口令或已登录会话。
	router.Patch("/auth/profile", s.handleUpdateProfile)
	router.Post("/auth/password", s.handleChangePassword)
	router.Post("/auth/avatar", s.handleUploadAvatar)
	router.Delete("/auth/avatar", s.handleDeleteAvatar)
	router.Get("/auth/avatar/:id", s.handleGetAvatar)

	router.Post("/token", func(c *fiber.Ctx) error {
		a := s.hub.Authenticator()
		if !a.Enabled() {
			return c.Status(fiber.StatusNotImplemented).JSON(fiber.Map{
				"error": "鉴权未启用（AUTH_MODE=off 或未配置 JWT_SECRET），当前无需令牌即可入会",
			})
		}
		// 授权顺序很重要：**先看登录会话**，再看运维密钥。
		// 会话是普通用户（前端）的凭据；X-API-Key 是脚本/运维的凭据，两者并列而不是二选一。
		// 曾经把密钥闸门放在前面，于是"已登录的用户"依然被 403 拦下——
		// 那种情况下登录界面永远拿不到令牌，闭环是断的。
		var sessUserEarly *store.User
		if u := s.sessionUser(c); u != nil {
			sessUserEarly = u
		}
		if cfg.AppEnv != "development" && sessUserEarly == nil {
			if cfg.TokenAPIKey == "" {
				return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
					"error": "服务端未配置 TOKEN_API_KEY，拒绝签发令牌",
				})
			}
			if c.Get("X-API-Key") != cfg.TokenAPIKey {
				return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "X-API-Key 无效"})
			}
		}

		// **会话优先**：有登录会话时，身份与主持人权限由服务端决定，
		// 请求体里的 identity/roomAdmin 一律忽略（否则前端自报就能当主持人）。
		// 没有会话时走原来的 X-API-Key/开发环境路径（评测脚本与本地演示用）。
		sessUser := sessUserEarly

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
		if sessUser != nil {
			// 登录用户的身份来自会话：用户名即身份，显示名优先取用户的 displayName
			req.Identity = sessUser.Username
			if req.Name == "" {
				if sessUser.DisplayName != "" {
					req.Name = sessUser.DisplayName
				} else {
					req.Name = sessUser.Username
				}
			}
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
		if sessUser != nil {
			// 主持人权限**由服务端按角色判定**：role=host 才能拿到 roomAdmin。
			// 这一行是整个登录体系里最关键的一行——前端说什么都不算数。
			grant.RoomAdmin = sessUser.Role == "host"
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
		servers := turn.Servers(cfg.StunURLs, cfg.TurnURLs, cfg.TurnSecret, identity, cfg.TurnCredTTL,
			turn.Static(cfg.ExternalTurnURLs, cfg.ExternalTurnUsername, cfg.ExternalTurnCredential)...)
		return c.JSON(fiber.Map{
			"iceServers":  servers,
			"ttlSeconds":  turn.TTLSeconds(cfg.TurnCredTTL),
			"turnEnabled": len(cfg.TurnURLs) > 0 && cfg.TurnSecret != "",
			// externalTurn=true 表示同时下发了外部 TURN 服务（静态凭证）。
			// 本机若无法对公网开放媒体端口，就靠它兜底（媒体全程走中继）。
			"externalTurn":     len(cfg.ExternalTurnURLs) > 0,
			"credentialScheme": "turn-rest-api", // coturn --use-auth-secret（外部 TURN 用静态凭证）
		})
	})

	// ---- 录制：会话列表与文件下载 ----
	//
	// 录制文件就是会议的音视频内容，属于敏感数据，所以访问控制与 /token 一致：
	// 非 development 环境必须带 X-API-Key（复用同一个运维密钥，少一个配置项）。
	recordKeyOK := func(c *fiber.Ctx) bool {
		if cfg.AppEnv == "development" {
			return true // 开发环境不拦，本地开箱可用
		}
		// 以前是 `cfg.TokenAPIKey == "" → return true`——**失败即放行**：
		// 生产一旦忘了配密钥，录制文件这份会议音视频内容就完全公开。
		// 改成失败即拒绝：没配密钥 = 只有带有效令牌的人能下（当前即无人能下），
		// 宁可下载不可用，也不能把会议内容敞开。
		if cfg.TokenAPIKey == "" {
			return false
		}
		return c.Get("X-API-Key") == cfg.TokenAPIKey
	}

	router.Get("/recordings", func(c *fiber.Ctx) error {
		if !recordKeyOK(c) {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "X-API-Key 无效"})
		}
		rec := s.manager.Recording()
		if rec == nil {
			// 没开启录制能力：明确返回 enabled=false，前端据此隐藏入口而不是报错
			return c.JSON(fiber.Map{
				"enabled":   false,
				"recording": fiber.Map{"active": false},
				"sessions":  []any{},
			})
		}
		sessions, err := rec.ListSessions()
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{
			"enabled":   true,
			"recording": s.manager.RecordingState(),
			"sessions":  sessions,
			"dir":       rec.Dir(),
		})
	})

	router.Get("/recordings/:session/:file", func(c *fiber.Ctx) error {
		if !recordKeyOK(c) {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "X-API-Key 无效"})
		}
		rec := s.manager.Recording()
		if rec == nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "服务端未开启录制"})
		}
		// 路径穿越防护：只用白名单字符，并拒绝 ..
		path, ok := rec.SessionPath(c.Params("session"), c.Params("file"))
		if !ok {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "非法文件名"})
		}
		if _, err := os.Stat(path); err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "文件不存在"})
		}
		c.Set(fiber.HeaderContentDisposition, fmt.Sprintf("attachment; filename=%q", c.Params("file")))
		return c.SendFile(path)
	})

	// ---- 媒体信令入口：<basePath>/ws ----
	signaling.Mount(router.Group("/ws"), s.hub)

	// ---- GraphQL：业务接口（房间 / 会议记录 / 指标聚合）----
	//
	// 数据层是 Ent（业务实体）+ sqlc（指标强聚合）的混合体，两者共用同一个 pgxpool；
	// resolver 只依赖 store.Store，因此接口层不需要知道某条数据来自哪一套。
	//
	// 注意 gqlgen 的 handler 是 net/http 的 http.Handler，必须经 adaptor.HTTPHandler
	// 桥接到 Fiber（Fiber 官方支持的方式）。
	gqlSrv := handler.NewDefaultServer(graph.NewExecutableSchema(graph.Config{Resolvers: graph.New(s.st)}))
	gqlHandler := adaptor.HTTPHandler(gqlSrv)
	// GraphQL 之前是**直接挂载、没有任何校验**：任何人都能调用 createRoom / closeRoom /
	// recordMetricSample，也能读到房间与会议记录。前端并不使用它（只有运维脚本/评测用它），
	// 所以非开发环境要求 X-API-Key；开发环境保持可直接用（含 GraphiQL 调试）。
	router.All("/graphql", func(c *fiber.Ctx) error {
		if cfg.AppEnv != "development" {
			if cfg.TokenAPIKey == "" || c.Get("X-API-Key") != cfg.TokenAPIKey {
				return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "GraphQL 需要有效的 X-API-Key"})
			}
		}
		return gqlHandler(c)
	})
	// GraphiQL 调试页：只在开发环境挂载，生产暴露它等于把整个 schema 摆出来给人试
	if cfg.AppEnv == "development" {
		router.Get("/playground", adaptor.HTTPHandler(playground.Handler("easylive GraphQL", "/graphql")))
	}

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
