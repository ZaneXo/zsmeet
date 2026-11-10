// Package config 从环境变量读取部署配置。
//
// 只放部署时必须知道的东西（数据库、目录、密钥）。大模型、飞书这类团队配置
// 由管理员在网页里填，存在数据库里，不在这里。
package config

import (
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

type Config struct {
	// Postgres 连接串
	DatabaseURL string
	// 监听地址
	Listen string
	// 录音、分片等文件的根目录
	DataDir string
	// 识别模型目录
	ModelsDir string
	// 用来加密数据库里的凭证；丢了就解不开已保存的 API Key
	SecretKey string
	// 下载模型用的 Hugging Face 地址，国内可以设成 https://hf-mirror.com
	ModelMirror string
	// 一个 worker 进程同时处理几场会议（识别仍然一场一场来，并行的是写纪要）
	Concurrency int
	// 识别用几个 CPU 线程
	ASRThreads int
	// 识别的语言：zh（默认，中文会议里夹着英文也能认）、en、ja、ko、yue，或者 auto（每一小段自动判断）。
	// auto 会把很短的「嗯」「哦」认成日文，中文团队不要用
	ASRLanguage string
	// 只通过 HTTPS 访问时设为 true，Cookie 会带 Secure
	SecureCookie bool
	// 飞书开放平台的地址。一般不用设：按网页上选的飞书 / Lark 决定。私有化部署的飞书、测试时才用
	FeishuBaseURL string
	// 前面的反向代理的地址（ZSMEET_TRUSTED_PROXIES）。只有从这些地址连过来的请求，才采用 X-Forwarded-For 里记的访问者 IP。
	// 默认只信本机：反向代理和 zsmeet 在同一台机器上、不经过 Docker 的时候
	TrustedProxies []netip.Prefix
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:   os.Getenv("ZSMEET_DATABASE_URL"),
		Listen:        cmp.Or(os.Getenv("ZSMEET_LISTEN"), ":8080"),
		DataDir:       cmp.Or(os.Getenv("ZSMEET_DATA_DIR"), "./data"),
		ModelsDir:     cmp.Or(os.Getenv("ZSMEET_MODELS_DIR"), "./models"),
		SecretKey:     os.Getenv("ZSMEET_SECRET_KEY"),
		ModelMirror:   cmp.Or(os.Getenv("ZSMEET_MODEL_MIRROR"), "https://huggingface.co"),
		Concurrency:   envInt("ZSMEET_CONCURRENCY", 2),
		ASRThreads:    envInt("ZSMEET_ASR_THREADS", min(runtime.NumCPU(), 4)),
		ASRLanguage:   cmp.Or(os.Getenv("ZSMEET_ASR_LANGUAGE"), "zh"),
		SecureCookie:  os.Getenv("ZSMEET_SECURE_COOKIE") == "true",
		FeishuBaseURL: os.Getenv("ZSMEET_FEISHU_BASE_URL"),
	}
	var errs []error
	proxies, err := ParseTrustedProxies(os.Getenv("ZSMEET_TRUSTED_PROXIES"))
	if err != nil {
		errs = append(errs, err)
	}
	c.TrustedProxies = proxies
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("缺少 ZSMEET_DATABASE_URL"))
	}
	if !slices.Contains([]string{"zh", "en", "ja", "ko", "yue", "auto"}, c.ASRLanguage) {
		errs = append(errs, errors.New("ZSMEET_ASR_LANGUAGE 只能是 zh、en、ja、ko、yue 或 auto"))
	}
	if len(c.SecretKey) < 16 {
		errs = append(errs, errors.New("ZSMEET_SECRET_KEY 至少 16 个字符，可以用 `openssl rand -hex 32` 生成"))
	}
	return c, errors.Join(errs...)
}

// ParseTrustedProxies 解析 ZSMEET_TRUSTED_PROXIES：用逗号或空格隔开的网段（172.16.0.0/12）或者单个 IP。
// 没设时只信本机（127.0.0.0/8 和 ::1）。
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(fields) == 0 {
		return []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}, nil
	}
	var out []netip.Prefix
	for _, f := range fields {
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("ZSMEET_TRUSTED_PROXIES 里的 %q 不是 IP 地址或者网段（例如 172.16.0.0/12）", f)
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// envInt 读一个正整数环境变量，没设或者不合法时用默认值。
func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}
