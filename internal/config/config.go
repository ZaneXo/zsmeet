// Package config 从环境变量读取部署配置。
//
// 只放部署时必须知道的东西（数据库、目录、密钥）。大模型、飞书这类团队配置
// 由管理员在网页里填，存在数据库里，不在这里。
package config

import (
	"cmp"
	"errors"
	"os"
	"runtime"
	"slices"
	"strconv"
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
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:  os.Getenv("ZSMEET_DATABASE_URL"),
		Listen:       cmp.Or(os.Getenv("ZSMEET_LISTEN"), ":8080"),
		DataDir:      cmp.Or(os.Getenv("ZSMEET_DATA_DIR"), "./data"),
		ModelsDir:    cmp.Or(os.Getenv("ZSMEET_MODELS_DIR"), "./models"),
		SecretKey:    os.Getenv("ZSMEET_SECRET_KEY"),
		ModelMirror:  cmp.Or(os.Getenv("ZSMEET_MODEL_MIRROR"), "https://huggingface.co"),
		Concurrency:  envInt("ZSMEET_CONCURRENCY", 2),
		ASRThreads:   envInt("ZSMEET_ASR_THREADS", min(runtime.NumCPU(), 4)),
		ASRLanguage:  cmp.Or(os.Getenv("ZSMEET_ASR_LANGUAGE"), "zh"),
		SecureCookie: os.Getenv("ZSMEET_SECURE_COOKIE") == "true",
	}
	var errs []error
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

// envInt 读一个正整数环境变量，没设或者不合法时用默认值。
func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}
