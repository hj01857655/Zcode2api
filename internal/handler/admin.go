// 管理端点：GET /admin/api/overview 是 /admin 面板的数据源。
// 返回凭据状态（单账号或账号池）、模型列表与本地鉴权开关；
// token 只回长度不回内容，与 /health 同一口径。
package handler

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"zcode2api/internal/config"
	"zcode2api/internal/zcode"
)

// StatsFn 请求计数快照函数（server 侧注入，面板概览展示）。
type StatsFn func() map[string]int64

// Overview 构造 GET /admin/api/overview。
func Overview(cfg *config.Config, client *zcode.Client, stats StatsFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r, cfg) {
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
			"local_auth": cfg.APIKey != "" || cfg.AdminPassword != "",
			"admin_auth": cfg.AdminPassword != "",
			// 接入 Key（面板「网关接入」卡与 curl 示例用）：对话入口 key 与管理员密码
			// 是两回事，必须单独回显，否则登录后接入卡显示不了 key
			"apiKey": cfg.APIKey,
			"stats":  stats(),
		})
	}
}

// collectClaims 池（或单账号）的 label->jwt 收集；only 非空只留该号。
func collectClaims(client *zcode.Client, only string) map[string]string {
	claims := map[string]string{}
	if client.Pool != nil {
		for _, acc := range client.Pool.Accounts {
			if acc.Token != "" && (only == "" || acc.Label == only) {
				claims[acc.Label] = acc.Token
			}
		}
	} else if client.Token != "" && only == "" {
		claims["default"] = client.Token
	}
	return claims
}

// sortedLabels 收集后排序，输出稳定。
func sortedLabels(claims map[string]string) []string {
	labels := make([]string, 0, len(claims))
	for label := range claims {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels
}

// BalanceHandler 构造 GET /admin/api/balance：账号池逐账号查 billing/balance
// （与 claim 同一套头组与 device_mid 状态目录），面板行内"余额"按钮的数据源。
func BalanceHandler(cfg *config.Config, client *zcode.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r, cfg) {
			return
		}
		claims := collectClaims(client, r.URL.Query().Get("account"))
		labels := sortedLabels(claims)
		stateDir := claimStateDir()
		out := make([]zcode.AccountBalance, 0, len(labels))
		for _, label := range labels {
			jwt := claims[label]
			acc := zcode.SwitchAccount{Name: label, File: label, JWT: jwt,
				DeviceMid: zcode.StableDeviceMid(stateDir, jwt)}
			b, err := zcode.FetchBalance(claimHTTP, acc)
			if err != nil {
				out = append(out, zcode.AccountBalance{Account: label, File: label, Err: err.Error()})
				continue
			}
			out = append(out, b)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": out})
	}
}

// TouchHandler 构造 GET /admin/api/touch?account=label：逐账号查 touch.query
// 投放清单（可领信号的前置数据源，与 ClaimBestForToken 主路径同源）。
func TouchHandler(cfg *config.Config, client *zcode.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r, cfg) {
			return
		}
		claims := collectClaims(client, r.URL.Query().Get("account"))
		labels := sortedLabels(claims)
		stateDir := claimStateDir()
		out := make([]map[string]any, 0, len(labels))
		for _, label := range labels {
			jwt := claims[label]
			dels, err := zcode.TouchPlans(claimHTTP, jwt, zcode.StableDeviceMid(stateDir, jwt))
			if err != nil {
				out = append(out, map[string]any{"account": label, "err": err.Error()})
				continue
			}
			out = append(out, map[string]any{"account": label, "deliveries": dels})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": out})
	}
}

// authed 面板鉴权：admin_password 优先（面板专用凭据，不复用模型调用 key）；
// 未设密码时回落 api_key（向后兼容）；两者都空 = 不鉴权（本机自用）。
// Bearer 或 x-api-key 任一命中。
func authed(w http.ResponseWriter, r *http.Request, cfg *config.Config) bool {
	key := cfg.AdminPassword
	if key == "" {
		key = cfg.APIKey
	}
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

// LogsHandler 构造 GET /admin/api/logs?n=200：请求日志（内存环最近 N 条，新的在前）。
// 面板「请求日志」页数据源——此前日志只落 request.log，admin 面板看不到。
func LogsHandler(cfg *config.Config, recent func(n int) []zcode.RequestLog) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r, cfg) {
			return
		}
		n := 200
		if v := r.URL.Query().Get("n"); v != "" {
			if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 512 {
				n = p
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"logs": recent(n)})
	}
}
