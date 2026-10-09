// Package config 负责从环境变量读取配置。
// 约定：所有配置项都来自 .env（由 docker compose 的 env_file 注入），代码里只给默认值，不硬编码密钥。
package config

import (
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
	MediaUDPPort    int
	AdvertiseIP     string
	StunURLs        []string
	TurnURLs        []string
	TurnSecret      string
	ICETCPPort      int
	IncludeLoopback bool

	// 数据
	PostgresDSN string

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
}

func Load() *Config {
	c := &Config{
		AppName:             getStr("APP_NAME", "easylive-api"),
		AppPort:             getStr("APP_PORT", "8080"),
		AppBasePath:         strings.TrimRight(getStr("APP_BASE_PATH", ""), "/"),
		AppEnv:              getStr("APP_ENV", "development"),
		LogLevel:            getStr("LOG_LEVEL", "debug"),
		MediaUDPPort:        getInt("MEDIA_UDP_PORT", 7881),
		AdvertiseIP:         getStr("SFU_ADVERTISE_IP", ""),
		StunURLs:            getList("STUN_URLS", "stun:stun.l.google.com:19302"),
		TurnURLs:            getList("TURN_URLS", ""),
		TurnSecret:          getStr("TURN_SECRET", ""),
		ICETCPPort:          getInt("ICE_TCP_PORT", 7882),
		IncludeLoopback:     getBool("ICE_INCLUDE_LOOPBACK", true),
		PostgresDSN:         buildDSN(),
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
	}
	c.WebOrigins = append(c.WebOrigins, getList("WEB_EXTRA_ORIGINS", "")...)
	return c
}

// LogSummary 打印关键配置（不打印任何密钥），便于容器里一眼确认环境是否正确。
func (c *Config) LogSummary() {
	log.Printf("config: env=%s port=%s basePath=%q", c.AppEnv, c.AppPort, c.AppBasePath)
	log.Printf("config: media udp=%d advertiseIP=%q stun=%v turn=%d 个", c.MediaUDPPort, c.AdvertiseIP, c.StunURLs, len(c.TurnURLs))
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
