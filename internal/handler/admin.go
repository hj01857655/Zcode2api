// 管理端点：GET /admin/api/overview 是 /admin 面板的数据源。
// 返回凭据状态（单账号或账号池）、模型列表与本地鉴权开关；
// token 只回长度不回内容，与 /health 同一口径。
package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"zcode2api/internal/config"
	"zcode2api/internal/zcode"
)

// Overview 构造 GET /admin/api/overview。
func Overview(cfg *config.Config, client *zcode.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r, cfg.APIKey) {
			return
		}
		cred := map[string]any{"valid": false, "mode": "none", "notice": ""}
		if client.Pool != nil {
			n := client.Pool.Size()
			cred["mode"] = "pool"
			cred["accounts"] = n
			cred["account_list"] = client.Pool.Snapshot()
			cred["valid"] = n > 0
			if n == 0 {
				cred["notice"] = "账号池为空，先执行 ./zcode2api-go.exe login"
			}
		} else if len(client.Token) > 0 {
			cred["mode"] = "single"
			cred["token_chars"] = len(client.Token)
			cred["valid"] = true
		} else {
			cred["notice"] = "token 未加载 —— 设置 ZCODE_TOKEN、执行 login，或登录 ZCode CLI/GUI 后自动重读"
		}
		// 模型列表与 /v1/models 同源：用 cfg.Models（真实可用表）。
		// 此前从 alias 表生成，残留了上游并不存在的 GLM-4.7 假项。
		models := []map[string]any{}
		for _, v := range cfg.Models {
			models = append(models, map[string]any{"id": v, "default": v == cfg.DefaultModel})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service":    "ZCode2api-go",
			"credential": cred,
			"models":     models,
			"local_auth": cfg.APIKey != "",
		})
	}
}

// authed 与 server.checkAuth 同语义：key 为空放行；否则 Bearer 或 x-api-key 任一命中。
func authed(w http.ResponseWriter, r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	got := ""
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		got = strings.TrimSpace(auth[7:])
	} else {
		got = r.Header.Get("x-api-key")
	}
	if got == key {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": "invalid api key for this gateway", "type": "invalid_request_error"},
	})
	return false
}
