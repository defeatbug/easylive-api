// Package config 负责从环境变量读取配置。
// 约定：所有配置项都来自 .env（由 docker compose 的 env_file 注入），代码里只给默认值，不硬编码密钥。
package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AppName     string
	AppPort     string
	AppBasePath string
	AppEnv      string
	LogLevel    string

	// 媒体面
	MediaUDPPort int
	AdvertiseIP  string
	StunURLs     []string
	TurnURLs     []string
	// ExternalTurn* 是可选的**静态凭证**外部 TURN 服务。
	//
	// 用途（重要）：本机若是 NAT 型 VPS——公网只放行了 22/80/443、媒体端口无法开放——
	// 那么"直连候选"对公网访客永远不可达。此时让 SFU 自己以 TURN 客户端身份申请中继候选，
	// 媒体就变成「客户端 ↔ 公网 TURN ↔ SFU」，**不需要本机开放任何入向端口**。
	// 客户端也会拿到同一组 TURN（可选），用于它们自己也需要中继的网络。
	ExternalTurnURLs       []string
	ExternalTurnUsername   string
	ExternalTurnCredential string
	TurnSecret             string
	// TurnCredTTL 是下发给客户端的 TURN 临时凭证有效期。
	// 短一点更安全（泄漏窗口小），但要大于一次会议的典型时长，否则中途失效会掉中继。
	TurnCredTTL     time.Duration
	ICETCPPort      int
	IncludeLoopback bool

	// 数据
	PostgresDSN string
	// MigrationsDir 是迁移文件目录（服务启动时按文件名顺序执行，幂等）
	MigrationsDir string
	// AutoMigrate 决定启动时是否自动执行迁移。
	// 默认开启：全新部署若没人手动跑迁移，业务接口会以"表不存在"告终，
	// 而媒体功能完全正常——这种组合极易被误判成"接口写错了"。
	AutoMigrate bool
	// RecordEnabled 打开会议录制：把每条视频轨的 RTP **原样落盘**（IVF/VP8，零转码）。
	// 默认关闭——录制要写磁盘，是否开启属于部署决策。
	RecordEnabled bool
	// RecordDir 录制文件输出目录
	RecordDir string
	// MetricsInterval 指标采样周期（0 表示不采样）
	MetricsInterval time.Duration
	// MetricsRetention 指标保留时长（<=0 表示不清理）
	MetricsRetention time.Duration

	// 信令保护
	WSMaxConnections    int
	HTTPRateLimitPerIP  int
	WSHeartbeat         time.Duration
	WSSessionTTL        time.Duration
	RoomMaxParticipants int
	// ResumeGrace 是连接断开后保留参与者身份与轨道的宽限期（0 = 立即离会）。
	// 取值依据见 ADR：小于 10s 会让弱网抖动直接变成"离会重入"，大于 60s 会长期占住人数上限。
	ResumeGrace time.Duration

	// Fiber 服务端参数。长连接（WebSocket）场景下读写超时保持 0（不限制），
	// 活性检测交给 WebSocket 层的 ping/pong deadline。
	FiberConcurrency  int
	FiberReadTimeout  time.Duration
	FiberWriteTimeout time.Duration

	// 前端来源（CORS）
	WebOrigins []string

	// 调试端点开关（/debug/stats：各参与者的连接状态与 RTP 计数）
	EnableDebugEndpoint bool

	// 鉴权（准入与权限模型）
	//
	// AuthMode 三态：off（默认，不校验）/ optional（带令牌则校验）/ required（必须带）。
	// 之所以默认 off：开题与答辩演示常在局域网、无 HTTPS 环境下进行，必须能一键跑通；
	// 部署到公网时把 AUTH_MODE 改成 required 即可，代码不用动。
	AuthMode    string
	JWTSecret   string
	TokenTTL    time.Duration
	TokenAPIKey string
}

func Load() *Config {
	c := &Config{
		AppName:                getStr("APP_NAME", "easylive-api"),
		AppPort:                getStr("APP_PORT", "8080"),
		AppBasePath:            strings.TrimRight(getStr("APP_BASE_PATH", ""), "/"),
		AppEnv:                 getStr("APP_ENV", "development"),
		LogLevel:               getStr("LOG_LEVEL", "debug"),
		MediaUDPPort:           getInt("MEDIA_UDP_PORT", 7881),
		AdvertiseIP:            getStr("SFU_ADVERTISE_IP", ""),
		StunURLs:               getList("STUN_URLS", "stun:stun.l.google.com:19302"),
		TurnURLs:               getList("TURN_URLS", ""),
		TurnSecret:             getStr("TURN_SECRET", ""),
		ExternalTurnURLs:       getList("EXTERNAL_TURN_URLS", ""),
		ExternalTurnUsername:   getStr("EXTERNAL_TURN_USERNAME", ""),
		ExternalTurnCredential: getStr("EXTERNAL_TURN_CREDENTIAL", ""),
		TurnCredTTL:            getDuration("TURN_CRED_TTL", time.Hour),
		ICETCPPort:             getInt("ICE_TCP_PORT", 7882),
		IncludeLoopback:        getBool("ICE_INCLUDE_LOOPBACK", true),
		PostgresDSN:            buildDSN(),
		MigrationsDir:          getStr("MIGRATIONS_DIR", "migrations"),
		// 诊断端点（/debug/stats）：默认关闭——它会调用 GetStats()，属于"耗时的观测"，
		// 不该在生产常态开启；需要排查媒体问题时临时打开。
		// 之前这里漏了赋值，导致端点写了却永远打不开（排查时白折腾了一轮）。
		EnableDebugEndpoint: getBool("ENABLE_DEBUG_ENDPOINT", false),
		AutoMigrate:         getBool("AUTO_MIGRATE", true),
		RecordEnabled:       getBool("RECORD_ENABLED", false),
		RecordDir:           getStr("RECORD_DIR", "recordings"),
		MetricsInterval:     getDuration("METRICS_INTERVAL", 5*time.Second),
		MetricsRetention:    getDuration("METRICS_RETENTION", 72*time.Hour),
		WSMaxConnections:    getInt("WS_MAX_CONNECTIONS", 200),
		HTTPRateLimitPerIP:  getInt("HTTP_RATE_LIMIT_PER_IP", 50),
		WSHeartbeat:         getDuration("WS_HEARTBEAT_INTERVAL", 30*time.Second),
		WSSessionTTL:        getDuration("WS_SESSION_TTL", 24*time.Hour),
		RoomMaxParticipants: getInt("ROOM_MAX_PARTICIPANTS", 16),
		ResumeGrace:         getDuration("RESUME_GRACE", 30*time.Second),
		FiberConcurrency:    getInt("FIBER_CONCURRENCY", 256*1024),
		FiberReadTimeout:    getDuration("FIBER_READ_TIMEOUT", 0),
		FiberWriteTimeout:   getDuration("FIBER_WRITE_TIMEOUT", 0),
		WebOrigins:          getList("WEB_ORIGIN", "http://localhost:5173"),
		AuthMode:            getStr("AUTH_MODE", "off"),
		JWTSecret:           getStr("JWT_SECRET", ""),
		TokenTTL:            getDuration("TOKEN_TTL", 12*time.Hour),
		TokenAPIKey:         getStr("TOKEN_API_KEY", ""),
	}
	c.WebOrigins = append(c.WebOrigins, getList("WEB_EXTRA_ORIGINS", "")...)
	return c
}

// ComposeHostHint 检查一个很常见、也很难自查的配置错误：
// 在**宿主机**上跑服务，但 POSTGRES_HOST 填的是 docker compose 的服务名
// （postgresql / postgres / db）。这个域名只在 compose 网络里能解析，
// 宿主机上必然连不上——而失败表现只是"数据层被跳过"，看起来就像根本没接数据库。
//
// 返回非空字符串表示命中了这种情况，调用方把它打给用户。
func (c *Config) ComposeHostHint() string {
	host := strings.ToLower(strings.TrimSpace(os.Getenv("POSTGRES_HOST")))
	switch host {
	case "postgres", "postgresql", "db", "database":
	default:
		return ""
	}
	// 容器内（有 /.dockerenv 或 /.containerenv）这是正确写法，不需要提示
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return ""
	}
	if _, err := os.Stat("/.containerenv"); err == nil {
		return ""
	}
	return fmt.Sprintf(
		"POSTGRES_HOST=%s 只在 docker compose 网络内可解析，看起来你是在宿主机上直接跑。"+
			"宿主机请改用 POSTGRES_HOST=127.0.0.1（或直接设 DATABASE_URL）。",
		host)
}

// LogSummary 打印关键配置（不打印任何密钥），便于容器里一眼确认环境是否正确。
func (c *Config) LogSummary() {
	log.Printf("config: env=%s port=%s basePath=%q", c.AppEnv, c.AppPort, c.AppBasePath)
	log.Printf("config: media udp=%d iceTcp=%d advertiseIP=%q stun=%v turn=%d 个（临时凭证 %s）",
		c.MediaUDPPort, c.ICETCPPort, c.AdvertiseIP, c.StunURLs, len(c.TurnURLs), c.TurnCredTTL)
	if len(c.ExternalTurnURLs) > 0 {
		log.Printf("config: 外部 TURN 已配置 %d 个地址（SFU 与客户端都会用它申请中继候选）", len(c.ExternalTurnURLs))
	}
	if len(c.TurnURLs) > 0 && c.TurnSecret == "" {
		log.Printf("config: 警告 配置了 TURN_URLS 但没有 TURN_SECRET，将不下发 TURN（无凭证的中继只会拖慢建连）")
	}
	// 只打印"是否配置"，绝不打印密钥本身
	if c.AuthMode != "off" && c.JWTSecret == "" {
		log.Printf("config: 警告 AUTH_MODE=%s 但未配置 JWT_SECRET，鉴权将降级为不校验", c.AuthMode)
	}
	log.Printf("config: auth mode=%s secret=%v tokenTTL=%s", c.AuthMode, c.JWTSecret != "", c.TokenTTL)
	if c.RecordEnabled {
		log.Printf("config: 会议录制已开启，输出目录 %s（每轨一个 IVF 文件，零转码）", c.RecordDir)
	}
	log.Printf("config: db autoMigrate=%v migrationsDir=%s metricsInterval=%s retention=%s",
		c.AutoMigrate, c.MigrationsDir, c.MetricsInterval, c.MetricsRetention)
	log.Printf("config: ws maxConn=%d roomMax=%d heartbeat=%s resumeGrace=%s origins=%v",
		c.WSMaxConnections, c.RoomMaxParticipants, c.WSHeartbeat, c.ResumeGrace, c.WebOrigins)
}

func getStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func getInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("config: %s=%q 不是整数，使用默认值 %d", key, os.Getenv(key), def)
	}
	return def
}

func getBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func getDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("config: %s=%q 不是合法 duration（如 30s），使用默认值 %s", key, os.Getenv(key), def)
	}
	return def
}

// getList 支持逗号分隔；空字符串返回空切片。
func getList(key, def string) []string {
	raw := getStr(key, def)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func buildDSN() string {
	host := getStr("POSTGRES_HOST", "localhost")
	port := getStr("POSTGRES_PORT", "5432")
	user := getStr("POSTGRES_USER", "postgres")
	pass := getStr("POSTGRES_PASSWORD", "postgres")
	name := getStr("POSTGRES_DB_NAME", "sfu_dev")
	return "postgres://" + user + ":" + pass + "@" + host + ":" + port + "/" + name + "?sslmode=disable"
}
