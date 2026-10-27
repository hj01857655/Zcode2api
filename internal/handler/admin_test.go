package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/zcode"
)

func getOverview(cfg *config.Config, client *zcode.Client, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	Overview(cfg, client)(rec, req)
	return rec
}

// 单账号模式：valid=true、mode=single，token 只回长度。
func TestOverviewSingleToken(t *testing.T) {
	cfg := config.Default()
	rec := getOverview(cfg, zcode.New("https://upstream.invalid", "tok-123"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var o struct {
		Service    string `json:"service"`
		Credential struct {
			Valid      bool   `json:"valid"`
			Mode       string `json:"mode"`
			TokenChars int    `json:"token_chars"`
			Notice     string `json:"notice"`
		} `json:"credential"`
		Models    []map[string]any `json:"models"`
		LocalAuth bool             `json:"local_auth"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if o.Service != "ZCode2api-go" {
		t.Errorf("service = %q", o.Service)
	}
	if !o.Credential.Valid || o.Credential.Mode != "single" || o.Credential.TokenChars != len("tok-123") {
		t.Errorf("credential = %+v", o.Credential)
	}
	if o.LocalAuth {
		t.Errorf("API_KEY 为空时 local_auth 应为 false")
	}
	if len(o.Models) == 0 {
		t.Errorf("models 不应为空")
	}
	hasDefault := false
	for _, m := range o.Models {
		if b, _ := m["default"].(bool); b {
			hasDefault = true
		}
	}
	if !hasDefault {
		t.Errorf("models 缺 default 标记: %v", o.Models)
	}
}

// 空账号池：仍 200，valid=false 并带 login 提示。
func TestOverviewEmptyPool(t *testing.T) {
	cfg := config.Default()
	client := zcode.New("https://upstream.invalid", "")
	client.Pool = &zcode.Pool{}
	rec := getOverview(cfg, client, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var o struct {
		Credential struct {
			Valid  bool   `json:"valid"`
			Mode   string `json:"mode"`
			Notice string `json:"notice"`
		} `json:"credential"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if o.Credential.Valid || o.Credential.Mode != "pool" || o.Credential.Notice == "" {
		t.Errorf("credential = %+v", o.Credential)
	}
}

// 设置 API_KEY 后无头 401，Bearer / x-api-key 任一命中放行（对齐 checkAuth）。
func TestOverviewAuth(t *testing.T) {
	cfg := config.Default()
	cfg.APIKey = "secret"
	client := zcode.New("https://upstream.invalid", "tok-123")
	if rec := getOverview(cfg, client, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("无凭据 status = %d, want 401", rec.Code)
	}
	if rec := getOverview(cfg, client, map[string]string{"Authorization": "Bearer secret"}); rec.Code != http.StatusOK {
		t.Errorf("Bearer status = %d, want 200", rec.Code)
	}
	if rec := getOverview(cfg, client, map[string]string{"x-api-key": "secret"}); rec.Code != http.StatusOK {
		t.Errorf("x-api-key status = %d, want 200", rec.Code)
	}
}

// 面板：200 + text/html + 内置页内容。
func TestPanel(t *testing.T) {
	rec := httptest.NewRecorder()
	PanelHandler()(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "ZCode2api-go") {
		t.Errorf("内置面板缺少标题")
	}
}
