// 套餐领取端点：POST /admin/api/claim（?preview=1 只列不领）。对账号池里
// 每个账号跑一遍 激活上报 → preview → 领最高优先级，逐账号返回结果。
// ClaimAll 供 main 的每日自动领取复用。
package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/zcode"
)

// claimHTTP 领取协议的短超时 client（chat 上游的 300s 不适用这里）。
var claimHTTP = &http.Client{Timeout: 30 * time.Second}

// claimStateDir device_mid 落点：与 token.json 同目录（本项目的状态目录语义）。
var claimStateDir = func() string {
	return filepath.Dir(zcode.LocalTokenPath())
}

// ClaimAll 对池内（或单账号）全部 token 各领一轮，返回逐账号结果。
func ClaimAll(cfg *config.Config, client *zcode.Client, previewOnly bool) []zcode.ClaimResult {
	out := []zcode.ClaimResult{}
	claims := map[string]string{} // label -> token
	if client.Pool != nil {
		for _, acc := range client.Pool.Accounts {
			if acc.Token != "" {
				claims[acc.Label] = acc.Token
			}
		}
	} else if client.Token != "" {
		claims["default"] = client.Token
	}
	stateDir := claimStateDir()
	for label, jwt := range claims {
		mid := zcode.StableDeviceMid(stateDir, jwt)
		if previewOnly {
			plans, err := zcode.PreviewPlans(claimHTTP, jwt, mid)
			if err != nil {
				out = append(out, zcode.ClaimResult{Account: label, OK: false, Message: "preview 失败: " + err.Error()})
				continue
			}
			msg := "现在没有可领取的套餐"
			if len(plans) > 0 {
				best := plans[0]
				for _, p := range plans[1:] {
					if p.Priority > best.Priority {
						best = p
					}
				}
				msg = "可领: " + best.Name
			}
			out = append(out, zcode.ClaimResult{Account: label, OK: true, Message: msg})
			continue
		}
		msg, _ := zcode.ClaimBestForToken(claimHTTP, jwt, mid)
		out = append(out, zcode.ClaimResult{Account: label, OK: true, Message: msg})
	}
	return out
}

// ClaimHandler POST /admin/api/claim。?preview=1 只列不领。
func ClaimHandler(cfg *config.Config, client *zcode.Client, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r, cfg.APIKey) {
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		previewOnly := r.URL.Query().Get("preview") == "1"
		results := ClaimAll(cfg, client, previewOnly)
		if log != nil {
			log.Info("套餐领取完成", slog.Bool("preview", previewOnly), slog.Int("accounts", len(results)))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "results": results})
	}
}
