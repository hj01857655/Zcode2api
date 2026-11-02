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

// 全局退避：SetGlobalBackoff 后 Pick fail-fast，剩余时长递减，过期自动解除
func TestPoolGlobalBackoff(t *testing.T) {
	pool := LoadPool(filepath.Join(t.TempDir(), "none.json"))
	pool.Upsert(Account{Label: "a", Token: "t1"})
	pool.SetGlobalBackoff(time.Minute)
	if _, ok := pool.Pick(); ok {
		t.Fatal("全局退避期内 Pick 应 fail-fast")
	}
	if pool.GlobalBackoffRemaining() <= 0 {
		t.Fatal("退避剩余应为正")
	}
	// 过期解除：手动把 globalUntil 拨到过去
	pool.mu.Lock()
	pool.globalUntil = time.Now().Add(-time.Second)
	pool.mu.Unlock()
	if _, ok := pool.Pick(); !ok {
		t.Fatal("退避过期后 Pick 应恢复")
	}
	if pool.GlobalBackoffRemaining() != 0 {
		t.Fatal("过期后退避剩余应为 0")
	}
	// SetGlobalBackoff(0/负) 不生效
	pool.SetGlobalBackoff(0)
	if pool.GlobalBackoffRemaining() < 0 {
		t.Fatal("SetGlobalBackoff(0) 不应产生负退避")
	}
}

// 熔断双闸：连败 5 次触发长冷却（Settle 返回 true）+ 成功重置；成功率窗口 80% 触发
func TestPoolSettleTripBreaker(t *testing.T) {
	pool := LoadPool(filepath.Join(t.TempDir(), "none.json"))
	pool.Upsert(Account{Label: "a", Token: "t1"})
	// 连败闸：5 连败第 5 次返回 true（触发熔断）
	tripped := false
	for i := 1; i <= tripFailStreak; i++ {
		if pool.Settle("a", false) {
			tripped = true
			if i != tripFailStreak {
				t.Fatalf("连败 %d 次不应提前触发", i)
			}
		}
	}
	if !tripped {
		t.Fatal("5 连败应触发熔断")
	}
	if d := pool.CooldownRemaining("a"); d <= 0 {
		t.Fatal("熔断后应有长冷却")
	}
	// 成功重置连败：恢复后 1 次成功清 streak，后续 4 次失败不再触发
	pool.mu.Lock()
	delete(pool.cool, "a") // 解除冷却便于后续断言
	pool.mu.Unlock()
	pool.Settle("a", true)
	for i := 0; i < tripFailStreak-1; i++ {
		if pool.Settle("a", false) {
			t.Fatalf("成功重置后 %d 连败不应触发", i+1)
		}
	}
}

// 成功率熔断：窗口 10 次内错误率 ≥80% 触发（与连败闸独立路径）。
// 序列约束：8 败 2 成、最大连败 3（<5 不触发连败闸），第 10 次时窗口恰为 80%。
func TestPoolSettleRateTrip(t *testing.T) {
	pool := LoadPool(filepath.Join(t.TempDir(), "none.json"))
	pool.Upsert(Account{Label: "a", Token: "t1"})
	seq := []bool{false, false, true, false, false, false, true, false, false, false}
	for i, ok := range seq {
		if pool.Settle("a", ok) {
			if i != len(seq)-1 {
				t.Fatalf("第 %d 次不应提前触发", i+1)
			}
			return
		}
	}
	t.Fatal("窗口错误率 80% 应触发熔断")
}

// sticky 会话亲和：同 key 固定同号；绑定号冷却后重绑；空 key 退化轮转；全局退避 fail-fast
func TestPoolPickSticky(t *testing.T) {
	pool := LoadPool(filepath.Join(t.TempDir(), "none.json"))
	pool.Upsert(Account{Label: "a", Token: "t1"})
	pool.Upsert(Account{Label: "b", Token: "t2"})
	// 同 key 固定同号
	a1, _ := pool.PickSticky("sess-1")
	a2, _ := pool.PickSticky("sess-1")
	if a1.Label != a2.Label {
		t.Fatalf("同 key 应固定同号: %s vs %s", a1.Label, a2.Label)
	}
	// 不同 key 各自绑定（两 key 可能撞同号，但绑定表应记录两条）
	pool.PickSticky("sess-2")
	// 绑定号冷却 → 重绑到另一个号
	pool.Cooldown(a1.Label, time.Minute)
	a3, ok := pool.PickSticky("sess-1")
	if !ok || a3.Label == a1.Label {
		t.Fatalf("绑定号冷却后应重绑到可用号, got %s", a3.Label)
	}
	// 空 key 退化普通轮转（不 panic、有号）
	if _, ok := pool.PickSticky(""); !ok {
		t.Fatal("空 key 应退化轮转")
	}
	// 全局退避：fail-fast
	pool.SetGlobalBackoff(time.Minute)
	if _, ok := pool.PickSticky("sess-3"); ok {
		t.Fatal("全局退避期内 PickSticky 应 fail-fast")
	}
}
