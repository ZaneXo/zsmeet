// Package secret 用部署时配置的主密钥加密存进数据库的凭证（API Key、App Secret）。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

type Box struct{ aead cipher.AEAD }

// derivePrefix 混进密钥派生里的固定前缀。它不是模块路径，永远不能改：
// 一改，派生出的密钥就变了，数据库里已经存的凭证全都解不开（TestOpensEarlierCiphertext 守着它）。
const derivePrefix = "zsmeet/secret/v1:"

// New 用主密钥派生 AES-256-GCM 密钥。
func New(masterKey string) (*Box, error) {
	key := sha256.Sum256([]byte(derivePrefix + masterKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal 加密，返回 base64。空字符串原样返回，表示没设置。
func (b *Box) Seal(plain string) string {
	if plain == "" {
		return ""
	}
	nonce := make([]byte, b.aead.NonceSize())
	rand.Read(nonce)
	return base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(plain), nil))
}

var ErrDecrypt = errors.New("解不开已保存的凭证：ZSMEET_SECRET_KEY 可能被改过，请在设置里重新填写")

func (b *Box) Open(sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", ErrDecrypt
	}
	n := b.aead.NonceSize()
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plain), nil
}

// SealFor 加密一段只在浏览器里存一会儿的东西（比如用飞书登录时的 state），返回 URL 安全的 base64，
// 可以直接放进 Cookie。purpose 说明它是做什么用的，一起参与校验：同一把钥匙加密的别的东西
// （数据库里的凭证、另一种用途的 Cookie）换到这里来是解不开的。
func (b *Box) SealFor(purpose, plain string) string {
	nonce := make([]byte, b.aead.NonceSize())
	rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(plain), []byte(purpose)))
}

// ErrTampered 表示 SealFor 加密的东西被改过、用途不对，或者根本不是它加密的。
var ErrTampered = errors.New("内容被改过，或者不是这个用途的")

// OpenFor 解开 SealFor 加密的东西。purpose 要和加密时的一样。
func (b *Box) OpenFor(purpose, sealed string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	n := b.aead.NonceSize()
	if err != nil || len(raw) < n {
		return "", ErrTampered
	}
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], []byte(purpose))
	if err != nil {
		return "", ErrTampered
	}
	return string(plain), nil
}
