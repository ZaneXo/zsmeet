//go:build embedweb

// Package web 把打包好的前端（web/dist）嵌进程序里。
// 发布版本用 -tags embedweb 构建；不带这个标签时用 placeholder.go 里的占位页。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS 返回前端的静态文件。
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
