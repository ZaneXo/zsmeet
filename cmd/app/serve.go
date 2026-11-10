package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZaneXo/zsmeet/internal/api"
	"github.com/ZaneXo/zsmeet/internal/config"
	"github.com/ZaneXo/zsmeet/internal/db"
	"github.com/ZaneXo/zsmeet/internal/db/sqlc"
	"github.com/ZaneXo/zsmeet/internal/pipeline"
	"github.com/ZaneXo/zsmeet/internal/queue"
	"github.com/ZaneXo/zsmeet/internal/secret"
	"github.com/ZaneXo/zsmeet/internal/service"
	"github.com/ZaneXo/zsmeet/internal/settings"
	"github.com/ZaneXo/zsmeet/internal/storage"
	"github.com/ZaneXo/zsmeet/web"
)

// version 在发布构建时用 -ldflags "-X main.version=..." 写入。
var version = "dev"

// deps 是 serve 和 worker 都要用的东西。
type deps struct {
	cfg      config.Config
	pool     *pgxpool.Pool
	store    *storage.Local
	settings *settings.Store
}

func setup(ctx context.Context) (*deps, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	box, err := secret.New(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	store, err := storage.NewLocal(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	// api 和 worker 谁先启动谁迁移，另一个等锁，拿到锁时发现已经是最新的
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &deps{cfg: cfg, pool: pool, store: store, settings: settings.New(sqlc.New(pool), box)}, nil
}

func serve(ctx context.Context) error {
	d, err := setup(ctx)
	if err != nil {
		return err
	}
	defer d.pool.Close()

	inserter, err := queue.NewInserter(d.pool)
	if err != nil {
		return err
	}
	svc := service.New(d.pool, d.store, d.settings, inserter)

	sess := api.NewSessions(pgxstore.New(d.pool), d.cfg.SecureCookie)

	srv := &http.Server{
		Addr:    d.cfg.Listen,
		Handler: api.NewHandler(svc, sess, web.FS(), version),
		// 不设整体的读写超时：进度推送是长连接，录音回放和分片上传也可能很慢
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("接口服务已启动", "listen", d.cfg.Listen, "version", version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("正在停止接口服务")
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(stopCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func work(ctx context.Context) error {
	d, err := setup(ctx)
	if err != nil {
		return err
	}
	defer d.pool.Close()

	// 任务用自己的 context：收到退出信号时由 StopAndCancel 负责取消，
	// 这样 River 能把没做完的任务记成「稍后重试」
	client, err := pipeline.Start(context.WithoutCancel(ctx), d.pool, d.cfg, d.store, d.settings)
	if err != nil {
		return err
	}
	slog.Info("后台处理已启动", "concurrency", d.cfg.Concurrency, "asr_threads", d.cfg.ASRThreads, "version", version)

	<-ctx.Done()
	slog.Info("正在停止后台处理，没做完的会议会在下次启动后继续")
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.StopAndCancel(stopCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func printOpenAPI() error {
	spec, err := api.OpenAPI(version)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, string(spec))
	return err
}
