// 企业微信群机器人 webhook 播报（最简 text 形态）。url 为空 = 未配置，静默跳过。
// 只依赖环境变量给到的 webhook 地址，无 SDK 依赖——SUPER-AGENT.md 第一期
// 「领取结果 IM 播报」的最小落地形态，后续 Router 层复用同一入口。
package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var wecomHTTP = &http.Client{Timeout: 10 * time.Second}

// truncateRunes 按 rune 截断——企业微信 text.content 上限 2048 字节，
// 中文按字节算约 680 字，600 rune 留出余量，截断尾部加省略号。
func truncateRunes(s string, max int) string {
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max-1]) + "…"
}

// NotifyWeCom 向企业微信群机器人 webhook 推一条 text 消息。
// errcode!=0 视为失败（企业微信语义：HTTP 200 也可能回业务错误）。
func NotifyWeCom(url string, text string) error {
	if url == "" {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"msgtype": "text",
		"text":    map[string]any{"content": truncateRunes(text, 600)},
	})
	if err != nil {
		return err
	}
	resp, err := wecomHTTP.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var payload struct {
		Errcode int    `json:"errcode"`
		Errmsg  string `json:"errmsg"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return fmt.Errorf("wecom 非 JSON 响应: HTTP %d, %.80s", resp.StatusCode, string(data))
	}
	if payload.Errcode != 0 {
		return fmt.Errorf("wecom errcode=%d: %s", payload.Errcode, payload.Errmsg)
	}
	return nil
}
