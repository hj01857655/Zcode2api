package server

// 网关优化回归（2026-11）：models 稳定排序 + /v1/messages 直通路径 sticky 亲和。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/zcode"
)

func TestModelsSorted(t *testing.T) {
	cfg := config.Default()
	cfg.Upstream = "http://127.0.0.1:1"
	// 乱序别名：ModelMap 白名单外的目标值一律回落 GLM-5.3-Flash（既有行为），
	// 去重后剩两个 id，输出必须按 id 升序稳定排序
	cfg.ModelAlias = map[string]string{
		"flashx":  "GLM-5.3-FlashX", // 白名单外 → 回落 GLM-5.3-Flash
		"glm-4":   "GLM-5.3-Flash",
		"glm-5.3": "GLM-5.3",
		"aha":     "GLM-5.3-Flash",
	}
	s := New(cfg, zcode.New(cfg.Upstream, "test-token-short"))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("models 输出应按 id 升序稳定排序, got %v", ids)
	}
	want := []string{"GLM-5.3", "GLM-5.3-Flash"}
	for _, w := range want {
		found := false
		for _, id := range ids {
			if id == w {
				found = true
			}
		}
		if !found {
			t.Fatalf("应含去重后的 %s, got %v", w, ids)
		}
	}
}

// stickyUpstream 记录上游收到的 Authorization，回放最小 SSE。
func stickyUpstream(t *testing.T) (*httptest.Server, func() map[string]int) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]int{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")]++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sampleSSE))
	}))
	return up, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

// /v1/messages 直通路径：客户端自带 metadata.user_id 是稳定会话标识，
// 两次请求必须命中同一账号（池内同号），且绑定表恰好一条。
func TestMessagesPassthroughStickyAffinity(t *testing.T) {
	up, snapshot := stickyUpstream(t)
	defer up.Close()

	s := newTestServer(t, up.URL)
	pool := zcode.LoadPool(filepath.Join(t.TempDir(), "none.json"))
	pool.Upsert(zcode.Account{Label: "a", Token: "tok-a"})
	pool.Upsert(zcode.Account{Label: "b", Token: "tok-b"})
	s.Client.Pool = pool

	send := func() {
		body := `{"model":"glm-5.3-flash","max_tokens":64,"metadata":{"user_id":"cc-user-1"},"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("下游应 200, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	send()
	send()

	hit := snapshot()
	if len(hit) != 1 {
		t.Fatalf("同 user_id 应固定同一账号, upstream tokens hit: %v", hit)
	}
	if len(pool.StickyCount()) != 1 {
		t.Fatalf("绑定表应恰好一条 user_id 绑定, got %v", pool.StickyCount())
	}
}

// chat/responses 路径（BuildAnthropicBody 每请求随机 user_id）不得污染绑定表：
// 两次 /v1/chat/completions 后，绑定表仍为空。
func TestChatDoesNotPolluteStickyTable(t *testing.T) {
	up, _ := stickyUpstream(t)
	defer up.Close()

	s := newTestServer(t, up.URL)
	pool := zcode.LoadPool(filepath.Join(t.TempDir(), "none.json"))
	pool.Upsert(zcode.Account{Label: "a", Token: "tok-a"})
	pool.Upsert(zcode.Account{Label: "b", Token: "tok-b"})
	s.Client.Pool = pool

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("下游应 200, got %d: %s", rec.Code, rec.Body.String())
		}
	}
	if n := len(pool.StickyCount()); n != 0 {
		t.Fatalf("chat 路径不得写绑定表, got %d 条", n)
	}
}
