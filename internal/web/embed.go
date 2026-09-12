package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
)

//go:embed static
var content embed.FS

func CheckBuild() error {
	if _, err := content.Open("static/vendor/model-viewer.min.js"); err != nil {
		return fmt.Errorf("缺少 3D 查看器，请先运行 npm ci --ignore-scripts && npm run build，再启动 Go 服务")
	}
	return nil
}

func Handler() http.Handler {
	files, err := fs.Sub(content, "static")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(files))
}
