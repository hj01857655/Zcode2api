package zcode

// sticky 亲和与 Transport 调优测试（2026-11 网关优化）。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestNewTransportTuned(t *testing.T) {
	c := New("http://127.0.0.1:1", "tok")
	tr, ok := c.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 应为显式 *http.Transport, got %T", c.HTTP.Transport)
	}
	if tr.MaxIdleConnsPerHost <= 2 {
		t.Fatalf("MaxIdleConnsPerHost=%d：默认值 2 对单上游高并发必现重拨+TLS 握手", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConns(%d) 不得小于 PerHost(%d)", tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 应保持开启")
	}
	if tr.IdleConnTimeout <= 0 {
		t.Fatal("IdleConnTimeout 应显式设置")
	}
	if tr.DialContext == nil {
		t.Fatal("DialContext 应显式设置")
	}
}

func TestPoolPickStickyEmptyKeyDoesNotBind(t *testing.T) {
	pool := LoadPool(filepath.Join(t.TempDir(), "none.json"))
	pool.Upsert(Account{Label: "a", Token: "t1"})
	pool.Upsert(Account{Label: "b", Token: "t2"})
	// 空 key = 纯轮转：绝不写绑定表。chat/responses 路径的 user_id 是
	// 每请求随机值，进表只会污染 LRU，把直通路径的真实绑定逐出去
	for i := 0; i < 10; i++ {
		if _, ok := pool.PickSticky(""); !ok {
			t.Fatal("空 key 应退化轮转且取到号")
		}
	}
	if len(pool.sticky) != 0 {
		t.Fatalf("空 key 不得写绑定表, sticky=%v", pool.sticky)
	}
	// 非 key 才绑定
	pool.PickSticky("sess-real")
	if pool.sticky["sess-real"] == "" {
		t.Fatal("非空 key 应写入绑定表")
	}
}

func TestMessagesWithAccountStickyAffinity(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{} // Bearer token → 命中次数
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")]++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"type":"message_start","message":{"usage":{"input_tokens":1}}}` + "\n" +
			"data: [DONE]\n\n"))
	}))
	defer up.Close()

	pool := LoadPool(filepath.Join(t.TempDir(), "none.json"))
	pool.Upsert(Account{Label: "a", Token: "tok-a"})
	pool.Upsert(Account{Label: "b", Token: "tok-b"})
	c := New(up.URL, "")
	c.Pool = pool

	// 同一 sticky key 两次请求命中同一账号（同会话固定同号）
	for i := 0; i < 2; i++ {
		resp, label, err := c.MessagesWithAccount([]byte(`{}`), "uid-stable-1")
		if err != nil {
			t.Fatalf("请求 %d 失败: %v", i, err)
		}
		// 读尽再关：半途 Close 的连接不回 idle 池，且 timeout 计时器要等 body 终止才停
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if label == "" {
			t.Fatalf("请求 %d 未回填账号 label", i)
		}
	}
	// 同 sticky 两次请求的命中先断言（必须只有 tok-a 一个 token）
	mu.Lock()
	if len(seen) != 1 {
		t.Fatalf("同 sticky 应固定同一账号, tokens hit: %v", seen)
	}
	mu.Unlock()
	// 空 sticky（chat/responses 路径）不写绑定表。
	// 注意：网络调用必须在持锁段之外——handler 回调也要拿 mu 记 seen，
	// 持锁发请求 = 经典自死锁（曾表现为卡满 300s 后被 client.Timeout 打断）
	if resp, _, err := c.MessagesWithAccount([]byte(`{}`), ""); err != nil {
		t.Fatalf("空 sticky 请求失败: %v", err)
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if len(pool.sticky) != 1 || pool.sticky["uid-stable-1"] == "" {
		t.Fatalf("绑定表应只有 uid-stable-1 一条, sticky=%v", pool.sticky)
	}
}

func TestMessagesWithAccountRotatesWithoutSticky(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")]++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()

	pool := LoadPool(filepath.Join(t.TempDir(), "none.json"))
	pool.Upsert(Account{Label: "a", Token: "tok-a"})
	pool.Upsert(Account{Label: "b", Token: "tok-b"})
	c := New(up.URL, "")
	c.Pool = pool

	// 纯轮转：两次空 sticky 请求应分摊到两个号
	for i := 0; i < 2; i++ {
		resp, _, err := c.MessagesWithAccount([]byte(`{}`), "")
		if err != nil {
			t.Fatalf("请求 %d 失败: %v", i, err)
		}
		resp.Body.Close()
		time.Sleep(time.Millisecond) // Pick 无时序要求，仅防同刻并发干扰断言
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("空 sticky 应轮转覆盖两号, hit: %v", seen)
	}
	if len(pool.sticky) != 0 {
		t.Fatalf("空 sticky 不得写绑定表, sticky=%v", pool.sticky)
	}
}

func TestGlobalBackoffScoping(t *testing.T) {
	// 3008 = user 级并发上限：单号冷却 + 换号成功，不设全局闸；
	// 3012 = IP 级风控：必须设全局闸（否则继续撞风控白烧重试轮次）。
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("Authorization")
		if tok == "Bearer tok-a" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":3008,"msg":"user concurrency limit exceeded"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()

	pool := LoadPool(filepath.Join(t.TempDir(), "none.json"))
	pool.Upsert(Account{Label: "a", Token: "tok-a"})
	pool.Upsert(Account{Label: "b", Token: "tok-b"})
	c := New(up.URL, "")
	c.Pool = pool

	resp, label, err := c.MessagesWithAccount([]byte(`{}`), "")
	if err != nil {
		t.Fatalf("3008 后应换号成功: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if label != "b" {
		t.Fatalf("应换到 b 号, got %s", label)
	}
	if d := pool.GlobalBackoffRemaining(); d > 0 {
		t.Fatalf("3008 不得设全局退避闸, remaining=%v", d)
	}
	if until, cool := pool.cool["a"]; !cool || until.Before(time.Now()) {
		t.Fatal("3008 的号应进入单号冷却")
	}

	// 对照：3012 仍设全局闸
	pool2 := LoadPool(filepath.Join(t.TempDir(), "none.json"))
	pool2.Upsert(Account{Label: "a", Token: "tok-a"})
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"code":3012}`))
	}))
	defer up2.Close()
	c2 := New(up2.URL, "tok-a")
	c2.Pool = pool2
	_, _, _ = c2.MessagesWithAccount([]byte(`{}`), "")
	if d := pool2.GlobalBackoffRemaining(); d <= 0 {
		t.Fatal("3012 应设全局退避闸")
	}
}
