// ZCode 限时套餐领取（社区叫「签到」，实际是 billing/claim：preview 列套餐、
// 按优先级领取、幂等——已领过返回 1003 算成功）。协议来自社区青龙脚本
// L0NE-6/ZCode-Daily 的抓包结论，并用本机真实 JWT 实测 preview/事件上报
// 双 200 验证过（2026-10-27，当时活动无可领套餐，端点与头组均正确）。
package zcode

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 领取协议端点（国内版/国际版同域名同路径，仅 JWT 签发方不同）。
const (
	ClaimOrigin   = "https://zcode.z.ai"
	ClaimPreview  = "/api/v1/zcode-plan/billing/preview"
	ClaimPath     = "/api/v1/zcode-plan/billing/claim"
	EventReport   = "/api/v1/event/report"
	ClaimAppVer   = "3.14.0"
	ClaimPlatform = "win32-x64"
)

// claimErrText 上游错误码的中文说明（脚本 FAILURE_LABELS 口径）。
var claimErrText = map[int]string{
	1001: "没有这个套餐",
	1002: "活动还没开始或已经结束",
	1003: "该账号已经领过这一期",
	1004: "当前账号或客户端版本不在活动范围内",
	1005: "今天领取次数已经用完",
	3001: "请求参数不对（device_mid）",
	3007: "验证码没通过",
	401:  "登录状态失效",
}

// Plan preview 返回的一个可领套餐。
type Plan struct {
	PlanID   string   `json:"plan_id"`
	Name     string   `json:"name"`
	Desc     string   `json:"description"`
	Priority int      `json:"priority"`
	Entitle  []string `json:"entitlements"`
}

// StableDeviceMid 每账号固定 device_mid（uuid v4，落盘 <dir>/zcode_devices.json，
// key=sha256(jwt)[:16]）。与脚本同构：device_mid 换来换去会触发 3001。
func StableDeviceMid(dir, jwt string) string {
	sum := sha256.Sum256([]byte(jwt))
	key := hex.EncodeToString(sum[:])[:16]
	path := filepath.Join(dir, "zcode_devices.json")
	store := map[string]map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &store)
	}
	if m, ok := store[key]; ok {
		if mid, _ := m["mid"].(string); mid != "" {
			return mid
		}
	}
	mid := UUIDv4()
	store[key] = map[string]any{"mid": mid, "at": time.Now().Unix()}
	if raw, err := json.MarshalIndent(store, "", "  "); err == nil {
		_ = os.WriteFile(path, raw, 0o600)
	}
	return mid
}

// jwtUserID 从 JWT payload 里解 user_id（解不出返回空串——事件上报可跳过）。
func jwtUserID(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var payload struct {
		UserID string `json:"user_id"`
		Sub    string `json:"sub"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	if payload.UserID != "" {
		return payload.UserID
	}
	return payload.Sub
}

// claimHeaders 领取三端点共用的头组（脚本验证过的形状）。
func claimHeaders(jwt, deviceMid string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+jwt)
	h.Set("Content-Type", "application/json")
	h.Set("X-ZCode-App-Version", ClaimAppVer)
	h.Set("X-Platform", ClaimPlatform)
	if deviceMid != "" {
		h.Set("X-Device-Mid", deviceMid)
	}
	return h
}

// claimPost 领取协议的 POST 帮手；captchaParam 非空时带阿里云验证码头。
func claimPost(client *http.Client, path string, jwt, deviceMid string, body any, captchaParam string) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, ClaimOrigin+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header = claimHeaders(jwt, deviceMid)
	if captchaParam != "" {
		req.Header.Set("X-Aliyun-Captcha-Verify-Param", captchaParam)
		req.Header.Set("X-Aliyun-Captcha-Verify-Region", "cn")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return nil, fmt.Errorf("HTTP %d 非 JSON 响应: %.80s", resp.StatusCode, string(data))
	}
	return payload, nil
}

// ReportActivation 补报 app_launch / app_daily_active——领取资格的前置动作
// （官方客户端启动/日活会报，网关进程替它补；失败不阻断领取）。
func ReportActivation(client *http.Client, jwt, deviceMid string) error {
	userID := jwtUserID(jwt)
	if userID == "" {
		return nil // 脚本同语义：缺 user_id 跳过上报
	}
	for _, event := range []string{"app_launch", "app_daily_active"} {
		body := map[string]any{
			"event_id":           UUIDv4(),
			"client_timezone":    "Asia/Shanghai",
			"client_language":    "zh-CN",
			"element_name":       event,
			"event_region":       "app",
			"event_type":         "view",
			"event_text":         "",
			"event_extra_detail": map[string]any{},
			"user_id":            userID,
			"screen_resolution":  "2560x1440",
			"app_version":        ClaimAppVer,
			"device_os_category": "windows",
			"device_os_version":  "",
			"device_mid":         deviceMid,
			"source_page":        "",
			"source_element":     "{}",
		}
		payload, err := claimPost(client, EventReport, jwt, deviceMid, body, "")
		if err != nil {
			return fmt.Errorf("%s: %w", event, err)
		}
		if code, _ := payload["code"].(float64); code != 0 {
			return fmt.Errorf("%s: code=%v", event, payload["code"])
		}
	}
	return nil
}

// PreviewPlans 列当前可领套餐。
func PreviewPlans(client *http.Client, jwt, deviceMid string) ([]Plan, error) {
	req, err := http.NewRequest(http.MethodGet,
		ClaimOrigin+ClaimPreview+"?app_version="+ClaimAppVer+"&platform="+ClaimPlatform, nil)
	if err != nil {
		return nil, err
	}
	req.Header = claimHeaders(jwt, deviceMid)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Plans []struct {
				PlanID   string `json:"plan_id"`
				PlanID2  string `json:"planId"`
				Name     string `json:"name"`
				Desc     string `json:"description"`
				Priority int    `json:"priority"`
				Entitle  []struct {
					ShowName string `json:"show_name"`
					Show2    string `json:"showName"`
					Unit     string `json:"unit_type"`
					Grant    any    `json:"grant_units"`
					Period   string `json:"period"`
				} `json:"entitlements"`
			} `json:"plans"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil, fmt.Errorf("preview 响应解析失败: %.80s", string(data))
	}
	plans := []Plan{}
	for _, p := range payload.Data.Plans {
		id := p.PlanID
		if id == "" {
			id = p.PlanID2
		}
		if id == "" {
			continue
		}
		ent := []string{}
		for _, e := range p.Entitle {
			name := e.ShowName
			if name == "" {
				name = e.Show2
			}
			if name == "" {
				continue
			}
			period := "一次性"
			if e.Period == "daily" {
				period = "每日"
			}
			ent = append(ent, fmt.Sprintf("%s %v%s（%s）", name, e.Grant, e.Unit, period))
		}
		plans = append(plans, Plan{PlanID: id, Name: p.Name, Desc: p.Desc,
			Priority: p.Priority, Entitle: ent})
	}
	return plans, nil
}

// DescribePlan 套餐文案：名称 + 权益明细（脚本 describe_plan 口径）。
func DescribePlan(p Plan) string {
	if len(p.Entitle) == 0 {
		return p.Name
	}
	return p.Name + "：" + strings.Join(p.Entitle, "，")
}

// ClaimResult 一次领取的结果（面板逐账号展示）。
type ClaimResult struct {
	Account string `json:"account"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// ClaimBestForToken 对单个 JWT：补报激活 → preview → 领 priority 最高的套餐。
// 幂等：1003（已领过）按成功处理；没有可领套餐返回 ok=false + 说明。
func ClaimBestForToken(client *http.Client, jwt, deviceMid string) (string, []Plan) {
	_ = ReportActivation(client, jwt, deviceMid) // 失败只影响资格，不阻断
	plans, err := PreviewPlans(client, jwt, deviceMid)
	if err != nil {
		return "preview 失败: " + err.Error(), nil
	}
	if len(plans) == 0 {
		return "现在没有可领取的套餐", plans
	}
	best := plans[0] // 同优先级取最先出现的（脚本 pick_plan 语义）
	for _, p := range plans[1:] {
		if p.Priority > best.Priority {
			best = p
		}
	}
	payload, err := claimPost(client, ClaimPath, jwt, deviceMid, map[string]string{"plan_id": best.PlanID}, "")
	if err != nil {
		return "claim 请求失败: " + err.Error(), plans
	}
	code := 0
	if c, ok := payload["code"].(float64); ok {
		code = int(c)
	}
	// 3007 = 验证码未通过：现场求解阿里云 captcha 后带 param 重试一次
	// （param 一次性，每次 claim 必须重新求）。
	if code == 3007 {
		if param, serr := SolveCaptcha(); serr == nil {
			if p2, err2 := claimPost(client, ClaimPath, jwt, deviceMid, map[string]string{"plan_id": best.PlanID}, param); err2 == nil {
				payload, code = p2, 0
				if cv, ok := p2["code"].(float64); ok {
					code = int(cv)
				}
			}
		} else {
			return "领取需要验证码，求解失败: " + serr.Error(), plans
		}
	}
	switch {
	case code == 0:
		return "领取成功: " + DescribePlan(best), plans
	case code == 1003:
		return "已领取过这一期: " + best.Name, plans
	default:
		if text, ok := claimErrText[code]; ok {
			return fmt.Sprintf("领取未成功: %s（code=%d）", text, code), plans
		}
		msg, _ := payload["message"].(string)
		return fmt.Sprintf("领取未成功: code=%d %s", code, msg), plans
	}
}
