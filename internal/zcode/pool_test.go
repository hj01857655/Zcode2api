package zcode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadPoolLegacyFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token.json")
	os.WriteFile(p, []byte(`{"provider":"zai","token":"tok-old","saved_at":"2026-10-26T00:00:00Z"}`), 0600)
	pool := LoadPool(p)
	if pool.Size() != 1 {
		t.Fatalf("旧格式应升级为 1 个账号, got %d", pool.Size())
	}
	a, ok := pool.Pick()
	if !ok || a.Token != "tok-old" || a.Provider != "zai" {
		t.Fatalf("旧格式解析错误: %+v ok=%v", a, ok)
	}
}

func TestPoolUpsertAndRemove(t *testing.T) {
	pool := LoadPool(filepath.Join(t.TempDir(), "none.json")) // 不存在 → 空池
	pool.Upsert(Account{Label: "a", Provider: "zai", Token: "t1"})
	pool.Upsert(Account{Label: "b", Provider: "bigmodel", Token: "t2"})
	if pool.Size() != 2 {
		t.Fatalf("应有 2 个账号, got %d", pool.Size())
	}
	pool.Upsert(Account{Label: "a", Provider: "zai", Token: "t1-new"}) // 同 label 覆盖
	if pool.Size() != 2 {
		t.Fatalf("Upsert 不应增加账号, got %d", pool.Size())
	}
	if !pool.Remove("a") {
		t.Fatal("Remove 失败")
	}
	if pool.Size() != 1 {
		t.Fatalf("Remove 后应为 1, got %d", pool.Size())
	}
}

func TestPoolPickRotates(t *testing.T) {
	pool := LoadPool(filepath.Join(t.TempDir(), "none.json"))
	pool.Upsert(Account{Label: "a", Token: "t1"})
	pool.Upsert(Account{Label: "b", Token: "t2"})
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		a, ok := pool.Pick()
		if !ok {
			t.Fatal("Pick 失败")
		}
		seen[a.Label] = true
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("Pick 应轮转两个账号, got %v", seen)
	}
}

func TestPoolCooldownSkips(t *testing.T) {
	pool := LoadPool(filepath.Join(t.TempDir(), "none.json"))
	pool.Upsert(Account{Label: "a", Token: "t1"})
	pool.Upsert(Account{Label: "b", Token: "t2"})
	pool.Cooldown("a", time.Minute)
	for i := 0; i < 4; i++ {
		a, _ := pool.Pick()
		if a.Label == "a" {
			t.Fatalf("冷却中的账号不应被选中")
		}
	}
	if d := pool.CooldownRemaining("a"); d <= 0 {
		t.Fatal("冷却剩余应为正")
	}
	pool.Cooldown("b", time.Minute)
	if _, ok := pool.Pick(); !ok {
		t.Fatal("全冷却时也应返回一个（最早解冻）")
	}
}

func TestPoolSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token.json")
	pool := LoadPool(p)
	pool.Upsert(Account{Label: "main", Provider: "zai", Token: "tok-1", SavedAt: "2026-10-26T00:00:00Z"})
	if err := pool.Save(p); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() == 0 {
		t.Fatal("落盘文件为空")
	}
	back := LoadPool(p)
	if back.Size() != 1 {
		t.Fatalf("回读账号数错: %d", back.Size())
	}
	a, _ := back.Pick()
	if a.Token != "tok-1" || a.Label != "main" {
		t.Fatalf("回读内容错: %+v", a)
	}
	// 文件形状确认
	var pf poolFile
	raw, _ := os.ReadFile(p)
	if json.Unmarshal(raw, &pf) != nil || len(pf.Accounts) != 1 {
		t.Fatalf("落盘形状错: %s", string(raw))
	}
}

func TestPoolSnapshotHidesToken(t *testing.T) {
	pool := LoadPool(filepath.Join(t.TempDir(), "none.json"))
	pool.Upsert(Account{Label: "a", Provider: "zai", Token: "secret-token-value"})
	snap := pool.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("快照条数错: %d", len(snap))
	}
	b, _ := json.Marshal(snap)
	if string(b) != "" && json.Valid(b) {
		if contains(string(b), "secret-token-value") {
			t.Fatalf("快照不得包含 token 内容: %s", b)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
