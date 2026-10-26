package server

// OpenAI Responses API 入口（/v1/responses）。
// 请求转换：input/instructions → Anthropic messages/system；max_output_tokens → max_tokens；
// reasoning.effort → output_config.effort。
// 响应转换：Anthropic message → Responses 形状（output[] + usage）；流式输出 response.* 事件集。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"zcode2api/internal/zcode"
)

// ResponsesRequest Responses API 入站（只取本网关支持的字段）。
type ResponsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Reasoning       *struct {
		Effort string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`
	Tools      json.RawMessage `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	Text       *struct {
		Format *struct {
			Type        string          `json:"type"`
			Name        string          `json:"name,omitempty"`
			Description string          `json:"description,omitempty"`
			Schema      json.RawMessage `json:"schema,omitempty"`
		} `json:"format,omitempty"`
	} `json:"text,omitempty"`
}

// inputItems Responses 的 input 项（宽容解析）。
type inputItem struct {
	Type    string          `json:"type,omitempty"`
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	// function_call（assistant 发起的工具调用）
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// function_call_output（客户端回传的工具结果）
	Output json.RawMessage `json:"output,omitempty"`
}

// BuildAnthropicFromResponses 把 Responses 请求转成 Anthropic body。
func BuildAnthropicFromResponses(req *ResponsesRequest, prefix []string) ([]byte, error) {
	var sysExtra []string
	if strings.TrimSpace(req.Instructions) != "" {
		sysExtra = append(sysExtra, req.Instructions)
	}
	// text.format → 提示词约束（Anthropic 上游无原生 structured output）
	if req.Text != nil && req.Text.Format != nil {
		switch req.Text.Format.Type {
		case "json_schema":
			hint := "必须只输出符合以下 JSON Schema 的 JSON 对象，不要输出任何解释、markdown 代码块或多余文字：\n"
			if len(req.Text.Format.Schema) > 0 {
				hint += string(req.Text.Format.Schema)
			}
			sysExtra = append(sysExtra, hint)
		case "json_object":
			sysExtra = append(sysExtra, "必须只输出一个 JSON 对象，不要输出任何解释或 markdown 代码块。")
		}
	}
	var msgs []map[string]any

	// input 可为字符串（单条 user）或 item 数组
	var asString string
	if json.Unmarshal(req.Input, &asString) == nil {
		if strings.TrimSpace(asString) != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": asString})
		}
	} else {
		var items []inputItem
		if err := json.Unmarshal(req.Input, &items); err == nil {
			for _, it := range items {
				role := it.Role
				if role == "" {
					role = "user"
				}
				text := flattenContent(it.Content)
				switch it.Type {
				case "function_call":
					// assistant 侧发起的工具调用 → tool_use 块
					input := map[string]any{}
					if it.Arguments != "" {
						var m map[string]any
						if json.Unmarshal([]byte(it.Arguments), &m) == nil {
							input = m
						} else {
							input = map[string]any{"_raw": it.Arguments}
						}
					}
					id := it.CallID
					if id == "" {
						id = "call_" + zcode.UUIDv4()[:16]
					}
					msgs = append(msgs, map[string]any{"role": "assistant", "content": []map[string]any{{
						"type": "tool_use", "id": id, "name": it.Name, "input": input,
					}}})
					continue
				case "function_call_output":
					// 客户端回传的工具结果 → user tool_result 块
					outText := flattenContent(it.Output)
					if outText == "" {
						outText = string(it.Output)
					}
					msgs = append(msgs, map[string]any{"role": "user", "content": []map[string]any{{
						"type": "tool_result", "tool_use_id": it.CallID, "content": outText,
					}}})
					continue
				}
				switch role {
				case "system", "developer":
					if text != "" {
						sysExtra = append(sysExtra, text)
					}
				case "assistant":
					if text != "" {
						msgs = append(msgs, map[string]any{"role": "assistant", "content": text})
					}
				default:
					if text != "" {
						msgs = append(msgs, map[string]any{"role": "user", "content": text})
					}
				}
			}
		}
	}
	if len(msgs) == 0 {
		msgs = append(msgs, map[string]any{"role": "user", "content": ""})
	}

	system := make([]map[string]any, 0, len(prefix)+1)
	for _, t := range prefix {
		system = append(system, map[string]any{"type": "text", "text": t})
	}
	if len(sysExtra) > 0 {
		system = append(system, map[string]any{"type": "text", "text": strings.Join(sysExtra, "\n\n")})
	}

	maxTok := req.MaxOutputTokens
	if maxTok <= 0 {
		maxTok = 4096
	}
	if maxTok > 128000 {
		maxTok = 128000
	}
	out := map[string]any{
		"model":      zcode.MapModel(req.Model),
		"messages":   msgs,
		"max_tokens": maxTok,
		"stream":     true, // 上游一律流式，非流式由 handler 聚合
		"system":     system,
		"metadata":   map[string]any{"user_id": zcode.UserIDString(zcode.UUIDv4())},
	}
	if req.Reasoning != nil {
		switch strings.ToLower(req.Reasoning.Effort) {
		case "low", "high", "max":
			out["output_config"] = map[string]any{"effort": strings.ToLower(req.Reasoning.Effort)}
		}
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	// tools：Responses 的 {type:"function",name,description,parameters} → Anthropic {name,description,input_schema}
	if tc := zcode.MapToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if len(req.Tools) > 0 {
		var raw []map[string]any
		if json.Unmarshal(req.Tools, &raw) == nil {
			var conv []map[string]any
			for _, t := range raw {
				typ, _ := t["type"].(string)
				if typ != "" && typ != "function" {
					continue // 内置工具（web_search 等）上游不支持
				}
				name, _ := t["name"].(string)
				if name == "" {
					if fn, ok := t["function"].(map[string]any); ok {
						name, _ = fn["name"].(string)
					}
				}
				if name == "" {
					continue
				}
				desc, _ := t["description"].(string)
				params := t["parameters"]
				if params == nil {
					if fn, ok := t["function"].(map[string]any); ok {
						params = fn["parameters"]
					}
				}
				if params == nil {
					params = map[string]any{"type": "object", "properties": map[string]any{}}
				}
				conv = append(conv, map[string]any{
					"name": name, "description": desc, "input_schema": params,
				})
			}
			if len(conv) > 0 {
				out["tools"] = conv
			}
		}
	}
	return json.Marshal(out)
}

// flattenContent Responses content 块数组 → 纯文本。
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if t, ok := p["text"].(string); ok && t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(w, r) {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "read body: "+err.Error(), "invalid_request_error")
		return
	}
	var req ResponsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid json", "invalid_request_error")
		return
	}
	body, err := BuildAnthropicFromResponses(&req, s.Prefix)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "build body: "+err.Error(), "zcode2api_error")
		return
	}
	var meta struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &meta)
	if req.Stream {
		s.streamResponses(w, meta.Model, body)
		return
	}
	s.aggregateResponses(w, meta.Model, body)
}

// responsesObject 组装 Responses 响应对象。
func responsesObject(id, model, text, status string, in, out int) map[string]any {
	return responsesObjectWithTools(id, model, text, status, in, out, nil)
}

// responsesObjectWithTools 同 responsesObject，附带 function_call 输出项。
func responsesObjectWithTools(id, model, text, status string, in, out int, tc *toolAccum) map[string]any {
	content := []map[string]any{}
	if text != "" {
		content = append(content, map[string]any{
			"type": "output_text", "text": text, "annotations": []any{},
		})
	}
	output := []map[string]any{}
	if text != "" || tc == nil || tc.empty() {
		output = append(output, map[string]any{
			"id": "msg_" + zcode.UUIDv4()[:24], "type": "message", "status": status,
			"role": "assistant", "content": content,
		})
	}
	if tc != nil {
		for _, idx := range tc.order {
			c := tc.byIdx[idx]
			args := c["args"]
			if args == "" {
				args = "{}"
			}
			output = append(output, map[string]any{
				"id": "fc_" + zcode.UUIDv4()[:24], "type": "function_call", "status": status,
				"call_id": c["id"], "name": c["name"], "arguments": args,
			})
		}
	}
	return map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      model,
		"output":     output,
		"usage": map[string]any{
			"input_tokens": in, "output_tokens": out, "total_tokens": in + out,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		},
	}
}

// aggregateResponses 非流式：聚合上游 SSE → 单个 response 对象。
func (s *Server) aggregateResponses(w http.ResponseWriter, model string, body []byte) {
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
	text := ""
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
		text += d.Content
	}
	w.Header().Set("Content-Type", "application/json")
	var tcPtr *toolAccum
	if !acc.empty() {
		tcPtr = acc
	}
	_ = json.NewEncoder(w).Encode(responsesObjectWithTools("resp_"+zcode.UUIDv4()[:24], model, text, "completed", in, out, tcPtr))
}

// streamResponses 流式：Anthropic SSE → Responses 事件集。
func (s *Server) streamResponses(w http.ResponseWriter, model string, body []byte) {
	resp, err := s.upstreamMessages(body)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	if err != nil {
		msg := "upstream: " + err.Error()
		if ue, ok := err.(*zcode.UpstreamError); ok {
			switch {
			case ue.Status == http.StatusUnauthorized:
				msg = "token 已失效，运行 zcode2api-go.exe login 重新授权"
			case ue.Status == http.StatusMethodNotAllowed || strings.Contains(ue.Body, "3012"):
				msg = "上游风控拒绝（3012）：system 白名单失效"
			case ue.Status == http.StatusTooManyRequests:
				msg = "上游限流/额度耗尽（429）"
			default:
				msg = fmt.Sprintf("upstream %d: %s", ue.Status, truncate(ue.Body, 400))
			}
		}
		sse(w, map[string]any{"type": "error", "error": map[string]any{"message": msg}})
		return
	}
	defer resp.Body.Close()
	flusher, _ := w.(http.Flusher)

	rid := "resp_" + zcode.UUIDv4()[:24]
	mid := "msg_" + zcode.UUIDv4()[:24]
	sse(w, map[string]any{"type": "response.created", "response": responsesObject(rid, model, "", "in_progress", 0, 0)})
	sse(w, map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"id": mid, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
	})
	if flusher != nil {
		flusher.Flush()
	}

	text := ""
	var in, out int
	acc := newToolAccum()
	started := map[int]bool{}
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
			sse(w, map[string]any{"type": "error", "error": map[string]any{"message": truncate(d.ErrText, 400)}})
			return
		}
		if d.UsageIn != nil {
			in = int(*d.UsageIn)
		}
		if d.UsageOut != nil {
			out = int(*d.UsageOut)
		}
		if d.ToolID != "" {
			acc.add(d)
			started[d.BlockIndex] = true
			sse(w, map[string]any{
				"type": "response.output_item.added", "output_index": d.BlockIndex + 1,
				"item": map[string]any{
					"id": "fc_" + zcode.UUIDv4()[:24], "type": "function_call",
					"status": "in_progress", "call_id": d.ToolID, "name": d.ToolName, "arguments": "",
				},
			})
			if flusher != nil {
				flusher.Flush()
			}
		}
		if d.ToolArgs != "" {
			acc.add(d)
			sse(w, map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": "fc_" + zcode.UUIDv4()[:24],
				"output_index": d.BlockIndex + 1, "delta": d.ToolArgs,
			})
			if flusher != nil {
				flusher.Flush()
			}
		}
		if d.Content != "" {
			text += d.Content
			sse(w, map[string]any{
				"type": "response.output_text.delta", "item_id": mid, "output_index": 0,
				"content_index": 0, "delta": d.Content,
			})
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	var tcPtr *toolAccum
	if !acc.empty() {
		tcPtr = acc
	}
	obj := responsesObjectWithTools(rid, model, text, "completed", in, out, tcPtr)
	sse(w, map[string]any{
		"type": "response.output_item.done", "output_index": 0,
		"item": map[string]any{
			"id": mid, "type": "message", "status": "completed", "role": "assistant",
			"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
		},
	})
	sse(w, map[string]any{"type": "response.completed", "response": obj})
}

// sse 写一帧 Responses 事件（event 行 + data 行）。
func sse(w io.Writer, obj map[string]any) {
	b, err := json.Marshal(obj)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %v\ndata: %s\n\n", obj["type"], b)
}
