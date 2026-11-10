// Package password 用 argon2id 保存和校验登录密码。
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// OWASP 推荐的 argon2id 最低参数：19 MiB 内存、2 轮、1 线程
const (
	memoryKiB = 19 * 1024
	passes    = 2
	threads   = 1
	saltLen   = 16
	keyLen    = 32
)

var b64 = base64.RawStdEncoding

// Hash 返回 PHC 格式的哈希，参数和盐都在字符串里，以后调参数不影响旧密码。
func Hash(plain string) string {
	salt := make([]byte, saltLen)
	rand.Read(salt)
	key := argon2.IDKey([]byte(plain), salt, passes, memoryKiB, threads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memoryKiB, passes, threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

var errFormat = errors.New("密码哈希格式不对")

// Verify 校验密码。哈希格式不对时返回错误，密码不对时返回 false。
func Verify(plain, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errFormat
	}
	var version int
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errFormat
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, errFormat
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, errFormat
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, errFormat
	}
	got := argon2.IDKey([]byte(plain), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
