// 面板静态页：内置一份 panel.html（//go:embed），/admin 开箱即用。
// 本仓库 config 没有面板覆盖项，内置版即唯一来源（assets/prefix.json 的
// embed 在 zcode 包，互不相干）。
package handler

import (
	_ "embed"
	"net/http"
)

//go:embed panel.html
var panelBuiltin []byte

// PanelHandler GET /admin：输出内置控制台页。
func PanelHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(panelBuiltin)
	}
}
