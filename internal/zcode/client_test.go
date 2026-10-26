package zcode

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".zcode", "v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, ".zcode", "v2", "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)
}

func TestLoadTokenPicksBrandProvider(t *testing.T) {
	// zai 品牌有值、bigmodel 空
	writeCfg(t, `{"provider":{"builtin:bigmodel-start-plan":{"options":{"apiKey":""}},"builtin:zai-start-plan":{"options":{"apiKey":"tok-zai"}}}}`)
	if got := LoadToken(""); got != "tok-zai" {
		t.Fatalf("应取 zai 品牌 token, got %q", got)
	}

	// 两者都有值：zai 优先
	writeCfg(t, `{"provider":{"builtin:bigmodel-start-plan":{"options":{"apiKey":"tok-bm"}},"builtin:zai-start-plan":{"options":{"apiKey":"tok-zai"}}}}`)
	if got := LoadToken(""); got != "tok-zai" {
		t.Fatalf("应优先 zai, got %q", got)
	}

	// 只有 bigmodel 有值
	writeCfg(t, `{"provider":{"builtin:zai-start-plan":{"options":{"apiKey":""}},"builtin:bigmodel-start-plan":{"options":{"apiKey":"tok-bm"}}}}`)
	if got := LoadToken(""); got != "tok-bm" {
		t.Fatalf("应取 bigmodel token, got %q", got)
	}

	// 任意自定义 start-plan provider 兜底
	writeCfg(t, `{"provider":{"builtin:bigmodel-start-plan":{"options":{"apiKey":""}},"builtin:custom-start-plan":{"options":{"apiKey":"tok-custom"}}}}`)
	if got := LoadToken(""); got != "tok-custom" {
		t.Fatalf("应兜底取自定义 start-plan token, got %q", got)
	}

	// 显式传入优先
	if got := LoadToken("explicit"); got != "explicit" {
		t.Fatalf("显式 token 应优先, got %q", got)
	}
}
