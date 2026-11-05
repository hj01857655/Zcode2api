package zcode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// FetchCaptchaConfig 解析 client/configs 的 data.configs.captcha（zcode-switch 同形状）。
func TestFetchCaptchaConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != CaptchaConfigsPath {
			t.Errorf("路径不符: %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Device-Mid"); got != "mid-1" {
			t.Errorf("device_mid 头缺失: %q", got)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"configs":{"captcha":{"enabled":true,"region":"cn","prefix":"no8xfe","sceneId":"11xygtvd"}}}}`))
	}))
	defer srv.Close()

	old := ClaimOrigin
	ClaimOrigin = srv.URL
	defer func() { ClaimOrigin = old }()

	c, err := FetchCaptchaConfig(srv.Client(), "jwt-x", "mid-1")
	if err != nil {
		t.Fatalf("FetchCaptchaConfig: %v", err)
	}
	if !c.Enabled || c.SceneID != "11xygtvd" || c.Prefix != "no8xfe" || c.Region != "cn" {
		t.Fatalf("解析结果不符: %+v", c)
	}
}

// 配置缺失时 Enabled=false、场景号为空，调用方应跳过预求解。
func TestFetchCaptchaConfigMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"configs":{}}}`))
	}))
	defer srv.Close()

	old := ClaimOrigin
	ClaimOrigin = srv.URL
	defer func() { ClaimOrigin = old }()

	c, err := FetchCaptchaConfig(srv.Client(), "jwt-x", "mid-1")
	if err != nil {
		t.Fatalf("FetchCaptchaConfig: %v", err)
	}
	if c.Enabled || c.SceneID != "" {
		t.Fatalf("缺失配置应得零值: %+v", c)
	}
}

// ---- 当日领取台账 ----

func TestClaimLedger(t *testing.T) {
	old := claimLedgerDir
	// 注意：Windows + 当前 Go 下 t.TempDir() 每次调用返回新目录，必须在闭包外取一次
	d := t.TempDir()
	claimLedgerDir = func() string { return d }
	defer func() { claimLedgerDir = old }()

	jwt := "jwt-ledger-test"
	if isClaimedToday(jwt, "plan-a") {
		t.Fatal("初始不应有台账")
	}
	markClaimed(jwt, "plan-a")
	if !isClaimedToday(jwt, "plan-a") {
		t.Fatal("领取成功后应跳过同 plan")
	}
	if isClaimedToday(jwt, "plan-b") {
		t.Fatal("新 plan_id 不受影响，必须仍可领")
	}
	if isClaimedToday("jwt-other", "plan-a") {
		t.Fatal("台账按账号隔离")
	}
	markClaimed(jwt, "plan-a") // 重复落账幂等
	if !isClaimedToday(jwt, "plan-a") {
		t.Fatal("重复 mark 后仍为已领")
	}
	// 跨日过期：手改台账日期
	l := loadLedger()
	l.Date = "2000-01-01"
	raw, _ := json.Marshal(l)
	_ = os.WriteFile(filepath.Join(claimLedgerDir(), "zcode_claimed.json"), raw, 0o600)
	if isClaimedToday(jwt, "plan-a") {
		t.Fatal("非当日台账必须失效")
	}
}
