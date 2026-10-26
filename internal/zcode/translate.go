// 翻译层：OpenAI chat/completions ↔ Anthropic messages。
// 语义逐条对齐终版 server.py（2026-10-26，FastAPI:3010），关键差异于旧版的点：
//   - system = 白名单前缀（seg0+seg1）+ 用户 system 追加段，无 cache_control（E5 实锤可去）；
//   - thinking 走官方 output_config.effort（low/high/max）；不传=上游默认无思考；
//   - 上游一律 stream=true，非流式由 handler 聚合；
//   - metadata.user_id 的 device_id 随机（风控不校验，形状照发）。
package zcode

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ModelMap 入站模型名 → 上游模型（GLM-5.3 / GLM-5.3-Flash / GLM-4.7 各自直通）。
var ModelMap = map[string]string{
	"glm-5.3-flash": "GLM-5.3-Flash",
	"glm-5.3":       "GLM-5.3",
	"glm-4.7":       "GLM-4.7",
}

// SentinelTools thinking=disabled 时必须携带的空壳工具（描述上劝退模型调用）。
var SentinelTools = []map[string]any{{
	"name":        "__noop",
	"description": "Internal placeholder tool. Never call it.",
	"input_schema": map[string]any{
		"type": "object", "properties": map[string]any{}, "required": []string{},
	},
}}

// MapModel：表内命中用映射值，否则回落 GLM-5.3-Flash。
func MapModel(name string) string {
	if m, ok := ModelMap[strings.ToLower(strings.TrimSpace(name))]; ok {
		return m
	}
	return "GLM-5.3-Flash"
}

// ---------- OpenAI 入站 ----------

// ChatMessage 入站消息；Content 兼容 string 与块数组两种形状。
type ChatMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
}

type OpenAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type ChatRequest struct {
	Model          string          `json:"model"`
	Messages       []ChatMessage   `json:"messages"`
	Stream         bool            `json:"stream"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	Stop           json.RawMessage `json:"stop,omitempty"`
	Thinking       string          `json:"thinking,omitempty"` // "enabled"|"disabled"（本网关扩展）
	ThinkingBudget int             `json:"thinking_budget,omitempty"`
	ToolChoice     json.RawMessage `json:"tool_choice,omitempty"`
	Tools          []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools,omitempty"`
}

// block 上游 content 块的宽松视图。
type block struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
	Function *struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function,omitempty"`
	ID string `json:"id"`
}

// flattenText content 归一为纯文本（system 提取用）。
func flattenText(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var out []string
	for _, p := range parts {
		if t, ok := p["text"].(string); ok && t != "" {
			out = append(out, t)
		}
	}
	return out
}

// safeJson tool arguments 宽松解析（终版 _safe_json 语义）。
func safeJSON(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil && m != nil {
		return m
	}
	return map[string]any{"_raw": string(raw)}
}

// dataImage 把 data:image URI 拆成 anthropic base64 image 块；非 data URI 返回 nil。
func dataImage(url string) map[string]any {
	const p = "data:image/"
	if !strings.HasPrefix(url, p) {
		return nil
	}
	meta, b64, ok := strings.Cut(url[len(p):], ",")
	if !ok {
		return nil
	}
	media := strings.SplitN(meta, ";", 2)[0]
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "base64", "media_type": "image/" + media, "data": b64,
		},
	}
}

// BuildAnthropicBody OpenAI 请求 → anthropic 请求（终版 _oai_to_anthropic 语义）。
// prefix 是白名单前缀（seg0+seg1 文本，来自 assets/prefix.json）。
func BuildAnthropicBody(req *ChatRequest, prefix []string) ([]byte, error) {
	var sysExtra []string
	msgs := []map[string]any{}

	for i := range req.Messages {
		m := &req.Messages[i]
		switch m.Role {
		case "system":
			sysExtra = append(sysExtra, flattenText(m.Content)...)
			continue
		case "tool":
			// 终版把 tool content 直接 str()；这里保持文本语义
			msgs = append(msgs, map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": m.ToolCallID,
					"content":     strings.Join(flattenText(m.Content), "\n"),
				}},
			})
			continue
		case "assistant":
			// 标准 OpenAI 形状：tool_calls 在消息级
			if len(m.ToolCalls) > 0 {
				parts := []map[string]any{}
				for _, t := range flattenText(m.Content) {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
				for _, tc := range m.ToolCalls {
					id := tc.ID
					if id == "" {
						id = "call_" + UUIDv4()[:16]
					}
					parts = append(parts, map[string]any{
						"type": "tool_use", "id": id,
						"name":  tc.Function.Name,
						"input": safeJSON(json.RawMessage(tc.Function.Arguments)),
					})
				}
				if len(parts) == 0 {
					parts = append(parts, map[string]any{"type": "text", "text": ""})
				}
				msgs = append(msgs, map[string]any{"role": "assistant", "content": parts})
				continue
			}
			// 兼容 content 数组里内联 function 块的形状（终版处理了它）
			var blocks []block
			if json.Unmarshal(m.Content, &blocks) == nil && blocks != nil {
				parts := []map[string]any{}
				for _, p := range blocks {
					switch {
					case p.Type == "text" && p.Text != "":
						parts = append(parts, map[string]any{"type": "text", "text": p.Text})
					case p.Function != nil:
						id := p.ID
						if id == "" {
							id = "call_" + UUIDv4()[:16]
						}
						parts = append(parts, map[string]any{
							"type": "tool_use", "id": id,
							"name": p.Function.Name, "input": safeJSON(p.Function.Arguments),
						})
					}
				}
				if len(parts) == 0 {
					parts = append(parts, map[string]any{"type": "text", "text": ""})
				}
				msgs = append(msgs, map[string]any{"role": "assistant", "content": parts})
				continue
			}
			msgs = append(msgs, map[string]any{"role": "assistant",
				"content": strings.Join(flattenText(m.Content), "\n")})
			continue
		default:
			// user 消息：支持视觉输入（data:image → base64 块）
			var blocks []block
			if json.Unmarshal(m.Content, &blocks) == nil && blocks != nil {
				parts := []map[string]any{}
				for _, p := range blocks {
					if p.Type == "text" && p.Text != "" {
						parts = append(parts, map[string]any{"type": "text", "text": p.Text})
					} else if p.ImageURL != nil {
						if img := dataImage(p.ImageURL.URL); img != nil {
							parts = append(parts, img)
						}
					}
				}
				if len(parts) == 0 {
					parts = append(parts, map[string]any{"type": "text", "text": ""})
				}
				msgs = append(msgs, map[string]any{"role": "user", "content": parts})
				continue
			}
			msgs = append(msgs, map[string]any{"role": "user",
				"content": strings.Join(flattenText(m.Content), "\n")})
		}
	}

	// system：白名单前缀 + 用户追加段（前缀匹配校验要求 seg0+seg1 打头）
	system := make([]map[string]any, 0, len(prefix)+1)
	for _, t := range prefix {
		system = append(system, map[string]any{"type": "text", "text": t})
	}
	if len(sysExtra) > 0 {
		system = append(system, map[string]any{"type": "text",
			"text": strings.Join(sysExtra, "\n\n")})
	}

	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 4096
	}
	if maxTok > 128000 {
		maxTok = 128000
	}
	out := map[string]any{
		"model":      MapModel(req.Model),
		"messages":   msgs,
		"max_tokens": maxTok,
		"stream":     true, // 上游一律流式；非流式由 handler 聚合
		"system":     system,
		"metadata":   map[string]any{"user_id": UserIDString(UUIDv4())},
	}
	// thinking：官方路径 output_config.effort（low/high/max）；不传 = 上游默认无思考。
	// 旧方案（thinking.disabled + 哨兵 tools + tool_choice none）实测导致上游返回空消息。
	effort := strings.ToLower(strings.TrimSpace(req.Thinking))
	if effort == "enabled" {
		effort = "high"
	}
	if effort != "low" && effort != "high" && effort != "max" {
		effort = ""
	}
	if effort != "" {
		out["output_config"] = map[string]any{"effort": effort}
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	// 用户自带 tools 原样透传
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			params := t.Function.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{
				"name":         t.Function.Name,
				"description":  t.Function.Description,
				"input_schema": json.RawMessage(params),
			})
		}
		out["tools"] = tools
	}
	// tool_choice：OpenAI 三种形态 → Anthropic
	//   "auto" → {"type":"auto"}；"required"/"any" → {"type":"any"}；
	//   {"type":"function","function":{"name":X}} → {"type":"tool","name":X}；
	//   "none" 不发（上游收到 none 会空响应）。
	if tc := MapToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if len(req.Stop) > 0 {
		var one string
		var many []string
		if json.Unmarshal(req.Stop, &one) == nil {
			out["stop_sequences"] = []string{one}
		} else if json.Unmarshal(req.Stop, &many) == nil {
			out["stop_sequences"] = many
		}
	}
	return json.Marshal(out)
}

// UserIDString metadata.user_id 的双层编码：JSON 对象再字符串化（cap[33] 形状）。
// device_id 风控不校验（F4/F6），随机值即可。
func UserIDString(deviceID string) string {
	inner, _ := json.Marshal(map[string]any{
		"device_id": deviceID, "account_uuid": "", "session_id": "",
	})
	return string(inner)
}

// ---------- 上游 SSE → OpenAI 出站 ----------

// StopMap anthropic stop_reason → OpenAI finish_reason。
var StopMap = map[string]string{
	"end_turn": "stop", "stop_sequence": "stop",
	"max_tokens": "length", "tool_use": "tool_calls",
}

// mapToolChoice OpenAI tool_choice → Anthropic tool_choice；无可映射值返回 nil。
func MapToolChoice(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required", "any":
			return map[string]any{"type": "any"}
		case "none":
			return nil
		}
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	name := obj.Function.Name
	if name == "" {
		name = obj.Name
	}
	if name == "" {
		return nil
	}
	return map[string]any{"type": "tool", "name": name}
}

// MapStop 终版 _map_stop 语义：空/未知一律 stop。
func MapStop(reason string) string {
	if m, ok := StopMap[reason]; ok {
		return m
	}
	return "stop"
}

// UpstreamEvent 上游 SSE 事件的宽松视图。
type UpstreamEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	// message_start
	Message *struct {
		Usage map[string]any `json:"usage"`
	} `json:"message,omitempty"`
	// content_block_start
	ContentBlock *struct {
		Type string `json:"type"`
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"content_block,omitempty"`
	// content_block_delta 与 message_delta 的 delta 形状互斥，合并为宽松超集
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		StopReason  string `json:"stop_reason"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta,omitempty"`
	Usage map[string]any `json:"usage,omitempty"`
	// error
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Delta 一次下游转发（content 或 reasoning 文本 + 可选 finish）。
type Delta struct {
	Role     string
	Content  string
	Reason   string
	Finish   string // 空 = 不发 finish
	ErrText  string // 非 JSON 上游负载或 error 事件
	UsageIn  *float64
	UsageOut *float64

	// 工具调用：上游 content_block_start(tool_use) 给 id/name，随后 input_json_delta 分批给参数。
	BlockIndex int    // 上游 content block index（对应 OpenAI tool_calls[].index）
	ToolID     string // toolu_xxx
	ToolName   string // 函数名
	ToolArgs   string // partial_json 增量
}

// TranslateSSELine 解析一行上游 data: 负载为转发动作；非 JSON/无关事件返回 nil。
func TranslateSSELine(payload string) *Delta {
	if payload == "" || payload == "[DONE]" {
		return nil
	}
	var ev UpstreamEvent
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return &Delta{ErrText: payload} // 非 JSON 负载原样透传（终版不吞）
	}
	switch ev.Type {
	case "message_start":
		d := &Delta{Role: "assistant"}
		if ev.Message != nil && ev.Message.Usage != nil {
			if v, ok := ev.Message.Usage["input_tokens"].(float64); ok {
				d.UsageIn = &v
			}
		}
		return d
	case "content_block_start":
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			return &Delta{
				BlockIndex: ev.Index,
				ToolID:     ev.ContentBlock.ID,
				ToolName:   ev.ContentBlock.Name,
			}
		}
		return nil
	case "content_block_delta":
		if ev.Delta == nil {
			return nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			return &Delta{Content: ev.Delta.Text}
		case "thinking_delta":
			return &Delta{Reason: ev.Delta.Thinking}
		case "input_json_delta":
			return &Delta{BlockIndex: ev.Index, ToolArgs: ev.Delta.PartialJSON}
		}
		return nil
	case "message_delta":
		d := &Delta{}
		if ev.Delta != nil {
			d.Finish = MapStop(ev.Delta.StopReason)
		}
		if ev.Usage != nil {
			if v, ok := ev.Usage["output_tokens"].(float64); ok {
				d.UsageOut = &v
			}
		}
		return d
	case "error":
		msg := ""
		if ev.Error != nil {
			msg = ev.Error.Message
		}
		return &Delta{ErrText: msg}
	}
	return nil
}

// CompletionFrame 非流式聚合结果 → OpenAI chat.completion（model 为映射后的上游模型名）。
func CompletionFrame(id, model, text, reasoning, finish string, in, out float64) map[string]any {
	msg := map[string]any{"role": "assistant", "content": text}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	return map[string]any{
		"id": id, "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finish}},
		"usage": map[string]any{
			"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out,
		},
	}
}

// Chunk 流式 chunk 帧（Python 终版 _sse 每帧新 id，id 由调用方生成）。
func Chunk(id, model string, delta map[string]any, finish any) ([]byte, error) {
	return json.Marshal(map[string]any{
		"id": id, "object": "chat.completion.chunk",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
}

// SSEFrame 通用 SSE data: 行封装。
func SSEFrame(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

var _ = fmt.Sprintf // 保留 fmt 供调试扩展
