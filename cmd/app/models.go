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
	lastPct := int64(-1)
	err := models.Ensure(ctx, dir, mirror, func(done, total int64) {
		if pct := done * 100 / total; pct >= lastPct+10 {
			lastPct = pct
			slog.Info("下载识别模型", "percent", pct, "mb", done>>20, "total_mb", total>>20)
		}
	})
	if err != nil {
		return err
	}
	slog.Info("识别模型已就绪", "dir", dir)
	return nil
}
