package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/zcode"
)

// fakeUpstream 假上游：校验收到的 anthropic body 并回放固定 SSE。
func fakeUpstream(t *testing.T, sse string) *httptest.Server {
	t.Helper()
	wantSeg0 := zcode.SystemPrefix()[0]
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("上游收到的请求不是合法 JSON: %v", err)
			w.WriteHeader(400)
			return
		}
		sys, _ := body["system"].([]any)
		if len(sys) == 0 {
			t.Error("上游请求缺 system 前缀（风控唯一键）")
		} else if sys[0].(map[string]any)["text"] != wantSeg0 {
			t.Errorf("system[0] 不是白名单 seg0: %v", sys[0])
		}
		if body["stream"] != true {
			t.Error("上游请求应恒为 stream=true")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
}

const sampleSSE = "data: " + `{"type":"message_start","message":{"usage":{"input_tokens":12}}}` + "\n" +
	"data: " + `{"type":"content_block_delta","delta":{"type":"text_delta","text":"你好"}}` + "\n" +
	"data: " + `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"想一下"}}` + "\n" +
	"data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n" +
	"data: [DONE]" + "\n\n"

func newTestServer(t *testing.T, upstream string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Upstream = upstream
	cfg.DumpDir = "" // 测试不落盘
	return New(cfg, zcode.New(upstream, "test-token-short"))
}

// 非流式：SSE 聚合为 chat.completion，usage/finish/reasoning 全到位。
func TestAggregateChat(t *testing.T) {
	up := fakeUpstream(t, sampleSSE)
	defer up.Close()
	s := newTestServer(t, up.URL)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["object"] != "chat.completion" || out["model"] != "GLM-5.3-Flash" {
		t.Fatalf("外层形状错误: %v", out)
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "你好" || msg["reasoning_content"] != "想一下" {
		t.Fatalf("聚合内容错误: %v", msg)
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason 应为 stop, got %v", choice["finish_reason"])
	}
	usage := out["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 12 || usage["completion_tokens"].(float64) != 5 {
		t.Fatalf("usage 错误: %v", usage)
	}
}

// 流式：chunk 帧 role→content→reasoning→finish→[DONE]。
func TestStreamChat(t *testing.T) {
	up := fakeUpstream(t, sampleSSE)
	defer up.Close()
	s := newTestServer(t, up.URL)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `"role":"assistant"`) ||
		!strings.Contains(body, `"content":"你好"`) ||
		!strings.Contains(body, `"reasoning_content":"想一下"`) ||
		!strings.Contains(body, `"finish_reason":"stop"`) ||
		!strings.Contains(body, "[DONE]") {
		t.Fatalf("流式帧缺失关键内容:\n%s", body)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("Content-Type 应为 text/event-stream, got %v", rec.Header())
	}
}

// 上游 405/3012 → 502 upstream_waf；401 → 401。
func TestUpstreamErrorMapping(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(405)
		_, _ = w.Write([]byte(`{"error":{"code":3012,"msg":"unusual activity"}}`))
	}))
	defer up.Close()
	s := newTestServer(t, up.URL)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 502 {
		t.Fatalf("405 应映射 502, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "3012") || !strings.Contains(rec.Body.String(), "upstream_waf") {
		t.Fatalf("错误体缺 3012/upstream_waf: %s", rec.Body.String())
	}

	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
	}))
	defer up2.Close()
	s2 := newTestServer(t, up2.URL)
	rec2 := httptest.NewRecorder()
	s2.Handler().ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`)))
	if rec2.Code != 401 || !strings.Contains(rec2.Body.String(), "upstream_auth") {
		t.Fatalf("401 应原样透传+upstream_auth: %d %s", rec2.Code, rec2.Body.String())
	}
}

// 入口鉴权：api_key 非空时 Bearer 命中放行、错 key 401。
func TestAuth(t *testing.T) {
	up := fakeUpstream(t, sampleSSE)
	defer up.Close()
	cfg := config.Default()
	cfg.Upstream = up.URL
	cfg.APIKey = "k2api-test-key"
	s := New(cfg, zcode.New(up.URL, "test-token-short"))

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("无凭据应 401, got %d", rec.Code)
	}

	req2 := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer k2api-test-key")
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("正确 Bearer 应 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// 401 自动重读 token 并重试一次：loader 返回新值则第二次请求成功。
func TestAuthRetryOn401(t *testing.T) {
	tries := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tries++
		if tries == 1 {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Authorization") != "Bearer refreshed-token" {
			t.Errorf("重试应携带新 token, got %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sampleSSE))
	}))
	defer up.Close()
	s := newTestServer(t, up.URL)
	s.Client.Token = "stale-token"
	s.TokenLoader = func() string { return "refreshed-token" }

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("401 后重试应成功, got %d: %s", rec.Code, rec.Body.String())
	}
	if tries != 2 {
		t.Fatalf("应重试恰好一次, tries=%d", tries)
	}

	// loader 返回同值 → 不重试，401 透传
	tries = 0
	s2 := newTestServer(t, up.URL)
	s2.Client.Token = "stale-token"
	s2.TokenLoader = func() string { return "stale-token" }
	rec2 := httptest.NewRecorder()
	s2.Handler().ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`)))
	if rec2.Code != 401 || tries != 1 {
		t.Fatalf("无新值应透传 401 且不重试: code=%d tries=%d", rec2.Code, tries)
	}
}

// models / health。
func TestModelsAndHealth(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	data := out["data"].([]any)
	ids := map[string]bool{}
	for _, d := range data {
		ids[d.(map[string]any)["id"].(string)] = true
	}
	for _, want := range []string{"GLM-5.3-Flash", "GLM-5.3", "GLM-4.7"} {
		if !ids[want] {
			t.Fatalf("models 应含 %s, got %v", want, data)
		}
	}

	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, httptest.NewRequest("GET", "/health", nil))
	var h map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &h)
	if h["ok"] != true {
		t.Fatalf("health 应 ok, got %v", h)
	}
	if _, has := h["token"]; has {
		t.Fatal("health 不得回传 token 内容")
	}
	if h["token_chars"].(float64) != float64(len("test-token-short")) {
		t.Fatalf("token_chars 错误: %v", h["token_chars"])
	}
}
