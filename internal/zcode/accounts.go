// zcode-switch 账号池读取与余额查询（~/.zcode-switch/accounts/*.json）。
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

// AccountsDir 账号目录：env ZCODE_ACCOUNTS_DIR 可覆盖，默认 ~/.zcode-switch/accounts。
func AccountsDir() string {
	if d := strings.TrimSpace(os.Getenv("ZCODE_ACCOUNTS_DIR")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".zcode-switch", "accounts")
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []SwitchAccount{{Name: "(目录不可读)", Err: err.Error()}}
	}
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	key, kerr := credentialsKey()
	if kerr != nil {
		return []SwitchAccount{{Name: "(密钥派生失败)", Err: kerr.Error()}}
}
		accs := []SwitchAccount{}
	for _, name := range names {
		if len(only) > 0 {
			hit := false
			for _, o := range only {
				if strings.Contains(name, o) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
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

// BalanceEntry billing/balance 里一个额度桶的摘要。
type BalanceEntry struct {
	Plan      string `json:"plan"`
	Model     string `json:"model"`
	Remaining int64  `json:"remaining"`
	Total     int64  `json:"total"`
	Used      int64  `json:"used"`
	ExpiresAt string `json:"expires_at"` // CST8
}

// AccountBalance 单账号余额查询结果。
type AccountBalance struct {
	Account string         `json:"account"`
	File    string         `json:"file"`
	UID     string         `json:"uid,omitempty"`
	Err     string         `json:"err,omitempty"`
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
		Code int `json:"code"`
		Data struct {
			Balances []struct {
				PlanID     string `json:"plan_id"`
				ShowName   string `json:"show_name"`
				TotalUnits int64  `json:"total_units"`
				UsedUnits  int64  `json:"used_units"`
				Remaining  int64  `json:"remaining_units"`
				Available  int64  `json:"available_units"`
				ExpiresAt  int64  `json:"expires_at"`
			} `json:"balances"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return out, fmt.Errorf("HTTP %d 非 JSON 响应: %.80s", resp.StatusCode, string(data))
	}
	if payload.Code != 0 {
		out.Err = fmt.Sprintf("code=%d", payload.Code)
		return out, nil
	}
	out.Entries = []BalanceEntry{}
	for _, b := range payload.Data.Balances {
		remain := b.Remaining
		if remain == 0 {
			remain = b.Available
		}
		exp := ""
		if b.ExpiresAt > 0 {
			exp = time.Unix(b.ExpiresAt, 0).In(CST8).Format("01-02 15:04")
		}
		out.Entries = append(out.Entries, BalanceEntry{
			Plan:      b.PlanID,
			Model:     b.ShowName,
			Remaining: remain,
			Total:     b.TotalUnits,
			Used:      b.UsedUnits,
			ExpiresAt: exp,
		})
	}
	return out, nil
}
