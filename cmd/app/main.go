// zsmeet：开源、自部署的会议纪要系统。
//
//	zsmeet serve     启动网页和接口
//	zsmeet worker    启动后台处理（转写、写纪要）
//	zsmeet models    提前把识别模型下载好
//	zsmeet openapi   输出接口文档（OpenAPI JSON）
//	zsmeet ping      检查本机的接口服务是否正常（容器健康检查用）
//
// 在服务器上救急用（没有人能登录管理员账号时）：
//
//	zsmeet reset-link 邮箱   给这个账号生成重置密码的链接
//	zsmeet make-admin 邮箱   把这个账号设为管理员
//	zsmeet unlock 邮箱       解开这个账号的登录锁（密码输错太多次被锁住时）
//
// 开发者调识别参数用：
//
//	zsmeet transcribe 录音文件 [选项]   用和后台处理一样的流程转写一个文件，打印每一句和统计（见 transcribe.go）
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	// 把时区数据打进程序里，精简的容器镜像里没有时区文件
	_ "time/tzdata"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "serve":
		err = serve(ctx)
	case "worker":
		err = work(ctx)
	case "models":
		err = downloadModels(ctx)
	case "openapi":
		err = printOpenAPI()
	case "ping":
		err = ping(ctx)
	case "reset-link":
		err = resetLink(ctx, os.Args[2:])
	case "make-admin":
		err = makeAdmin(ctx, os.Args[2:])
	case "unlock":
		err = unlock(ctx, os.Args[2:])
	case "transcribe":
		err = transcribeFile(ctx, os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "用法：zsmeet serve | worker | models | openapi | ping | reset-link 邮箱 | make-admin 邮箱 | unlock 邮箱 | transcribe 录音文件")
		os.Exit(2)
	}
	if err != nil {
		slog.Error("退出", "err", err)
		os.Exit(1)
	}
}
