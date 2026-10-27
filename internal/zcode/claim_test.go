package zcode

import (
	"net/http"
	"net/http/httptest"
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
