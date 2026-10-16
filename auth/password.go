package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ErrBadHash 表示数据库里的口令哈希格式无法识别。
var ErrBadHash = errors.New("口令哈希格式不正确")

// argon2id 参数：这些是 OWASP 在 2024 年前后推荐的量级
// （内存 64MiB、迭代 1 次、并行 4），在笔记本上单次约几十毫秒——
// 对登录接口足够快，对离线爆破足够贵。
const (
	argonMemoryKiB = 64 * 1024
	argonTime      = 1
	argonThreads   = 4
	argonKeyLen    = 32
	argonSaltLen   = 16
)

// HashPassword 生成 PHC 格式的 argon2id 哈希字符串（含随机盐与参数）。
//
// 为什么把参数写进哈希串：将来调参（例如内存加到 128MiB）时，
// 老用户的哈希仍能按其自带参数校验通过，不需要强制所有人改密码。
func HashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", fmt.Errorf("口令至少 8 个字符")
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成盐失败: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword 校验明文口令与 PHC 哈希是否匹配。
//
// 比较用 subtle.ConstantTimeCompare：普通 == 会在第一个不同字节处返回，
// 逐字节的耗时差足以被用来还原哈希。
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// 形如 ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, key]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrBadHash
	}
	if version != argon2.Version {
		return false, fmt.Errorf("不支持的 argon2 版本: %d", version)
	}
	var memory uint32
	var timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false, ErrBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrBadHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrBadHash
	}
	got := argon2.IDKey([]byte(password), salt, timeCost, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash 用于"用户不存在"时也走一遍完整校验，避免通过响应耗时枚举用户名。
var dummyHash = func() string {
	h, err := HashPassword("dummy-password-for-timing")
	if err != nil {
		panic(err)
	}
	return h
}()

// DummyVerify 在用户不存在时调用，让耗时与"用户存在但口令错"一致。
func DummyVerify(password string) {
	_, _ = VerifyPassword(password, dummyHash)
}
