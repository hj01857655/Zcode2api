package zcode

import (
	"testing"
)

// AllClaimedToday 投放监视的跳过判据：全池都已领过这批 plan 才允许跳过。
// 任一账号缺任一 plan 都必须重跑（台账防重由 ClaimBestForToken 内部兜底）。
func TestAllClaimedToday(t *testing.T) {
	old := claimLedgerDir
	// Windows 下 t.TempDir() 每次调用返回新目录，闭包外取一次（与 claim_test.go 同坑）
	d := t.TempDir()
	claimLedgerDir = func() string { return d }
	defer func() { claimLedgerDir = old }()

	jwts := []string{"jwt-1", "jwt-2"}
	if AllClaimedToday(jwts, []string{"plan-a"}) {
		t.Fatal("无人领取时不应跳过")
	}
	markClaimed("jwt-1", "plan-a")
	if AllClaimedToday(jwts, []string{"plan-a"}) {
		t.Fatal("仅一账号领取时不应跳过")
	}
	markClaimed("jwt-2", "plan-a")
	if !AllClaimedToday(jwts, []string{"plan-a"}) {
		t.Fatal("全池已领应跳过")
	}
	// 新投放出现：任一未领的 plan 都不跳过
	if AllClaimedToday(jwts, []string{"plan-a", "plan-b"}) {
		t.Fatal("新增 plan 未领时不应跳过")
	}
	// 空入参防御：无账号/无投放都不能判定为「已全领」
	if AllClaimedToday(nil, []string{"plan-a"}) || AllClaimedToday(jwts, nil) {
		t.Fatal("空入参应返回 false")
	}
}
