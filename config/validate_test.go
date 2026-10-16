package config

import (
	"os"
	"strings"
	"testing"
)

// TestValidateFailsClosed AUTH_MODE 要求校验却没有密钥时，必须**拒绝启动**。
//
// 这条以前是"打日志降级为不校验"，而"不校验"在服务端等价于全权限（含 roomAdmin）——
// 也就是一次环境变量漏配，会让所有参会者都变成主持人。安全配置必须失败即拒绝启动。
func TestValidateFailsClosed(t *testing.T) {
	t.Setenv("AUTH_MODE", "")
	t.Setenv("JWT_SECRET", "")

	// required 但缺密钥：必须报错
	c := &Config{AppEnv: "production", AuthMode: "required", JWTSecret: ""}
	if err := c.Validate(); err == nil {
		t.Fatal("required 缺密钥应当拒绝启动")
	}
	// 密钥太短也拒绝（避免用 "1" 这种凑数的密钥）
	c = &Config{AppEnv: "production", AuthMode: "required", JWTSecret: "short"}
	if err := c.Validate(); err == nil {
		t.Fatal("密钥过短应当拒绝启动")
	}
	// 配置齐备：通过
	c = &Config{AppEnv: "production", AuthMode: "required", JWTSecret: strings.Repeat("k", 32)}
	if err := c.Validate(); err != nil {
		t.Fatalf("配置齐备不应报错: %v", err)
	}
	// 未知模式：拒绝（避免拼错 AUTH_MODE=requried 变成"不校验"）
	// 注意要把 env 也设成同一个拼错值：Validate 用 env 判断"是否显式设置过"，
	// 只在结构体上填值会被"未设置即 required"的默认逻辑覆盖掉（第一版就是这么写错的）。
	t.Setenv("AUTH_MODE", "requried")
	c = &Config{AppEnv: "production", AuthMode: "requried", JWTSecret: strings.Repeat("k", 32)}
	if err := c.Validate(); err == nil {
		t.Fatal("未知 AUTH_MODE 应当拒绝启动")
	}
}

// TestValidateSecureDefaultOutsideDev 非开发环境未设置 AUTH_MODE 时按 required 处理。
//
// "忘了配"必须走失败路径，而不是变成"完全开放"。
func TestValidateSecureDefaultOutsideDev(t *testing.T) {
	t.Setenv("AUTH_MODE", "") // 未设置
	t.Setenv("JWT_SECRET", "")
	c := &Config{AppEnv: "production", AuthMode: "off"}
	err := c.Validate()
	if err == nil {
		t.Fatal("非开发环境未设 AUTH_MODE 且无密钥时应当拒绝启动（安全默认）")
	}
	if c.AuthMode != "required" {
		t.Fatalf("应当被提升为 required，实际 %q", c.AuthMode)
	}

	// 开发环境：off 是允许的（本地演示开箱可用）
	c = &Config{AppEnv: "development", AuthMode: "off"}
	if err := c.Validate(); err != nil {
		t.Fatalf("开发环境 off 不应报错: %v", err)
	}

	// 非开发环境**显式** off：允许但要告警（不打断既有部署），仅确认不报错
	_ = os.Setenv("AUTH_MODE", "off")
	c = &Config{AppEnv: "production", AuthMode: "off"}
	if err := c.Validate(); err != nil {
		t.Fatalf("显式 off 应当允许（告警而非拒绝），实际 %v", err)
	}
}
