//go:build !embedweb

package web

import (
	"io/fs"
	"testing/fstest"
)

const placeholder = `<!doctype html>
<meta charset="utf-8">
<title>zsmeet</title>
<body style="font-family: system-ui; max-width: 560px; margin: 80px auto; line-height: 1.7">
<h1>zsmeet 接口已启动</h1>
<p>这个程序构建时没有带上网页。开发时在 web 目录运行 <code>pnpm dev</code>，然后打开它提示的地址；
发布版本用 <code>make build</code> 或 Docker 镜像，会把网页一起打进来。</p>
<p>接口文档在 <a href="/api/docs">/api/docs</a>。</p>
`

// FS 在没有嵌入前端时返回一个说明页，让后端可以单独编译和运行。
func FS() fs.FS {
	return fstest.MapFS{"index.html": {Data: []byte(placeholder)}}
}
