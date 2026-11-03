// ZCode 限时套餐领取（社区叫「签到」，实际是 billing/claim：preview 列套餐、
// 按优先级领取、幂等——已领过返回 1003 算成功）。协议来自社区青龙脚本
// L0NE-6/ZCode-Daily 的抓包结论，并用本机真实 JWT 实测 preview/事件上报
// 双 200 验证过（2026-10-27，当时活动无可领套餐，端点与头组均正确）。
package zcode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// 领取协议端点（国内版/国际版同域名同路径，仅 JWT 签发方不同）。
// ClaimOrigin 用 var 以便测试替换。
const (
	ClaimPreview  = "/api/v1/zcode-plan/billing/preview"
	ClaimPath     = "/api/v1/zcode-plan/billing/claim"
	EventReport   = "/api/v1/event/report"
	TouchQuery    = "/api/v1/marketing/touch"
	TouchAction   = "/api/v1/marketing/touch/action"
	ClaimAppVer   = "3.17.2"
	ClaimPlatform = "win32-x64"
)

var ClaimOrigin = "https://zcode.z.ai"

// CST8 官方活动时区=北京时间（中国无夏令时，用固定偏移，Windows/精简容器
// 没有 IANA tzdb 也可用）。领取投放（UTC 11:00 起）与 300M 使用窗口
// （UTC 15:00–次日 01:00）都按它算，与部署机本地时区无关。
var CST8 = time.FixedZone("UTC+8", 8*3600)

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
	uid, _ := jwtIdentity(jwt)
	return uid
}

// claimHeaders 领取端点共用的头组（CLAIM-FLOW.md 桌面端形状对齐：
// Bearer + Content-Type + 版本/平台/设备 + UA/Referer/Title）。
func claimHeaders(jwt, deviceMid string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+jwt)
	h.Set("Content-Type", "application/json")
	h.Set("X-ZCode-App-Version", ClaimAppVer)
	h.Set("X-Platform", ClaimPlatform)
	h.Set("User-Agent", "ZCode/"+ClaimAppVer)
	h.Set("HTTP-Referer", "https://zcode.z.ai")
	h.Set("X-Title", "Z Code@electron")
	if deviceMid != "" {
		h.Set("X-Device-Mid", deviceMid)
	}
	return h
}

// claimPost 领取协议的 POST 帮手；captchaParam 非空时带阿里云验证码头，
// captchaRegion 为空回落 cn。
func claimPost(client *http.Client, path string, jwt, deviceMid string, body any, captchaParam, captchaRegion string) (map[string]any, error) {
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
		if captchaRegion == "" {
			captchaRegion = "cn"
		}
		req.Header.Set("X-Aliyun-Captcha-Verify-Region", captchaRegion)
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
		payload, err := claimPost(client, EventReport, jwt, deviceMid, body, "", "")
		if err != nil {
			return fmt.Errorf("%s: %w", event, err)
		}
		if code, _ := payload["code"].(float64); code != 0 {
			return fmt.Errorf("%s: code=%v", event, payload["code"])
		}
	}
	return nil
}

// CaptchaConfig client/configs 下发的阿里云验证码配置（zcode-switch 同口径：
// claim 开启验证码时必须带 param 才能发起，场景号动态下发防换号失效）。
type CaptchaConfig struct {
	Enabled bool
	SceneID string
	Region  string
	Prefix  string
}

// CaptchaConfigsPath 验证码配置下发端点。
const CaptchaConfigsPath = "/api/v1/client/configs"

// FetchCaptchaConfig 拉当前账号的验证码配置。
func FetchCaptchaConfig(client *http.Client, jwt, deviceMid string) (CaptchaConfig, error) {
	req, err := http.NewRequest(http.MethodGet, ClaimOrigin+CaptchaConfigsPath, nil)
	if err != nil {
		return CaptchaConfig{}, err
	}
	req.Header = claimHeaders(jwt, deviceMid)
	resp, err := client.Do(req)
	if err != nil {
		return CaptchaConfig{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return CaptchaConfig{}, err
	}
	var payload struct {
		Data struct {
			Configs struct {
				Captcha struct {
					Enabled bool   `json:"enabled"`
					Region  string `json:"region"`
					Prefix  string `json:"prefix"`
					SceneID string `json:"sceneId"`
				} `json:"captcha"`
			} `json:"configs"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return CaptchaConfig{}, fmt.Errorf("configs 响应解析失败: %.80s", string(data))
	}
	c := payload.Data.Configs.Captcha
	return CaptchaConfig{Enabled: c.Enabled, SceneID: c.SceneID, Region: c.Region, Prefix: c.Prefix}, nil
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
	// 主路径：touch 投放清单（桌面端领取链路的前置数据源唯一是 touch.query，
	// preview 注册但零调用——CLAIM-FLOW.md §1.5 实锤）。touch 失败回落 preview。
	dels, terr := TouchPlans(client, jwt, deviceMid)
	var planID, campaignID string
	if terr == nil {
		for _, d := range dels {
			if len(d.PlanIDs) > 0 {
				planID = d.PlanIDs[0]
				campaignID = d.CampaignID
				break
			}
		}
	}
	var plans []Plan
	if planID == "" {
		// 回落：preview 兼容路径（touch 无投放或失败时仍可尝试）
		var err error
		plans, err = PreviewPlans(client, jwt, deviceMid)
		if err != nil {
			return "preview 失败: " + err.Error(), nil
		}
		if len(plans) == 0 {
			return "现在没有可领取的套餐", plans
		}
		best := plans[0]
		for _, p := range plans[1:] {
			if p.Priority > best.Priority {
				best = p
			}
		}
		planID = best.PlanID
	}
	body := map[string]string{"plan_id": planID}
	// 验证码配置动态下发；开启时先求解再发起 claim（param 一次性），避免
	// 先裸发一轮吃 3007。求解失败不阻断——退回 3007 现场重试的兜底路径。
	var captcha CaptchaConfig
	if c, err := FetchCaptchaConfig(client, jwt, deviceMid); err == nil {
		captcha = c
	}
	var param string
	if captcha.Enabled && captcha.SceneID != "" {
		if p, serr := SolveCaptcha(captcha.SceneID, captcha.Prefix, captcha.Region); serr == nil {
			param = p
		}
	}
	// 提交循环（对齐今天实测的领取链行为）：
	//   429 —— 限流是出口 IP 级（连领 4 个后第 5 个必撞），指数退避 60s/120s 重试两轮；
	//          验证码 param 一次性，每轮重试前重新求解；
	//   3012 —— unusual activity 风控标记期小时级，CLI 等不起，不自动重试，直接失败说明；
	//   3007 —— 验证码未通过：现场重新求解再发一次（param 一次性）。
	var payload map[string]any
	var err error
	code := 0
	solved3007 := false
	for attempt := 0; ; attempt++ {
		payload, err = claimPost(client, ClaimPath, jwt, deviceMid, body, param, captcha.Region)
		if err != nil {
			return "claim 请求失败: " + err.Error(), plans
		}
		code = 0
		if c, ok := payload["code"].(float64); ok {
			code = int(c)
		}
		switch {
		case code == 0:
		case code == 3007 && !solved3007:
			solved3007 = true
			p, serr := SolveCaptcha(captcha.SceneID, captcha.Prefix, captcha.Region)
			if serr != nil {
				return "领取需要验证码，求解失败: " + errTextOr(serr.Error()), plans
			}
			param = p
			continue
		case code == 429 && attempt < 2:
			time.Sleep(time.Duration(60*(attempt+1)) * time.Second)
			if captcha.Enabled && captcha.SceneID != "" {
				if p, serr := SolveCaptcha(captcha.SceneID, captcha.Prefix, captcha.Region); serr == nil {
					param = p
				}
			}
			continue
		case code == 3012:
			return "领取被风控拦截（3012 unusual activity）：该出口 IP 已被标记，请挂代理或等待标记期过后再试", plans
		}
		break
	}
	var planName string
	if planID != "" && len(plans) > 0 {
		planName = plans[0].Name // preview 回落路径：best 对应的套餐名
	}
	switch {
	case code == 0:
		// 桌面对齐：领取成功后上报投放动作 confirm（遥测形状，失败不影响领取结果）
		_ = TouchReport(client, jwt, deviceMid, campaignID, "confirm")
		return "领取成功: " + planName, plans
	case code == 1002:
		// 桌面对齐：任意失败码回读 data.plan.ends_at 作权威冷却（CLAIM-FLOW.md §1）
		endsAt := int64(0)
		if plan, ok := payload["data"].(map[string]any); ok {
			if p2, ok := plan["plan"].(map[string]any); ok {
				if v, ok := p2["ends_at"].(float64); ok {
					endsAt = int64(v) * 1000
				}
			}
		}
		msg := claimErrText[code]
		if m, ok := payload["message"].(string); ok && m != "" {
			msg = m
		}
		if endsAt > 0 {
			return fmt.Sprintf("领取未成功: %s（code=%d，计划期至 %s）", msg, code,
				time.UnixMilli(endsAt).In(CST8).Format("01-02 15:04")), plans
		}
		return fmt.Sprintf("领取未成功: %s（code=%d）", msg, code), plans
	case code == 1003:
		return "已领取过这一期: " + planName, plans
	default:
		if text, ok := claimErrText[code]; ok {
			return fmt.Sprintf("领取未成功: %s（code=%d）", text, code), plans
		}
		msg, _ := payload["message"].(string)
		return fmt.Sprintf("领取未成功: code=%d %s", code, msg), plans
	}
}

func errTextOr(s string) string {
	if s == "" {
		return "未知错误"
	}
	return s
}

// ---- touch 投放通道（CLAIM-FLOW.md §1.7/§4 对齐）----

// TouchSeq touch 查询序号：桌面端为模块级递增计数器（asar `Xde=0, seq=String(Xde++)`，
// 首查发 0），非固定值——对齐服务端按 seq 的去重语义。
var touchSeq atomic.Uint64

// TouchDelivery 投放壳解析后的可领信号（claim_zcode_plan 按钮的唯一来源）。
type TouchDelivery struct {
	CampaignID       string
	ResourcePosition string
	PlanIDs          []string // buttons[].action.type=="claim_zcode_plan" 的 plan_id
}

// TouchPlans 查当前账号的投放清单（替代 preview 作为可领数据源——桌面端
// 领取链路的前置数据源唯一是 touch.query，preview 注册但零调用）。
// 响应 ≤32 deliveries，resource_position 重复保留第一条（parseMarketingTouchResponse 同语义）。
func TouchPlans(client *http.Client, jwt, deviceMid string) ([]TouchDelivery, error) {
	seq := touchSeq.Add(1) - 1 // 首查发 0
	req, err := http.NewRequest(http.MethodGet,
		ClaimOrigin+TouchQuery+"?seq="+fmt.Sprint(seq), nil)
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
			ServerTime int64            `json:"server_time"`
			Language   string           `json:"language"`
			Deliveries []map[string]any `json:"deliveries"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil, fmt.Errorf("touch 响应解析失败: %.80s", string(data))
	}
	if payload.Code != 0 {
		return nil, fmt.Errorf("touch code=%d", payload.Code)
	}
	out := []TouchDelivery{}
	seenPos := map[string]bool{}
	for _, d := range payload.Data.Deliveries {
		pos, _ := d["resource_position"].(string)
		if pos == "" {
			pos, _ = d["resourcePosition"].(string)
		}
		if pos == "" || seenPos[pos] {
			continue
		}
		seenPos[pos] = true
		td := TouchDelivery{ResourcePosition: pos}
		if c, ok := d["campaign_id"].(string); ok {
			td.CampaignID = c
		} else if c, ok := d["campaignId"].(string); ok {
			td.CampaignID = c
		}
		// 双壳按钮扫描：banner.buttons 与根 buttons（popup 壳不再漏）
		nodes := []map[string]any{}
		if b, ok := d["banner"].(map[string]any); ok {
			nodes = append(nodes, b)
		}
		nodes = append(nodes, d)
		seenPlan := map[string]bool{}
		for _, node := range nodes {
			btns, _ := node["buttons"].([]any)
			for _, raw := range btns {
				b, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				a, _ := b["action"].(map[string]any)
				if a == nil || a["type"] != "claim_zcode_plan" {
					continue
				}
				args, _ := a["args"].(map[string]any)
				if args == nil {
					continue
				}
				pid, _ := args["plan_id"].(string)
				if pid == "" {
					pid, _ = args["planId"].(string)
				}
				if pid = strings.TrimSpace(pid); pid != "" && !seenPlan[pid] {
					seenPlan[pid] = true
					td.PlanIDs = append(td.PlanIDs, pid)
				}
			}
		}
		out = append(out, td)
	}
	return out, nil
}

// TouchReport 投放动作回执：领取成功后上报 confirm（遥测形状，失败不影响
// 领取结果——服务端对 campaign 活跃性/重复提交均不校验，{code:0} 恒回）。
func TouchReport(client *http.Client, jwt, deviceMid, campaignID, actionType string) error {
	if campaignID == "" {
		return nil
	}
	payload, err := claimPost(client, TouchAction, jwt, deviceMid,
		map[string]any{"campaign_id": campaignID, "action_type": actionType}, "", "")
	if err != nil {
		return err
	}
	if code, _ := payload["code"].(float64); code != 0 {
		return fmt.Errorf("code=%v", payload["code"])
	}
	return nil
}
