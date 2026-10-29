package zcode

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
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

// TestDeviceFlowLogin：init 用自造 pollToken、poll 改用服务端下发的 poll_token、
// ready 返回 data.token。Login 只返回 token，落盘在调用方（main.go runLogin）。
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
