package zcode

import (
	"encoding/json"
	"strings"
	"testing"
)

// 锁死风控唯一键：system 必须以白名单前缀（seg0+seg1）打头，用户段追加在后。
func TestBuildBody_SystemPrefixFirst(t *testing.T) {
	prefix := []string{"You are ZCode, an interactive coding agent", "主提示词seg1"}
	body, err := BuildAnthropicBody(&ChatRequest{
		Model: "glm-5.3-flash",
		Messages: []ChatMessage{
			{Role: "system", Content: json.RawMessage(`"自定义人设"`)},
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	}, prefix)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	sys, ok := out["system"].([]any)
	if !ok || len(sys) != 3 {
		t.Fatalf("system 应为前缀2段+用户追加段，got %v", out["system"])
	}
	for i := range prefix {
		blk := sys[i].(map[string]any)
		if blk["text"] != prefix[i] {
			t.Fatalf("system[%d] 前缀被改动: %v", i, blk)
		}
		if _, has := blk["cache_control"]; has {
			t.Fatalf("system[%d] 不应带 cache_control（E5 实锤可去）", i)
		}
	}
	if sys[len(prefix)].(map[string]any)["text"] != "自定义人设" {
		t.Fatalf("用户 system 应追加在前缀之后: %v", sys[len(prefix)])
	}
}

// thinking 默认 disabled：必须带哨兵 tools + tool_choice none（F3 实锤）。
// 默认不发送任何思考字段（上游默认无思考）；旧方案 thinking.disabled + 哨兵 tools
// + tool_choice none 实测导致上游返回空消息（usage 全 0）。
func TestBuildBody_NoThinkingByDefault(t *testing.T) {
	body, _ := BuildAnthropicBody(&ChatRequest{
		Model:    "glm-5.3-flash",
		Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}, []string{"seg0", "seg1"})
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if _, has := out["thinking"]; has {
		t.Fatalf("默认不应发送 thinking, got %v", out["thinking"])
	}
	if _, has := out["tools"]; has {
		t.Fatalf("默认不应发送哨兵 tools, got %v", out["tools"])
	}
	if _, has := out["tool_choice"]; has {
		t.Fatalf("默认不应发送 tool_choice, got %v", out["tool_choice"])
	}
	if _, has := out["output_config"]; has {
		t.Fatalf("默认不应发送 output_config, got %v", out["output_config"])
	}
}

// 显式档位走官方 output_config.effort（low/high/max）；enabled 映射为 high。
func TestBuildBody_OutputConfigEffort(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"low", "low"}, {"high", "high"}, {"max", "max"}, {"enabled", "high"},
	} {
		body, _ := BuildAnthropicBody(&ChatRequest{
			Model:    "glm-5.3-flash",
			Thinking: c.in,
			Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		}, []string{"seg0", "seg1"})
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		oc, ok := out["output_config"].(map[string]any)
		if !ok || oc["effort"] != c.want {
			t.Fatalf("thinking=%s 应映射 output_config.effort=%s, got %v", c.in, c.want, out["output_config"])
		}
		if _, has := out["thinking"]; has {
			t.Fatalf("不应再发送 thinking 字段, got %v", out["thinking"])
		}
	}
}

// 带 tools 的请求自动切 enabled + budget 夹取；且思考模式不带 temperature/top_p。
// thinking=enabled 走官方 effort=high；thinking_budget 不再参与（上游按 effort 档位计）。
func TestBuildBody_ThinkingEnabledMapsToEffort(t *testing.T) {
	req := &ChatRequest{
		Model:          "glm-5.3-flash",
		MaxTokens:      3000,
		Thinking:       "enabled",
		ThinkingBudget: 99999,
		Temperature:    ptr(0.7),
		Messages:       []ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	body, _ := BuildAnthropicBody(req, []string{"seg0", "seg1"})
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	oc, ok := out["output_config"].(map[string]any)
	if !ok || oc["effort"] != "high" {
		t.Fatalf("enabled 应映射 effort=high, got %v", out["output_config"])
	}
	if out["max_tokens"].(float64) != 3000 {
		t.Fatalf("max_tokens 应为 3000, got %v", out["max_tokens"])
	}
}

func TestBuildBody_MaxTokensAndMetadata(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{
		{0, 4096}, {-5, 4096}, {200000, 128000}, {1234, 1234},
	} {
		body, _ := BuildAnthropicBody(&ChatRequest{
			MaxTokens: int(tc.in),
			Messages:  []ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		}, []string{"seg0", "seg1"})
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		if out["max_tokens"].(float64) != tc.want {
			t.Fatalf("max_tokens(%v)=%v, want %v", tc.in, out["max_tokens"], tc.want)
		}
	}
	body, _ := BuildAnthropicBody(&ChatRequest{
		Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}, []string{"seg0", "seg1"})
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	uid := out["metadata"].(map[string]any)["user_id"].(string)
	var inner map[string]any
	if json.Unmarshal([]byte(uid), &inner) != nil {
		t.Fatalf("user_id 应为 JSON 字符串化的对象, got %q", uid)
	}
	if inner["account_uuid"] != "" || inner["session_id"] != "" {
		t.Fatalf("account_uuid/session_id 应为空串, got %v", inner)
	}
	if dev, ok := inner["device_id"].(string); !ok || len(dev) != 36 {
		t.Fatalf("device_id 应为 36 位 uuid, got %v", inner["device_id"])
	}
}

// 消息形状：tool → tool_result；assistant tool_calls → tool_use；image data URI → base64 块。
func TestBuildBody_MessageShapes(t *testing.T) {
	body, _ := BuildAnthropicBody(&ChatRequest{
		Messages: []ChatMessage{
			{Role: "system", Content: json.RawMessage(`"你是翻译"`)},
			{Role: "user", Content: json.RawMessage(`"看图"`)},
			{Role: "assistant", Content: json.RawMessage(`""`),
				ToolCalls: []OpenAIToolCall{{ID: "call_1", Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{"read", `{"path":"a.txt"}`}}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"file content"`)},
		},
	}, []string{"seg0", "seg1"})
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	msgs := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("system 提升后应余 3 条消息, got %d", len(msgs))
	}
	asst := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if asst["type"] != "tool_use" || asst["name"] != "read" {
		t.Fatalf("assistant tool_calls 应转 tool_use, got %v", asst)
	}
	tr := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" {
		t.Fatalf("tool 应转 tool_result, got %v", tr)
	}
}

// MapModel：全部别名收敛到 GLM-5.3-Flash 单出口。
func TestMapModel(t *testing.T) {
	// 表内命中用映射值：GLM-5.3 / GLM-4.7 各自直通（实测上游 200 且返回内容）。
	for in, want := range map[string]string{
		"glm-5.3-flash": "GLM-5.3-Flash",
		"glm-5.3":       "GLM-5.3",
		"glm-4.7":       "GLM-4.7",
		"GLM-5.3-Flash": "GLM-5.3-Flash",
	} {
		if got := MapModel(in); got != want {
			t.Fatalf("MapModel(%q)=%q, want %q", in, got, want)
		}
	}
	// 表外一律回落 GLM-5.3-Flash
	for _, in := range []string{"gpt-4o", "", "unknown"} {
		if got := MapModel(in); got != "GLM-5.3-Flash" {
			t.Fatalf("MapModel(%q)=%q, want GLM-5.3-Flash", in, got)
		}
	}
}

// SSE 翻译：message_start 取 input usage；thinking→reasoning_content；tool_use 提示；stop 映射。
func TestTranslateSSELine(t *testing.T) {
	d := TranslateSSELine(`{"type":"message_start","message":{"usage":{"input_tokens":7}}}`)
	if d == nil || d.Role != "assistant" || d.UsageIn == nil || *d.UsageIn != 7 {
		t.Fatalf("message_start 解析错误: %+v", d)
	}
	d = TranslateSSELine(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"推理中"}}`)
	if d == nil || d.Reason != "推理中" {
		t.Fatalf("thinking_delta 应进 Reason: %+v", d)
	}
	// tool_use 块：记 id/name 供组装 tool_calls（旧版发 "[tool_use:read] " 文本占位，已废弃）
	d = TranslateSSELine(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_abc","name":"read"}}`)
	if d == nil || d.ToolName != "read" || d.ToolID != "toolu_abc" || d.BlockIndex != 1 {
		t.Fatalf("tool_use start 应记 id/name/index: %+v", d)
	}
	if d.Content != "" {
		t.Fatalf("tool_use start 不应产出正文文本: %q", d.Content)
	}
	// 工具参数增量
	d = TranslateSSELine(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a\"}"}}`)
	if d == nil || d.ToolArgs != `{"path":"a"}` || d.BlockIndex != 1 {
		t.Fatalf("input_json_delta 应记参数增量: %+v", d)
	}
	d = TranslateSSELine(`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":42}}`)
	if d == nil || d.Finish != "length" || d.UsageOut == nil || *d.UsageOut != 42 {
		t.Fatalf("message_delta 解析错误: %+v", d)
	}
	d = TranslateSSELine(`{"type":"error","error":{"message":"boom"}}`)
	if d == nil || d.ErrText != "boom" {
		t.Fatalf("error 事件应透传: %+v", d)
	}
	if TranslateSSELine(`{"type":"ping"}`) != nil {
		t.Fatal("ping 应忽略")
	}
	if TranslateSSELine("not json").ErrText == "" {
		t.Fatal("非 JSON 负载应原样透传")
	}
}

func TestMapStop(t *testing.T) {
	cases := map[string]string{
		"end_turn": "stop", "stop_sequence": "stop",
		"max_tokens": "length", "tool_use": "tool_calls",
		"": "stop", "unknown": "stop",
	}
	for in, want := range cases {
		if got := MapStop(in); got != want {
			t.Fatalf("MapStop(%q)=%q, want %q", in, got, want)
		}
	}
}

// prefix.json 资产完整性：至少 2 段、seg0 为官方身份句。
func TestSystemPrefixAsset(t *testing.T) {
	prefix := SystemPrefix()
	if len(prefix) < 2 {
		t.Fatalf("system 前缀应至少 2 段（seg0+seg1）, got %d", len(prefix))
	}
	if !strings.HasPrefix(prefix[0], "You are ZCode") {
		t.Fatalf("seg0 应为官方身份句, got %q", prefix[0][:min(40, len(prefix[0]))])
	}
	if len(prefix[1]) < 2000 {
		t.Fatalf("seg1 主提示词应 ≈2313c, got %d", len(prefix[1]))
	}
}

func ptr(f float64) *float64 { return &f }
