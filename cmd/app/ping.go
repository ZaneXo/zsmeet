package main

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// ping 检查本机的接口服务是否在正常响应，给容器的健康检查用（精简镜像里没有 curl）。
func ping(ctx context.Context) error {
	_, port, err := net.SplitHostPort(cmp.Or(os.Getenv("ZSMEET_LISTEN"), ":8080"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/api/status", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("接口返回 %d", resp.StatusCode)
	}
	return nil
}
