package main

import (
	"cmp"
	"context"
	"log/slog"
	"os"

	"github.com/ZaneXo/zsmeet/internal/models"
)

// downloadModels 不需要数据库，只看模型目录和镜像地址这两个环境变量。
// 构建自带模型的镜像、或者想在第一场会议之前把模型备好时用。
func downloadModels(ctx context.Context) error {
	dir := cmp.Or(os.Getenv("ZSMEET_MODELS_DIR"), "./models")
	mirror := cmp.Or(os.Getenv("ZSMEET_MODEL_MIRROR"), "https://huggingface.co")
	if err := models.Ensure(ctx, dir, mirror, logDownload()); err != nil {
		return err
	}
	slog.Info("识别模型已就绪", "dir", dir)
	return nil
}

// logDownload 是下载模型时的进度回调：每下载 10% 记一行日志。
func logDownload() func(done, total int64) {
	lastPct := int64(-1)
	return func(done, total int64) {
		if pct := done * 100 / total; pct >= lastPct+10 {
			lastPct = pct
			slog.Info("下载识别模型", "percent", pct, "mb", done>>20, "total_mb", total>>20)
		}
	}
}
