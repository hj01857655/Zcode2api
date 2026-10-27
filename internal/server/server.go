// Package server 提供 OpenAI 兼容 HTTP 网关（纯 stdlib net/http）。
// 语义逐条对齐终版 server.py（2026-10-26，FastAPI:3010）：
//   - POST /v1/chat/completions：上游一律 stream=true，非流式聚合、流式逐帧翻译；
//   - GET /v1/models、GET /health；
//   - GET /admin 内置控制台（internal/handler），数据源 GET /admin/api/overview；
//   - 可选入口鉴权（Authorization: Bearer 或 x-api-key，api_key 为空则不鉴权）。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/handler"
	"zcode2api/internal/zcode"
)

// Server 装配好的网关实例。
type Server struct {
	Cfg    *config.Config
	Client *zcode.Client
	Prefix []string // system 白名单前缀（seg0+seg1）
	// TokenLoader 401 时的 token 重读函数（可注入测试）；默认读 env/配置文件。
	// 返回与当前不同的非空值则热替换 Client.Token 并重试一次。
	TokenLoader func() string
}

// New 装配。
func New(cfg *config.Config, client *zcode.Client) *Server {
	return &Server{Cfg: cfg, Client: client, Prefix: zcode.SystemPrefix()}
}

// upstreamMessages 发上游请求；401 时尝试重读 token 并重试一次
// （CLI/GUI 在别的进程重登后 credentials.json 已刷新，网关无需重启）。
// 重试只发生在向下游写出任何字节之前，流式/非流式都安全。
func (s *Server) upstreamMessages(body []byte) (*http.Response, error) {
	resp, err := s.Client.Messages(body)
	if err == nil {
		return resp, nil
	}
	if ue, ok := err.(*zcode.UpstreamError); ok && ue.Status == http.StatusUnauthorized && s.Client.Pool == nil {
		loader := s.TokenLoader
		if loader == nil {
			loader = func() string { return zcode.LoadToken(s.Cfg.TokenSource) }
		}
		if tok := loader(); tok != "" && tok != s.Client.Token {
			s.Client.Token = tok
			return s.Client.Messages(body)
		}
	}
	return resp, err
}

// Handler 返回根 mux。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	mux.HandleFunc("GET /admin", handler.PanelHandler())
	mux.HandleFunc("GET /admin/api/overview", handler.Overview(s.Cfg, s.Client))
	return mux
}

// errJSON OpenAI 错误形状（对齐 _err）。
func errJSON(w http.ResponseWriter, status int, msg, etype string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": etype},
	})
}

// checkAuth 对齐 _check_auth：api_key 为空放行；否则 Bearer 或 x-api-key 任一命中。
func (s *Server) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	key := s.Cfg.APIKey
	if key == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	got := ""
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		got = strings.TrimSpace(auth[7:])
	} else {
		got = r.Header.Get("x-api-key")
	}
	if got != key {
		errJSON(w, http.StatusUnauthorized, "invalid api key for this gateway", "invalid_request_error")
		return false
	}
	return true
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	seen := []map[string]any{}
	dup := map[string]bool{}
	for _, alias := range s.Cfg.ModelAlias {
		v := zcode.MapModel(alias)
		if !dup[v] {
			dup[v] = true
			seen = append(seen, map[string]any{"id": v, "object": "model", "owned_by": "zcode-start-plan"})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": seen})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.Client.Pool != nil {
		n := s.Client.Pool.Size()
		body := map[string]any{
			"ok": n > 0, "mode": "pool", "accounts": n,
			"account_list": s.Client.Pool.Snapshot(),
			"upstream":     s.Cfg.Upstream, "version": "2.1.0",
		}
		if n == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			body["error"] = "账号池为空，先执行 ./zcode2api-go.exe login"
		}
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	if len(s.Client.Token) == 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "token 未加载"})
		return
	}
	// 只回长度不回内容：token 不出网关
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "mode": "single", "token_chars": len(s.Client.Token),
		"upstream": s.Cfg.Upstream, "version": "2.1.0",
	})
}

// mapUpstreamError 对齐 _map_upstream_error：401=token 过期、405/3012=白名单失效、429=限流。
func (s *Server) mapUpstreamError(w http.ResponseWriter, status int, raw string) {
	switch {
	case status == http.StatusUnauthorized:
		errJSON(w, 401, "token 已失效且自动重读无新值 —— 运行 zcode2api-go.exe login 走 OAuth 重新授权，或重登 ZCode CLI/GUI 后自动恢复", "upstream_auth")
	case status == http.StatusMethodNotAllowed || strings.Contains(raw, "3012"):
		errJSON(w, 502, "上游风控拒绝（3012）：system 白名单前缀失效，ZCode 客户端更新后需重新抓包提取 system_prefix.json", "upstream_waf")
	case status == http.StatusTooManyRequests:
		errJSON(w, 429, "上游限流/额度耗尽（429）", "upstream_rate_limit")
	default:
		body := raw
		if len(body) > 400 {
			body = body[:400]
		}
		errJSON(w, 502, fmt.Sprintf("upstream %d: %s", status, body), "zcode2api_error")
	}
}

// toolAccum 按上游 content block index 累积 tool_use（id/name/参数增量）。
type toolAccum struct {
	order []int
	byIdx map[int]map[string]string // index → {id,name,args}
}

func newToolAccum() *toolAccum { return &toolAccum{byIdx: map[int]map[string]string{}} }

func (t *toolAccum) add(d *zcode.Delta) {
	if d.ToolID == "" && d.ToolArgs == "" {
		return
	}
	cur, ok := t.byIdx[d.BlockIndex]
	if !ok {
		cur = map[string]string{"id": "", "name": "", "args": ""}
		t.byIdx[d.BlockIndex] = cur
		t.order = append(t.order, d.BlockIndex)
	}
	if d.ToolID != "" {
		cur["id"] = d.ToolID
	}
	if d.ToolName != "" {
		cur["name"] = d.ToolName
	}
	if d.ToolArgs != "" {
		cur["args"] += d.ToolArgs
	}
}

// empty 无可转发工具块。
func (t *toolAccum) empty() bool { return len(t.order) == 0 }

// openAIToolCalls 组装 OpenAI tool_calls 数组（非流式）。
func (t *toolAccum) openAIToolCalls() []any {
	out := make([]any, 0, len(t.order))
	for _, idx := range t.order {
		c := t.byIdx[idx]
		args := c["args"]
		if args == "" {
			args = "{}"
		}
		out = append(out, map[string]any{
			"index": idx, "id": c["id"], "type": "function",
			"function": map[string]any{"name": c["name"], "arguments": args},
		})
	}
	return out
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(w, r) {
		return
	}
	var req zcode.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json", "invalid_request_error")
		return
	}
	model := zcode.MapModel(req.Model)
	body, err := zcode.BuildAnthropicBody(&req, s.Prefix)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "build body: "+err.Error(), "zcode2api_error")
		return
	}
	if req.Stream {
		s.streamChat(w, model, body)
		return
	}
	s.aggregateChat(w, model, body)
}

// aggregateChat 非流式：消费上游 SSE 聚合为单个 chat.completion（对齐 _collect_sse 路径）。
func (s *Server) aggregateChat(w http.ResponseWriter, model string, body []byte) {
	resp, err := s.upstreamMessages(body)
	if err != nil {
		if ue, ok := err.(*zcode.UpstreamError); ok {
			s.mapUpstreamError(w, ue.Status, ue.Body)
			return
		}
		errJSON(w, 502, "upstream: "+err.Error(), "zcode2api_error")
		return
	}
	defer resp.Body.Close()

	text, reasoning, finish := "", "", ""
	acc := newToolAccum()
	var in, out float64
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		d := zcode.TranslateSSELine(strings.TrimPrefix(line, "data: "))
		if d == nil {
			continue
		}
		if d.ErrText != "" {
			// 上游 error 事件 → 聚合中断，返回 502（对齐 sse_error 分支）
			errJSON(w, 502, "upstream: "+d.ErrText, "zcode2api_error")
			return
		}
		if d.UsageIn != nil {
			in = *d.UsageIn
		}
		if d.UsageOut != nil {
			out = *d.UsageOut
		}
		if d.Role != "" {
			// message_start 的 role 帧：帧本身不转发，只贡献上面的 input usage
			continue
		}
		acc.add(d)
		text += d.Content
		reasoning += d.Reason
		if d.Finish != "" {
			finish = d.Finish
		}
	}
	if err := sc.Err(); err != nil {
		errJSON(w, 502, "upstream: "+err.Error(), "zcode2api_error")
		return
	}
	if !acc.empty() {
		finish = "tool_calls"
	}
	if finish == "" {
		finish = zcode.MapStop("") // 上游未给 stop_reason 时兜底 stop
	}
	frame := zcode.CompletionFrame(
		"chatcmpl-"+zcode.UUIDv4()[:24], model, text, reasoning, finish, in, out)
	if !acc.empty() {
		if ch, ok := frame["choices"].([]any); ok && len(ch) > 0 {
			if c0, ok := ch[0].(map[string]any); ok {
				if msg, ok := c0["message"].(map[string]any); ok {
					msg["tool_calls"] = acc.openAIToolCalls()
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(frame)
}

// streamChat 流式：上游 SSE 逐帧翻译为 OpenAI chunk（对齐 gen() 生成器）。
func (s *Server) streamChat(w http.ResponseWriter, model string, body []byte) {
	resp, err := s.upstreamMessages(body)
	if err != nil {
		// 对齐 Python：失败也以 SSE error 帧 + [DONE] 收尾
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		var msg string
		if ue, ok := err.(*zcode.UpstreamError); ok {
			switch {
			case ue.Status == http.StatusUnauthorized:
				msg = "token 已失效且自动重读无新值 —— 运行 zcode2api-go.exe login 重新授权"
			case ue.Status == http.StatusMethodNotAllowed || strings.Contains(ue.Body, "3012"):
				msg = "上游风控拒绝（3012）：system 白名单失效，需重新提取 system_prefix.json"
			case ue.Status == http.StatusTooManyRequests:
				msg = "上游限流/额度耗尽（429）"
			default:
				b := ue.Body
				if len(b) > 400 {
					b = b[:400]
				}
				msg = fmt.Sprintf("upstream %d: %s", ue.Status, b)
			}
		} else {
			msg = "upstream: " + err.Error()
		}
		fmt.Fprint(w, "data: "+mustJSON(map[string]any{
			"error": map[string]any{"message": msg, "type": "upstream_error"}})+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		d := zcode.TranslateSSELine(strings.TrimPrefix(line, "data: "))
		if d == nil {
			continue
		}
		switch {
		case d.ErrText != "":
			fmt.Fprint(w, "data: "+mustJSON(map[string]any{
				"error": map[string]any{"message": truncate(d.ErrText, 400)}})+"\n\n")
		case d.Role != "":
			fmt.Fprint(w, "data: "+mustJSON(chunkFrame(model, map[string]any{"role": "assistant", "content": ""}, nil))+"\n\n")
		case d.ToolID != "":
			fmt.Fprint(w, "data: "+mustJSON(chunkFrame(model, map[string]any{
				"tool_calls": []any{map[string]any{
					"index": d.BlockIndex, "id": d.ToolID, "type": "function",
					"function": map[string]any{"name": d.ToolName, "arguments": ""},
				}},
			}, nil))+"\n\n")
		case d.ToolArgs != "":
			fmt.Fprint(w, "data: "+mustJSON(chunkFrame(model, map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    d.BlockIndex,
					"function": map[string]any{"arguments": d.ToolArgs},
				}},
			}, nil))+"\n\n")
		case d.Content != "":
			fmt.Fprint(w, "data: "+mustJSON(chunkFrame(model, map[string]any{"content": d.Content}, nil))+"\n\n")
		case d.Reason != "":
			fmt.Fprint(w, "data: "+mustJSON(chunkFrame(model, map[string]any{"reasoning_content": d.Reason}, nil))+"\n\n")
		case d.Finish != "":
			fmt.Fprint(w, "data: "+mustJSON(chunkFrame(model, map[string]any{}, d.Finish))+"\n\n")
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// chunkFrame 对齐 _sse：每帧新 id、finish_reason 独立帧。
func chunkFrame(model string, delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id": "chatcmpl-" + zcode.UUIDv4()[:24], "object": "chat.completion.chunk",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
