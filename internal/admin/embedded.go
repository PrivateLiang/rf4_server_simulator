package admin

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:web
var webRoot embed.FS

// WebFS 返回内嵌前端根（web/）。
func WebFS() fs.FS {
	sub, err := fs.Sub(webRoot, "web")
	if err != nil {
		return webRoot
	}
	return sub
}

// WebHandler 提供内嵌前端静态文件。AI 与 app/ 源文件走 no-cache（避免旧 JS 配新 HTML），
// 其他路径保持 http.FileServer 的原有缓存语义不变。
func WebHandler(webFS fs.FS) http.Handler {
	files := http.FileServer(http.FS(webFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 新后台由 web/app/ 下多个 ES 模块组成，必须整体一起更新，否则会出现新页面配旧模块。
		if strings.HasPrefix(r.URL.Path, "/ai/") || strings.HasPrefix(r.URL.Path, "/app/") {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
