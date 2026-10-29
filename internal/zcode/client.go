// Package zcode 实现 start-plan 上游客户端。
// 协议结论（2026-10-26 消融实测，见 PROGRESS.md 终局战报）：
//   - 风控唯一键 = system 前缀白名单（seg0+seg1，前缀匹配，可微调可追加）；
//     头/UA/device_id/metadata/cache_control/TLS 均不校验（E4/F4/F6 实锤）。
//   - 因此头组保持极简 5 头（对齐终版 server.py），不画蛇添足。
package zcode

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// systemPrefixConfig assets/system_prefix.json 的形状（seg0+seg1 必须作为 system 前缀）。
type systemPrefixConfig struct {
	ExtractedAt  string   `json:"extracted_at"`
	ModelDefault string   `json:"model_default"`
	SystemPrefix []string `json:"system_prefix"`
	Seg2Len      int      `json:"seg2_len"`
	Note         string   `json:"note"`
}

//go:embed assets/prefix.json
var prefixRaw []byte

// SystemPrefix 白名单前缀文本（seg0+seg1）。ZCode 大版本更新后需重新提取。
func SystemPrefix() []string {
	var cfg systemPrefixConfig
	if err := json.Unmarshal(prefixRaw, &cfg); err != nil {
		// assets 与源码同编译，损坏属构建错误，直接 panic 暴露
		panic("system_prefix.json 损坏: " + err.Error())
	}
	return cfg.SystemPrefix
}

// UUIDv4 生成 RFC 4122 v4 随机 UUID（避免为每请求引第三方依赖）。
func UUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Client 单上游客户端；Token 允许外部热替换（两级交换场景）。
type Client struct {
	Base  string // 完整 messages 端点
	Token string // 单账号模式 token（Pool 为 nil 时用）
	Pool  *Pool  // 非 nil 时按账号池选号，失败账号冷却后换下一个
	HTTP  *http.Client
	// Dump 可选抓包回调：direction 为 "req"/"resp"，payload 为原始字节。
	Dump func(direction, label string, status int, payload []byte)
}

// New 装配默认客户端（timeout 300s 对齐 server.py 的 httpx.Timeout(300, connect=15)）。
func New(base, token string) *Client {
	return &Client{
		Base:  base,
		Token: token,
		HTTP:  &http.Client{Timeout: 300 * time.Second},
	}
}

// Headers 极简头组（终版 server.py 形状）。风控不校验头， UA 的
// " ai-sdk/anthropic/3.0.81" 后缀保留是成本最低的伪装惯性，非必需。
func (c *Client) Headers(token string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("anthropic-version", "2023-06-01")
	h.Set("User-Agent", "ZCode/3.16.3 ai-sdk/anthropic/3.0.81")
	return h
}

// UpstreamError 上游非 200；Status 为状态码，Body 为截断后的原文。
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream %d: %s", e.Status, e.Body)
}

// Messages 发送 anthropic 协议 body（已序列化的 JSON），返回上游 SSE 响应。
// 语义对齐终版 server.py：无论下游请求流式与否，上游一律 stream=true，
// 非流式由 handler 聚合——绕开 stream:false 的未知行为。
// Messages 发送 anthropic 协议 body，返回上游响应。
// Pool 非 nil 时按池选号；遇到 401/429/3012 自动冷却该账号并换下一个重试（最多一轮）。
func (c *Client) Messages(body []byte) (*http.Response, error) {
	if c.Pool == nil || c.Pool.Size() == 0 {
		return c.send(c.Token, body)
	}
	tries := c.Pool.Size()
	if tries < 1 {
		tries = 1
	}
	var lastErr error
	for i := 0; i < tries; i++ {
		acc, ok := c.Pool.Pick()
		if !ok {
			break
		}
		resp, err := c.send(acc.Token, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ue, ok := err.(*UpstreamError); ok {
			d := cooldownFor(ue)
			if d > 0 {
				c.Pool.Cooldown(acc.Label, d)
				continue // 换下一个账号
			}
		}
		return nil, err // 非可重试错误（400/405 等）直接返回
	}
	if lastErr == nil {
		lastErr = &UpstreamError{Status: 503, Body: "no available account"}
	}
	return nil, lastErr
}

// cooldownFor 按上游错误决定冷却时长；0 = 不冷却（不换号）。
func cooldownFor(ue *UpstreamError) time.Duration {
	switch {
	case ue.Status == http.StatusUnauthorized:
		return 30 * time.Minute // token 失效，长期冷却
	case ue.Status == http.StatusTooManyRequests:
		return 60 * time.Second // 限流/额度
	case ue.Status == http.StatusMethodNotAllowed || strings.Contains(ue.Body, "3012"):
		return 10 * time.Minute // 风控
	case strings.Contains(ue.Body, "3008") || strings.Contains(ue.Body, "3009") || strings.Contains(ue.Body, "3010"):
		return 60 * time.Second // 并发上限
	}
	return 0
}

// send 用指定 token 发一次请求。
func (c *Client) send(token string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, c.Base, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = c.Headers(token)
	if c.Dump != nil {
		c.Dump("req", UUIDv4(), 0, body)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if c.Dump != nil {
			c.Dump("resp", "err", resp.StatusCode, raw)
		}
		return nil, &UpstreamError{Status: resp.StatusCode, Body: string(raw)}
	}
	return resp, nil
}

// LoadToken token 解析顺序（池为空时的单账号链）：config token_source > env
// ZCODE_TOKEN > token.json 旧格式单对象 > ~/.zcode/v2/config.json 的 start-plan
// provider apiKey（明文存放的 zcodejwttoken 本体）。
// 不走 credentials.json 的 AES-256-GCM 层——同一 token 在 config.json 里有明文副本，
// 纯 stdlib 即可完成，等价于 zk.token() 的取值结果。
func LoadToken(explicit string) string {
	if explicit != "" {
		return explicit
	}
	// 本地 token.json 优先（login 写入，不依赖也不改写 ~/.zcode 的 IDE 凭证）
	if tok := LoadTokenLocal(LocalTokenPath()); tok != "" {
		return tok
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".zcode", "v2", "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Provider map[string]struct {
			Options struct {
				APIKey string `json:"apiKey"`
			} `json:"options"`
		} `json:"provider"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	// start-plan 的 provider id 带品牌前缀（builtin:zai-start-plan / builtin:bigmodel-start-plan），
	// OAuth 登录哪个品牌就把 token 写进哪个 provider，不能写死单一 id。
	pick := func(ids ...string) string {
		for _, id := range ids {
			if p, ok := cfg.Provider[id]; ok && p.Options.APIKey != "" {
				return p.Options.APIKey
			}
		}
		return ""
	}
	if tok := pick("builtin:zai-start-plan", "builtin:bigmodel-start-plan"); tok != "" {
		return tok
	}
	var ids []string
	for id, p := range cfg.Provider {
		if strings.HasSuffix(id, "-start-plan") && p.Options.APIKey != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		return cfg.Provider[ids[0]].Options.APIKey
	}
	return ""
}
