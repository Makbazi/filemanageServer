// Package adminui 内嵌管理员 Web 界面（需求23）。
// 静态页面（纯 HTML/JS/CSS，无构建链）经 go:embed 打进二进制，
// 浏览器访问 http://ip:port/admin/ 即可管理用户、用户组、文件夹权限、分享链接。
package adminui

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed all:web
var webFS embed.FS

// Mount 把管理界面挂到 gin 引擎的 /admin 路径（静态行为与原 net/http 一致）。
func Mount(e *gin.Engine) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("adminui: " + err.Error())
	}
	fileServer := http.FileServerFS(sub)
	e.GET("/admin", func(c *gin.Context) {
		http.Redirect(c.Writer, c.Request, "/admin/", http.StatusFound)
	})
	e.GET("/admin/*filepath", gin.WrapH(http.StripPrefix("/admin", fileServer)))
}
