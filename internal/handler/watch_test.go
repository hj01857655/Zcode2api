package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/zcode"
)

// newTestPool 临时 token.json 的多账号池（label→jwt 一一对应）。
func newTestPool(t *testing.T, labels []string) (*zcode.Pool, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token.json")
	pool := zcode.LoadPool(path)
	for _, l := range labels {
		pool.Upsert(zcode.Account{Label: l, Provider: "zai", Token: "jwt-" + l})
	}
	if err := pool.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return pool, path
}

// touchPayload 投放响应体：一个投放，plan_id 可指定。
func touchPayload(planID string) string {
	return fmt.Sprintf(`{"code":0,"data":{"deliveries":[{"resource_position":"p1","campaign_id":"c1","banner":{"buttons":[{"action":{"type":"claim_zcode_plan","args":{"plan_id":"%s"}}}]}}]}}`, planID)
}

// ---- 企业微信播报 ----

func TestNotifyWeCom(t *testing.T) {
	var got map[string]any
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	if err := NotifyWeCom(srv.URL, "测试播报"); err != nil {
		t.Fatalf("NotifyWeCom: %v", err)
	}
	if calls != 1 {
		t.Fatalf("应调用一次 webhook，实际 %d", calls)
	}
	if got["msgtype"] != "text" {
		t.Fatalf("msgtype 应为 text: %v", got)
	}
	text := got["text"].(map[string]any)
	if text["content"] != "测试播报" {
		t.Fatalf("content 不符: %v", text)
	}

	// 业务错误码非 0 视为失败
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errcode":93000,"errmsg":"invalid webhook"}`))
	}))
	defer srv2.Close()
	if err := NotifyWeCom(srv2.URL, "x"); err == nil {
		t.Fatal("errcode!=0 应返回错误")
	}
}

func TestNotifyWeComEmptyURL(t *testing.T) {
	if err := NotifyWeCom("", "x"); err != nil {
		t.Fatalf("未配置 webhook 应静默跳过: %v", err)
	}
}

func TestTruncateRunes(t *testing.T) {
	long := make([]rune, 700)
	for i := range long {
		long[i] = '测'
	}
	got := truncateRunes(string(long), 600)
	if n := len([]rune(got)); n != 600 {
		t.Fatalf("截断后应为 600 rune，实际 %d", n)
	}
}

// ---- 投放探测 ----

func TestDetectDeliveryUnionAcrossAccounts(t *testing.T) {
	oldDir := claimStateDir
	claimStateDir = t.TempDir
	defer func() { claimStateDir = oldDir }()

	// 两个账号各自投放不同 plan_id：探测应返回并集；第三个账号 touch 失败不影响
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer jwt-a":
			_, _ = w.Write([]byte(touchPayload("plan-a")))
		case "Bearer jwt-b":
			_, _ = w.Write([]byte(touchPayload("plan-b")))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":-1}`))
		}
	}))
	defer srv.Close()
	old := zcode.ClaimOrigin
	zcode.ClaimOrigin = srv.URL
	defer func() { zcode.ClaimOrigin = old }()

	_, poolPath := newTestPool(t, []string{"a", "b", "c"})
	client := zcode.New(srv.URL, "")
	client.Pool = zcode.LoadPool(poolPath)

	pids := detectDelivery(client, nil)
	if len(pids) != 2 || pids[0] != "plan-a" || pids[1] != "plan-b" {
		t.Fatalf("应得排序并集 [plan-a plan-b]，实际 %v", pids)
	}
}

func TestDetectDeliveryEmpty(t *testing.T) {
	if pids := detectDelivery(zcode.New("https://unused", ""), nil); pids != nil {
		t.Fatalf("无账号应返回 nil，实际 %v", pids)
	}
}

// ---- 有界并发抢批 ----

func TestClaimAllConcurrentBounded(t *testing.T) {
	oldDir := claimStateDir
	claimStateDir = t.TempDir
	defer func() { claimStateDir = oldDir }()

	var active, maxActive atomic.Int32
	oldOne := claimOneFunc
	claimOneFunc = func(jwt, mid string) string {
		n := active.Add(1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		active.Add(-1)
		return "done " + jwt
	}
	defer func() { claimOneFunc = oldOne }()

	_, poolPath := newTestPool(t, []string{"a", "b", "c", "d", "e"})
	client := zcode.New("https://unused", "")
	client.Pool = zcode.LoadPool(poolPath)

	results := ClaimAllConcurrent(config.Default(), client, "", 2, nil)
	if len(results) != 5 {
		t.Fatalf("5 个账号应全有结果，实际 %d", len(results))
	}
	if max := maxActive.Load(); max > 2 {
		t.Fatalf("并发上限 2 被突破，峰值 %d", max)
	}
	for _, r := range results {
		if r.Message == "" {
			t.Fatalf("%s 结果为空", r.Account)
		}
	}
}

func TestClaimAllConcurrentSingleAccount(t *testing.T) {
	oldDir := claimStateDir
	claimStateDir = t.TempDir
	defer func() { claimStateDir = oldDir }()

	oldOne := claimOneFunc
	claimOneFunc = func(jwt, mid string) string { return "ok" }
	defer func() { claimOneFunc = oldOne }()

	client := zcode.New("https://unused", "jwt-single")
	results := ClaimAllConcurrent(config.Default(), client, "", 0, nil)
	if len(results) != 1 || results[0].Account != "default" {
		t.Fatalf("单账号模式应得 [default]，实际 %+v", results)
	}
}

// ---- 播报文案 ----

func TestBuildClaimSummary(t *testing.T) {
	summary := buildClaimSummary([]string{"plan-a"},
		[]zcode.ClaimResult{{Account: "a", OK: true, Message: "领取成功: plan-a"}})
	if !strings.Contains(summary, "plan-a") || !strings.Contains(summary, "领取成功") {
		t.Fatalf("文案缺关键信息: %q", summary)
	}
}
