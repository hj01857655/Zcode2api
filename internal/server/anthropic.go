package server

// Anthropic Messages 原生入口（/v1/messages）。
// Claude Code / Cline / Cherry Studio 等直连 Anthropic 协议的客户端走这里。
// 与 chat/completions 的差别：不做 OpenAI 转换，请求体即上游形状，只做 system 前缀注入、
// model 映射与 thinking 归一，然后流式透传上游响应。

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"zcode2api/internal/zcode"
)

// InjectSystemPrefix 把白名单前缀拼到客户端 system 之前。
// 客户端 system 可为 string 或 [{type:text,text}]；结果统一为块数组。
func InjectSystemPrefix(prefix []string, system any) []map[string]any {
	out := make([]map[string]any, 0, len(prefix)+2)
	for _, t := range prefix {
		out = append(out, map[string]any{"type": "text", "text": t})
	}
	switch v := system.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			out = append(out, map[string]any{"type": "text", "text": v})
		}
	case []any:
		var parts []string
		for _, it := range v {
			if m, ok := it.(map[string]any); ok {
				if t, ok := m["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) > 0 {
			out = append(out, map[string]any{"type": "text", "text": strings.Join(parts, "\n\n")})
		}
	}
	return out
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(w, r) {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "read body: "+err.Error(), "invalid_request_error")
		return
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json", "invalid_request_error")
		return
	}

	// model 映射（客户端可能传 claude-* 等别名，表外回落 GLM-5.3-Flash）
	if m, ok := req["model"].(string); ok {
		req["model"] = zcode.MapModel(m)
	}

	// system = 白名单前缀 + 客户端 system
	req["system"] = InjectSystemPrefix(s.Prefix, req["system"])

	// thinking 归一：Anthropic 客户端常发 {"type":"disabled"}，上游默认即无思考，
	// 显式 disabled 会让上游报 1210，故删除；enabled+budget_tokens 保留（兼容路径）。
	if th, ok := req["thinking"].(map[string]any); ok {
		if t, _ := th["type"].(string); t == "disabled" {
			delete(req, "thinking")
		}
	}

	// metadata.user_id 形状（风控不校验内容，补齐形状）
	if _, ok := req["metadata"]; !ok {
		req["metadata"] = map[string]any{"user_id": zcode.UserIDString(zcode.UUIDv4())}
	}

	if _, ok := req["max_tokens"]; !ok {
		req["max_tokens"] = 4096
	}

	// 上游一律 stream=true（非流式由本函数聚合），避免上游对非流式 JSON 回 br 压缩
	clientStream, _ := req["stream"].(bool)
	req["stream"] = true

	body, err := json.Marshal(req)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "encode: "+err.Error(), "zcode2api_error")
		return
	}
	model := ""
	if m, ok := req["model"].(string); ok {
		model = m
	}
	if !clientStream {
		s.aggregateMessages(w, model, body)
		return
	}
	s.passthroughMessages(w, body)
}

// aggregateMessages 非流式：聚合上游 SSE → Anthropic message 形状。
func (s *Server) aggregateMessages(w http.ResponseWriter, model string, body []byte) {
	resp, err := s.upstreamMessages(body)
	if err != nil {
		if ue, ok := err.(*zcode.UpstreamError); ok {
			s.mapUpstreamError(w, ue.Status, ue.Body)
			return
		}
		errJSON(w, http.StatusBadGateway, "upstream: "+err.Error(), "zcode2api_error")
		return
	}
	defer resp.Body.Close()

	var content []map[string]any
	var text, thinking, stop string
	var in, out int
	acc := newToolAccum()
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
			errJSON(w, http.StatusBadGateway, "upstream: "+d.ErrText, "zcode2api_error")
			return
		}
		if d.UsageIn != nil {
			in = int(*d.UsageIn)
		}
		if d.UsageOut != nil {
			out = int(*d.UsageOut)
		}
		acc.add(d)
		thinking += d.Reason
		text += d.Content
		if d.Finish != "" {
			stop = d.Finish
		}
	}
	if thinking != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": thinking})
	}
	// tool_use 块（Anthropic 原生形状：id / name / input 对象）
	for _, idx := range acc.order {
		c := acc.byIdx[idx]
		var input any = map[string]any{}
		if c["args"] != "" {
			var m map[string]any
			if json.Unmarshal([]byte(c["args"]), &m) == nil {
				input = m
			} else {
				input = map[string]any{"_raw": c["args"]}
			}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": c["id"], "name": c["name"], "input": input,
		})
	}
	if text != "" || len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	if !acc.empty() {
		stop = "tool_use"
	}
	if stop == "" {
		stop = "end_turn"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "msg_" + zcode.UUIDv4()[:24], "type": "message", "role": "assistant",
		"model": model, "content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": in, "output_tokens": out},
	})
}

// passthroughMessages 流式：原样透传上游 SSE 字节流。
func (s *Server) passthroughMessages(w http.ResponseWriter, body []byte) {
	resp, err := s.upstreamMessages(body)
	if err != nil {
		if ue, ok := err.(*zcode.UpstreamError); ok {
			s.mapUpstreamError(w, ue.Status, ue.Body)
			return
		}
		errJSON(w, http.StatusBadGateway, "upstream: "+err.Error(), "zcode2api_error")
		return
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/event-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
