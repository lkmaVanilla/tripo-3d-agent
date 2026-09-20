// Package web 把最小前端和本地 3D 查看器一起嵌入 Go 程序，运行时不依赖前端开发服务器。
package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
)

// content 在编译时固定静态文件；修改前端后需要重新构建 Go 程序才会生效。
//
//go:embed static
var content embed.FS

// CheckBuild 在启动时发现漏掉查看器构建步骤的情况，避免进入页面后才出现资源缺失。
func CheckBuild() error {
	if _, err := content.Open("static/vendor/model-viewer.min.js"); err != nil {
		return fmt.Errorf("缺少 3D 查看器，请先运行 npm ci --ignore-scripts && npm run build，再启动 Go 服务")
	}
	return nil
}

// Handler 以 static 为 URL 根目录提供嵌入资源；会话 API 的访问控制由 app 包负责。
func Handler() http.Handler {
	files, err := fs.Sub(content, "static")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(files))
}
