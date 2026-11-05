// zcode-hub 账号池读取与余额查询（~/.zcode-hub/accounts/*.json）。
// 文件形如 {name, hash, virtual_device_mid, credentials:{...}}，credentials 值多为
// enc:v1 密文，密钥派生与 ~/.zcode/v2/credentials.json 同一套（credentialsKey，见 auth.go）。
// 密文只在本机内存中解；JWT 永不写盘、不打日志、不进任何返回值。
package zcode

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AccountsDir 账号目录：env ZCODE_ACCOUNTS_DIR 可覆盖。默认读 ~/.zcode-hub/accounts，
// 项目更名迁移兼容：新目录不存在且旧 ~/.zcode-switch/accounts 存在时回落旧目录，
// 两个来源都读（同名文件以新目录为准），避免迁移过渡期漏号。
func AccountsDir() string {
	if d := strings.TrimSpace(os.Getenv("ZCODE_ACCOUNTS_DIR")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".zcode-hub", "accounts")
}

// accountDirs 返回按优先级排列的候选账号目录：新目录（迁移目标）优先，旧目录兜底。
func accountDirs() []string {
	dirs := []string{AccountsDir()}
	if strings.Contains(AccountsDir(), ".zcode-hub") {
		home, err := os.UserHomeDir()
		if err == nil {
			dirs = append(dirs, filepath.Join(home, ".zcode-switch", "accounts"))
		}
	}
	return dirs
}

// SwitchAccount 单个 zcode-switch 账号的运行态身份。
type SwitchAccount struct {
	Name      string // 账号文件的 name 字段（空则回落文件名短 ID）
	File      string // 文件名（不含 .json）前 8 位，作短 ID
	Path      string
	JWT       string
	DeviceMid string
	UID       string
	Email     string
	Expired   bool
	ExpAt     time.Time
	Err       string // 解密/解析/过期原因；非空则 JWT 不可用
}

// LoadAccounts 读取并解密账号目录；only 非空时按文件名子串过滤
// （claim_check.js 的 ZCODE_ONLY 同语义）。
func LoadAccounts(dir string, only []string) []SwitchAccount {
	// 迁移兼容：dir 为空时用 accountDirs 双读（新目录优先，旧目录兜底，同名以新为准）
	if strings.TrimSpace(dir) == "" {
		return loadAccountsFrom(accountDirs(), only)
	}
	return loadAccountsFrom([]string{dir}, only)
}

func loadAccountsFrom(dirs []string, only []string) []SwitchAccount {
	seen := map[string]bool{} // 文件名（短 ID）→ 已读
	out := []SwitchAccount{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // 目录不存在（旧目录已迁移/新目录首装）→ 跳过
		}
		names := []string{}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && !seen[e.Name()] {
				seen[e.Name()] = true
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		out = append(out, loadAccountNames(dir, names, only)...)
	}
	return out
}

func loadAccountNames(dir string, names []string, only []string) []SwitchAccount {
	onlySet := map[string]bool{}
	for _, o := range only {
		onlySet[o] = true
	}
	want := names
	if len(only) > 0 {
		want = nil
		for _, n := range names {
			stem := strings.TrimSuffix(n, ".json")
			if onlySet[n] || onlySet[stem] || onlySet[stem[:min(8, len(stem))]] {
				want = append(want, n)
			}
		}
	}

	key, kerr := credentialsKey()
	if kerr != nil {
		return []SwitchAccount{{Name: "(密钥派生失败)", Err: kerr.Error()}}
	}
	accs := []SwitchAccount{}
	for _, name := range want {
		accs = append(accs, loadAccount(filepath.Join(dir, name), key))
	}
	return accs
}

func loadAccount(path string, key []byte) SwitchAccount {
	acc := SwitchAccount{Path: path}
	acc.File = strings.TrimSuffix(filepath.Base(path), ".json")
	if len(acc.File) > 8 {
		acc.File = acc.File[:8]
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		acc.Err = err.Error()
		return acc
	}
	var doc struct {
		Name             string         `json:"name"`
		VirtualDeviceMid string         `json:"virtual_device_mid"`
		Credentials      map[string]any `json:"credentials"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		acc.Err = "JSON 解析失败"
		return acc
	}
	if doc.Name != "" {
		acc.Name = doc.Name
	} else {
		acc.Name = acc.File
	}
	acc.DeviceMid = doc.VirtualDeviceMid

	jwtEnc, _ := doc.Credentials["zcodejwttoken"].(string)
	if jwtEnc == "" {
		acc.Err = "无 zcodejwttoken"
		return acc
	}
	jwt, derr := openCredential(jwtEnc, key)
	if derr != nil {
		acc.Err = "jwt 解密失败: " + derr.Error()
		return acc
	}
	acc.JWT = jwt
	acc.UID, acc.Email = jwtIdentity(jwt)
	if exp, ok := jwtExp(jwt); ok {
		acc.ExpAt = exp
		acc.Expired = exp.Before(time.Now())
		if acc.Expired {
			acc.Err = "JWT 已过期"
		}
	}
	return acc
}

// openCredential 解 enc:v1:{b64url(iv)}.{b64url(tag)}.{b64url(ct)}（sealCredential 的逆）；
// 无前缀按明文直存的历史值处理。
func openCredential(v string, key []byte) (string, error) {
	const prefix = "enc:v1:"
	if !strings.HasPrefix(v, prefix) {
		return v, nil
	}
	parts := strings.Split(v[len(prefix):], ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("密文应为 iv.tag.ct 三段")
	}
	b64d := base64.RawURLEncoding.DecodeString
	iv, err1 := b64d(parts[0])
	tag, err2 := b64d(parts[1])
	ct, err3 := b64d(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return "", fmt.Errorf("base64url 解码失败: %v %v %v", err1, err2, err3)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	pt, err := gcm.Open(nil, iv, append(ct, tag...), nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// jwtIdentity 取 JWT payload 的 user_id（缺省 sub）与 email。
func jwtIdentity(jwt string) (uid, email string) {
	var p struct {
		UserID string `json:"user_id"`
		Sub    string `json:"sub"`
		Email  string `json:"email"`
	}
	raw, ok := jwtPayload(jwt)
	if !ok || json.Unmarshal(raw, &p) != nil {
		return "", ""
	}
	if p.UserID != "" {
		return p.UserID, p.Email
	}
	return p.Sub, p.Email
}

// jwtExp 取 JWT payload 的 exp（无该字段返回 ok=false）。
func jwtExp(jwt string) (time.Time, bool) {
	var p struct {
		Exp int64 `json:"exp"`
	}
	raw, ok := jwtPayload(jwt)
	if !ok || json.Unmarshal(raw, &p) != nil || p.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(p.Exp, 0), true
}

func jwtPayload(jwt string) ([]byte, bool) {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	return raw, err == nil
}

// BalanceEntry billing/balance 里一个额度桶的摘要。上游桶的完整形态
// （实测 2026-11-04 原始响应）还包括 bucket_id/entitlement_id/capabilities/
// period_start/period_end——面板要靠 capabilities 区分模型桶、靠 period 说明
// 每日重置，不能只留数字三件套。
type BalanceEntry struct {
	Plan          string   `json:"plan"`
	Model         string   `json:"model"`      // show_name
	BucketID      string   `json:"bucket_id,omitempty"`
	EntitlementID string   `json:"entitlement_id,omitempty"`
	Meter         string   `json:"meter,omitempty"`
	UnitType      string   `json:"unit_type,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"` // 如 model:glm-5.3
	Priority      int      `json:"priority,omitempty"`
	PeriodStart   string   `json:"period_start,omitempty"` // CST8 MM-DD HH:MM
	PeriodEnd     string   `json:"period_end,omitempty"`
	Remaining     int64    `json:"remaining"`
	Total         int64    `json:"total"`
	Used          int64    `json:"used"`
	ExpiresAt     string   `json:"expires_at"` // CST8
}

// BalanceEntitlement plans[].entitlements 一条配额定义。
type BalanceEntitlement struct {
	EntitlementID string `json:"entitlement_id"`
	ShowName      string `json:"show_name"`
	GrantUnits    int64  `json:"grant_units"`
	Period        string `json:"period"` // daily 等：桶的重置口径
	Meter         string `json:"meter,omitempty"`
}

// BalancePlan data.plans[] 一个已激活计划（名称/状态/有效期）。此前这整块
// 都没解析，面板看不到账号当前挂在哪个计划上。
type BalancePlan struct {
	UserPlanID   string               `json:"user_plan_id,omitempty"`
	PlanID       string               `json:"plan_id"`
	Name         string               `json:"name"`
	Description  string               `json:"description,omitempty"`
	Status       string               `json:"status"`
	Priority     int                  `json:"priority,omitempty"`
	StartsAt     string               `json:"starts_at,omitempty"`
	EndsAt       string               `json:"ends_at,omitempty"`
	Entitlements []BalanceEntitlement `json:"entitlements,omitempty"`
}

// AccountBalance 单账号余额查询结果。
type AccountBalance struct {
	Account string         `json:"account"`
	File    string         `json:"file"`
	UID     string         `json:"uid,omitempty"`
	Err     string         `json:"err,omitempty"`
	Plans   []BalancePlan  `json:"plans,omitempty"`
	Entries []BalanceEntry `json:"entries,omitempty"`
}

// balancePath billing/balance 端点（claim_check.js --balance 同源）。
const balancePath = "/api/v1/zcode-plan/billing/balance"

// FetchBalance 查单账号额度桶；账号本身解密失败时返回带 Err 的结果而不报错。
func FetchBalance(client *http.Client, acc SwitchAccount) (AccountBalance, error) {
	out := AccountBalance{Account: acc.Name, File: acc.File, UID: acc.UID}
	if acc.Err != "" {
		out.Err = acc.Err
		return out, nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequest(http.MethodGet, ClaimOrigin+balancePath+"?app_version="+ClaimAppVer, nil)
	if err != nil {
		return out, err
	}
	req.Header = claimHeaders(acc.JWT, acc.DeviceMid)
	req.Header.Set("X-Client-Language", "zh-CN")
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, err
	}
	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Plans []struct {
				UserPlanID   string `json:"user_plan_id"`
				PlanID       string `json:"plan_id"`
				Name         string `json:"name"`
				Description  string `json:"description"`
				Priority     int    `json:"priority"`
				Status       string `json:"status"`
				StartsAt     int64  `json:"starts_at"`
				EndsAt       int64  `json:"ends_at"`
				Entitlements []struct {
					EntitlementID string `json:"entitlement_id"`
					ShowName      string `json:"show_name"`
					Meter         string `json:"meter"`
					UnitType      string `json:"unit_type"`
					GrantUnits    int64  `json:"grant_units"`
					Period        string `json:"period"`
					Priority      int    `json:"priority"`
				} `json:"entitlements"`
			} `json:"plans"`
			Balances []struct {
				BucketID      string   `json:"bucket_id"`
				PlanID        string   `json:"plan_id"`
				EntitlementID string   `json:"entitlement_id"`
				ShowName      string   `json:"show_name"`
				Meter         string   `json:"meter"`
				UnitType      string   `json:"unit_type"`
				Capabilities  []string `json:"capabilities"`
				Priority      int      `json:"priority"`
				TotalUnits    int64    `json:"total_units"`
				UsedUnits     int64    `json:"used_units"`
				Remaining     int64    `json:"remaining_units"`
				Available     int64    `json:"available_units"`
				PeriodStart   int64    `json:"period_start"`
				PeriodEnd     int64    `json:"period_end"`
				ExpiresAt     int64    `json:"expires_at"`
			} `json:"balances"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return out, fmt.Errorf("HTTP %d 非 JSON 响应: %.80s", resp.StatusCode, string(data))
	}
	if payload.Code != 0 {
		msg := payload.Msg
		if msg == "" {
			msg = claimErrText[payload.Code]
		}
		out.Err = fmt.Sprintf("balance code=%d: %s", payload.Code, msg)
		return out, nil
	}
	cst := func(ts int64) string {
		if ts <= 0 {
			return ""
		}
		return time.Unix(ts, 0).In(CST8).Format("01-02 15:04")
	}
	out.Entries = []BalanceEntry{}
	for _, b := range payload.Data.Balances {
		remain := b.Remaining
		if remain == 0 {
			remain = b.Available
		}
		out.Entries = append(out.Entries, BalanceEntry{
			Plan:          b.PlanID,
			Model:         b.ShowName,
			BucketID:      b.BucketID,
			EntitlementID: b.EntitlementID,
			Meter:         b.Meter,
			UnitType:      b.UnitType,
			Capabilities:  b.Capabilities,
			Priority:      b.Priority,
			PeriodStart:   cst(b.PeriodStart),
			PeriodEnd:     cst(b.PeriodEnd),
			Remaining:     remain,
			Total:         b.TotalUnits,
			Used:          b.UsedUnits,
			ExpiresAt:     cst(b.ExpiresAt),
		})
	}
	for _, p := range payload.Data.Plans {
		bp := BalancePlan{
			UserPlanID:  p.UserPlanID,
			PlanID:      p.PlanID,
			Name:        p.Name,
			Description: p.Description,
			Status:      p.Status,
			Priority:    p.Priority,
			StartsAt:    cst(p.StartsAt),
			EndsAt:      cst(p.EndsAt),
		}
		for _, e := range p.Entitlements {
			bp.Entitlements = append(bp.Entitlements, BalanceEntitlement{
				EntitlementID: e.EntitlementID,
				ShowName:      e.ShowName,
				GrantUnits:    e.GrantUnits,
				Period:        e.Period,
				Meter:         e.Meter,
			})
		}
		out.Plans = append(out.Plans, bp)
	}
	return out, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
