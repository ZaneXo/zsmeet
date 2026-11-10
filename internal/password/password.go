// Package password 用 argon2id 保存和校验登录密码。
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
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

// 算一次哈希要占 19 MiB 内存、跑满一个核。同时算的个数不超过 CPU 核数（容器限了核数的话按限的算）：
// 再多也不会更快，只会让一波同时来的登录请求把内存吃光。多出来的排队等着
var slots = make(chan struct{}, max(runtime.GOMAXPROCS(0), 1))

// idKey 是排着队的 argon2.IDKey。
func idKey(plain, salt []byte, t, m uint32, p uint8, n uint32) []byte {
	slots <- struct{}{}
	defer func() { <-slots }()
	return argon2.IDKey(plain, salt, t, m, p, n)
}

// Hash 返回 PHC 格式的哈希，参数和盐都在字符串里，以后调参数不影响旧密码。
func Hash(plain string) string {
	salt := make([]byte, saltLen)
	rand.Read(salt)
	key := idKey([]byte(plain), salt, passes, memoryKiB, threads, keyLen)
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
	got := idKey([]byte(plain), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
