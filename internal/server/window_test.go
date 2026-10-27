package server

import (
	"testing"
	"time"

	"zcode2api/internal/zcode"
)

// 活动时段必须按北京时间算，与部署机时区无关（Docker 默认 UTC）。
// 北京 23:30 = UTC 15:30 → 窗口内；北京 10:00 = UTC 02:00 → 窗口外。
func TestUsageWindowUsesBeijingTime(t *testing.T) {
	utc := time.UTC

	in := time.Date(2026, 10, 27, 15, 30, 0, 0, utc) // 北京 23:30
	got := usageWindow(in)
	if got["active"] != true {
		t.Fatalf("北京 23:30 应在使用窗口内，得到 %+v", got)
	}

	out := time.Date(2026, 10, 27, 2, 0, 0, 0, utc) // 北京 10:00
	got = usageWindow(out)
	if got["active"] != false {
		t.Fatalf("北京 10:00 应在使用窗口外，得到 %+v", got)
	}
}

// 领取时刻按北京时间 19:05 起算：UTC 11:00 时下一个 slot 是北京 19:05。
func TestNextClaimTimeUsesBeijingTime(t *testing.T) {
	utc := time.UTC
	now := time.Date(2026, 10, 27, 11, 0, 0, 0, utc) // 北京 19:00
	next := nextClaimTime(now)
	beijing := next.In(zcode.CST8)
	if beijing.Hour() != 19 || beijing.Minute() != 5 {
		t.Fatalf("下一个领取时刻应为北京 19:05，得到 %s", beijing.Format("2006-01-02 15:04"))
	}
}
