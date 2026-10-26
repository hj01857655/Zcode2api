package zcode

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sealCredential round-trip：加密后按存储格式（iv.tag.ct）解回原值。
func TestSealCredentialRoundTrip(t *testing.T) {
	key := credentialsKeyFor("win32", `C:\Users\t`, "t")
	pt := []byte("eyJhbGciOi.204-char-jwt-body")
	enc, err := sealCredential(key, pt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "enc:v1:") {
		t.Fatalf("密文应以 enc:v1: 开头: %q", enc[:10])
	}
	// 按存储格式解回：base64url 无 padding，三段 iv/tag/ct
	parts := strings.Split(enc[7:], ".")
	if len(parts) != 3 {
		t.Fatalf("密文应为 iv.tag.ct 三段: %d", len(parts))
	}
	b64d := base64.RawURLEncoding.DecodeString
	iv, err1 := b64d(parts[0])
	tag, err2 := b64d(parts[1])
	ct, err3 := b64d(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatalf("base64url 解码失败: %v %v %v", err1, err2, err3)
	}
	if len(iv) != 12 || len(tag) != 16 {
		t.Fatalf("iv 应 12B、tag 应 16B: %d %d", len(iv), len(tag))
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	got, err := gcm.Open(nil, iv, append(ct, tag...), nil)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if string(got) != string(pt) {
		t.Fatalf("round-trip 不一致")
	}
}

// WritebackToken：临时目录里写回 config.json apiKey + credentials.json zcodejwttoken，
// 其它键原样保留，token 在落盘文件中只以密文出现。
func TestWritebackToken(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	credPath := filepath.Join(dir, "credentials.json")

	os.WriteFile(cfgPath, []byte(`{"port":3010,"provider":{"builtin:bigmodel-start-plan":{"options":{"apiKey":"old"}},"other":{"options":{"apiKey":"keep"}}}}`), 0600)
	os.WriteFile(credPath, []byte(`{"zcodejwttoken":"enc:v1:OLD","oauth:login_attribution":"enc:v1:KEEP"}`), 0600)

	written, err := WritebackToken(cfgPath, credPath, "zai", "new-token-value")
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 {
		t.Fatalf("应写回两处: %v", written)
	}

	var cfg map[string]any
	raw, _ := os.ReadFile(cfgPath)
	_ = json.Unmarshal(raw, &cfg)
	if cfg["port"].(float64) != 3010 {
		t.Fatal("config.json 其它字段应保留")
	}
	prov := cfg["provider"].(map[string]any)
	// 当前登录品牌是 zai：写入/新建 builtin:zai-start-plan 段
	zai, ok := prov["builtin:zai-start-plan"].(map[string]any)
	if !ok {
		t.Fatalf("应新建 builtin:zai-start-plan 段, got %v", prov)
	}
	if zai["options"].(map[string]any)["apiKey"] != "new-token-value" {
		t.Fatal("zai 段 apiKey 未更新")
	}
	// 其它品牌段保持原值
	if prov["builtin:bigmodel-start-plan"].(map[string]any)["options"].(map[string]any)["apiKey"] != "old" {
		t.Fatal("bigmodel 段不应被改动")
	}
	if prov["other"].(map[string]any)["options"].(map[string]any)["apiKey"] != "keep" {
		t.Fatal("其它 provider 段不应被改动")
	}

	var creds map[string]string
	raw2, _ := os.ReadFile(credPath)
	_ = json.Unmarshal(raw2, &creds)
	if creds["oauth:login_attribution"] != "enc:v1:KEEP" {
		t.Fatal("credentials 其它键应原样保留")
	}
	newTok := creds["zcodejwttoken"]
	if strings.Contains(newTok, "new-token-value") || newTok == "enc:v1:OLD" {
		t.Fatal("zcodejwttoken 应为重新加密的新密文")
	}
	// 用与写回相同的 key 解回，验证密文就是新 token（AES-GCM round-trip）
	parts := strings.Split(newTok[7:], ".")
	b64d := base64.RawURLEncoding.DecodeString
	ivb, _ := b64d(parts[0])
	tagb, _ := b64d(parts[1])
	ctb, _ := b64d(parts[2])
	key, err := credentialsKey()
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	got, err := gcm.Open(nil, ivb, append(ctb, tagb...), nil)
	if err != nil {
		t.Fatalf("写回的密文解不开: %v", err)
	}
	if string(got) != "new-token-value" {
		t.Fatalf("解回值不符: %q", got)
	}
}

// DeviceFlow 全流程（假上游）：init 下发 poll_token → pending → ready 带 token。
func TestDeviceFlowLogin(t *testing.T) {
	var selfToken string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/cli/init":
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				t.Errorf("init 缺 Bearer pollToken")
				w.WriteHeader(400)
				return
			}
			selfToken = strings.TrimPrefix(auth, "Bearer ")
			_, _ = w.Write([]byte(`{"code":0,"data":{"flow_id":"f1","authorize_url":"https://chat.z.ai/api/oauth/authorize?client_id=client_P8X5CMWmlaRO9gyO-KSqtg&redirect_uri=https://zcode.z.ai/api/v1/oauth/cli/callback/zai&state=8645bdd0cd6cf69060cb44b4a63c85ec&response_type=code","poll_token":"server-token"}}`))
		case r.URL.Path == "/oauth/cli/poll/f1":
			if r.Header.Get("Authorization") != "Bearer server-token" {
				t.Errorf("poll 应采用服务端下发的 poll_token")
			}
			if selfToken == "" {
				t.Errorf("poll 先于 init 到达")
			}
			_, _ = w.Write([]byte(`{"data":{"status":"ready","token":"new-jwt-token"}}`))
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer up.Close()

	flow := &DeviceFlow{Base: up.URL, PollWait: time.Millisecond, MaxTries: 3, OpenBrowserFn: func(string) error { return nil }}
	tok, err := flow.Login()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "new-jwt-token" {
		t.Fatalf("应返回 ready 的 token, got %q", tok)
	}
}

// poll pending 若干次后 ready；failed 立即终止。
func TestDeviceFlowPendingThenReady(t *testing.T) {
	n := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/cli/init" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"flow_id":"f1","authorize_url":"https://chat.z.ai/api/oauth/authorize?client_id=client_P8X5CMWmlaRO9gyO-KSqtg&redirect_uri=https://zcode.z.ai/api/v1/oauth/cli/callback/zai&state=8645bdd0cd6cf69060cb44b4a63c85ec&response_type=code"}}`))
			return
		}
		n++
		if n < 3 {
			_, _ = w.Write([]byte(`{"data":{"status":"pending"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"status":"ready","token":"late-token"}}`))
	}))
	defer up.Close()
	flow := &DeviceFlow{Base: up.URL, PollWait: time.Millisecond, MaxTries: 10, OpenBrowserFn: func(string) error { return nil }}
	tok, err := flow.Login()
	if err != nil || tok != "late-token" {
		t.Fatalf("pending→ready 应继续轮询: %v %q", err, tok)
	}

	// failed
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/cli/init" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"flow_id":"f2","authorize_url":"https://chat.z.ai/api/oauth/authorize?client_id=client_P8X5CMWmlaRO9gyO-KSqtg&redirect_uri=https://zcode.z.ai/api/v1/oauth/cli/callback/zai&state=8645bdd0cd6cf69060cb44b4a63c85ec&response_type=code"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"status":"failed","message":"user denied"}}`))
	}))
	defer up2.Close()
	flow2 := &DeviceFlow{Base: up2.URL, PollWait: time.Millisecond, MaxTries: 5, OpenBrowserFn: func(string) error { return nil }}
	if _, err := flow2.Login(); err == nil || !strings.Contains(err.Error(), "user denied") {
		t.Fatalf("failed 应立即报错: %v", err)
	}
}
