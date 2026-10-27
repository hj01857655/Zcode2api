// OAuth device flow 登录与 token 写回。
// 协议对齐 dengyie app/oauth.py（官方 zcode.cjs createZaiCliOAuthClient）：
//   - init:  POST {base}/oauth/cli/init，仅 Authorization: Bearer <pollToken> + Content-Type，
//     body {"provider": d.Provider}；响应 data.poll_token 由服务端下发时优先采用；
//   - poll:  GET {base}/oauth/cli/poll/{flow_id}，2s 轮询；ready 时 data.token 即 zcodejwttoken
//     （无需两级交换——start-plan 认的就是它；data.zai.access_token 是兑换 project token 的
//     oauth 凭证，网关用不到，不取）；
//   - 写回：config.json 的 builtin:bigmodel-start-plan apiKey（网关下次启动读它）+
//     credentials.json 的 zcodejwttoken 键（enc:v1 AES-256-GCM，CLI/GUI 登录态同步恢复）。
package zcode

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// OAuthAPIBase ZCode 官方 OAuth 端点基址。
const OAuthAPIBase = "https://zcode.z.ai/api/v1"

// zcodeConfigDir 返回 ~/.zcode/v2。
func zcodeConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".zcode", "v2"), nil
}

// credentialsKeyFor credentials.json enc:v1 的 AES-256-GCM key：
// SHA-256("zcode-credential-fallback:{platform}:{home}:{user}")。
// platform 对齐 Python sys.platform（Windows 为 win32）；user 对齐 getpass.getuser()。
func credentialsKeyFor(platform, home, user string) []byte {
	h := sha256.Sum256([]byte("zcode-credential-fallback:" + platform + ":" + home + ":" + user))
	return h[:]
}

func credentialsKey() ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32" // sys.platform 语义
	}
	user := os.Getenv("USERNAME")
	if user == "" {
		user = os.Getenv("USER")
	}
	return credentialsKeyFor(platform, home, user), nil
}

// sealCredential 按 enc:v1:{b64url(iv)}.{b64url(tag)}.{b64url(ct)} 加密（tag 为 GCM 末 16 字节）。
func sealCredential(key, plaintext []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	out := gcm.Seal(nil, iv, plaintext, nil) // ct||tag
	tag, ct := out[len(out)-16:], out[:len(out)-16]
	b64u := base64.RawURLEncoding
	return "enc:v1:" + b64u.EncodeToString(iv) + "." + b64u.EncodeToString(tag) + "." +
		b64u.EncodeToString(ct), nil
}

// OpenBrowser 用系统默认浏览器打开授权页（Windows 走 rundll32，不弹 cmd 窗口）。
func OpenBrowser(url string) error {
	if runtime.GOOS == "windows" {
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
	return exec.Command("xdg-open", url).Start()
}

// DeviceFlow 一次 OAuth 登录会话。
type DeviceFlow struct {
	Base     string // 默认 OAuthAPIBase
	HTTP     *http.Client
	PollWait time.Duration // 轮询间隔，默认 2s
	MaxTries int           // 轮询次数上限，默认 150（≈5 分钟）
	Out      io.Writer     // 进度提示输出（nil = 丢弃）
	Provider string        // "zai" | "bigmodel"，默认 zai
	// OpenBrowserFn 打开授权页的函数；nil = 系统默认浏览器。测试注入 no-op。
	OpenBrowserFn func(url string) error
}

func (d *DeviceFlow) defaults() {
	if d.Base == "" {
		d.Base = OAuthAPIBase
	}
	if d.HTTP == nil {
		d.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if d.PollWait == 0 {
		d.PollWait = 2 * time.Second
	}
	if d.MaxTries == 0 {
		d.MaxTries = 150
	}
	if d.Provider == "" {
		d.Provider = "zai"
	}
}

func (d *DeviceFlow) printf(format string, args ...any) {
	if d.Out != nil {
		fmt.Fprintf(d.Out, format+"\n", args...)
	}
}

// Login 执行完整 device flow，返回新的 zcodejwttoken。需要用户在浏览器完成授权。
func (d *DeviceFlow) Login() (string, error) {
	d.defaults()
	pollToken := make([]byte, 32)
	if _, err := rand.Read(pollToken); err != nil {
		return "", err
	}
	selfToken := hex.EncodeToString(pollToken)

	// init：仅 Bearer + Content-Type 两个头（官方 CLI 规范，不带伪装头避免会话绑定）
	body, _ := json.Marshal(map[string]any{"provider": d.Provider})
	req, err := http.NewRequest(http.MethodPost, d.Base+"/oauth/cli/init", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+selfToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := d.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth init: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth init http %d: %s", res.StatusCode, truncateStr(string(raw), 300))
	}
	var initRes struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			FlowID       string `json:"flow_id"`
			AuthorizeURL string `json:"authorize_url"`
			PollToken    string `json:"poll_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &initRes); err != nil {
		return "", fmt.Errorf("oauth init 响应解析失败: %w", err)
	}
	if initRes.Code != 0 {
		return "", fmt.Errorf("oauth init 被拒绝（code=%d %s）", initRes.Code, initRes.Msg)
	}
	if initRes.Data.FlowID == "" || initRes.Data.AuthorizeURL == "" {
		return "", fmt.Errorf("oauth init 返回数据不完整")
	}
	// 2026-09 起官方协议：poll 必须用服务端下发的 poll_token
	token := selfToken
	if t := initRes.Data.PollToken; t != "" {
		token = t
	}

	d.printf("请在浏览器中打开下面链接完成授权（5 分钟内）：")
	d.printf("%s", initRes.Data.AuthorizeURL)
	open := d.OpenBrowserFn
	if open == nil {
		open = OpenBrowser
	}
	_ = open(initRes.Data.AuthorizeURL)
	d.printf("等待授权中...")

	for i := 0; i < d.MaxTries; i++ {
		time.Sleep(d.PollWait)
		preq, err := http.NewRequest(http.MethodGet,
			d.Base+"/oauth/cli/poll/"+initRes.Data.FlowID, nil)
		if err != nil {
			return "", err
		}
		preq.Header.Set("Authorization", "Bearer "+token)
		pres, err := d.HTTP.Do(preq)
		if err != nil {
			continue // 网络抖动不中断轮询（对齐 dengyie cli.py）
		}
		praw, _ := io.ReadAll(io.LimitReader(pres.Body, 1<<20))
		pres.Body.Close()
		if pres.StatusCode != http.StatusOK {
			continue
		}
		var pollRes struct {
			Data struct {
				Status  string `json:"status"`
				Message string `json:"message"`
				Token   string `json:"token"`
			} `json:"data"`
		}
		if json.Unmarshal(praw, &pollRes) != nil {
			continue
		}
		switch pollRes.Data.Status {
		case "ready":
			jwt := pollRes.Data.Token
			if jwt == "" {
				return "", fmt.Errorf("oauth poll ready 但未返回 token")
			}
			d.printf("授权成功，已取得新 token（len=%d）", len(jwt))
			return jwt, nil
		case "failed":
			return "", fmt.Errorf("授权失败或被拒绝: %s", pollRes.Data.Message)
		}
	}
	return "", fmt.Errorf("登录超时（等待 %s）", time.Duration(d.MaxTries)*d.PollWait)
}

// WritebackToken 把新 token 写回两处持久层，下次启动自动生效：
//   - configPath 的 provider["builtin:bigmodel-start-plan"].options.apiKey（明文）
//   - credPath 的 zcodejwttoken 键（enc:v1 AES-256-GCM，其它键原样保留）
//
// 任一文件不存在则跳过该处（返回值注明写回了哪些）。token 值不打印。

// WritebackDefaultPaths 用真实 ~/.zcode/v2 路径写回。

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------- 本地 token 存储（不写入 ~/.zcode 的 IDE 凭证） ----------

// LocalToken token.json 的形状。
type LocalToken struct {
	Provider string `json:"provider"`
	Token    string `json:"token"`
	SavedAt  string `json:"saved_at"`
}

// SaveTokenLocal 原子写入本地 token 文件（0600）。
func SaveTokenLocal(path, provider, token string) error {
	b, err := json.MarshalIndent(LocalToken{Provider: provider, Token: token, SavedAt: time.Now().Format(time.RFC3339)}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadTokenLocal 读本地 token 文件；不存在或格式不符返回空串。
func LoadTokenLocal(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var t LocalToken
	if json.Unmarshal(raw, &t) != nil {
		return ""
	}
	return strings.TrimSpace(t.Token)
}

// LocalTokenPath 解析 token.json 路径：优先 exe 同目录，其次工作目录。
func LocalTokenPath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "token.json")
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, "token.json")
	}
	return "token.json"
}
