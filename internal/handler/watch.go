// 投放监视（SUPER-AGENT.md 第一期 P0：事件驱动抢批）。
// 固定 19:05/20:05/… slot 兜底之外，每 interval 全池逐号轮询 touch 投放；
// 任何账号出现 claim_zcode_plan 投放且台账显示未全领 → 立即有界并发抢批，
// 结果播报企业微信群机器人 webhook。收益：新批次投放瞬间开抢，不再等整点——
// 11-03 批次如果有这套，9 个号都能抢到。
//
// 环境：
//
//	ZCODE_CLAIM_WATCH=0            关闭监视（ZCODE_AUTO_CLAIM 总开关仍生效）
//	ZCODE_WATCH_INTERVAL=300       轮询间隔秒（默认 5min）
//	ZCODE_CLAIM_PARALLEL=4         抢批并发上限（出口 IP 级 429 实测口径）
//	ZCODE_WEIXIN_WORK_WEBHOOK=...  企业微信群机器人 webhook，空 = 只记日志
package handler

import (
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/zcode"
)

const (
	defaultWatchInterval = 5 * time.Minute
	defaultClaimParallel = 4
	// touchTouchStagger 逐号 touch 的错峰间隔：投放名单服务端动态进出，
	// 单 canary 会漏灰度，全池逐号又怕同 IP 连查撞 429——400ms 间隔把
	// 14 号一轮摊到 ~6s，平均 2.8 req/min，远低于查询限流口径。
	touchStagger = 400 * time.Millisecond
	// claimBatchCooldown 抢批完成后的静默期：台账已落、风控标记需观察，
	// 不在 5min 内对同一投放重复触发。
	claimBatchCooldown = 10 * time.Minute
)

// watchBatchRunning 抢批进行中标记：投放信号可能在批次期间持续出现，
// 不允许重入（台账防重靠 ledger，防重入靠这个）。
var watchBatchRunning atomic.Bool

// watchAccounts 监视目标：与 claimTargets 同构但带 JWT。
type watchAccount struct {
	Label string
	JWT   string
}

func watchAccounts(client *zcode.Client) []watchAccount {
	out := []watchAccount{}
	if client.Pool != nil {
		for _, acc := range client.Pool.Accounts {
			if acc.Token != "" {
				out = append(out, watchAccount{acc.Label, acc.Token})
			}
		}
	} else if client.Token != "" {
		out = append(out, watchAccount{"default", client.Token})
	}
	return out
}

// detectDelivery 全池逐号 touch，返回本轮看到的 plan_ids 并集（排序保证
// 日志与台账判定的确定性）。单号失败只记日志——投放名单动态进出，
// 单号 429/风控不代表平台无投放。
func detectDelivery(client *zcode.Client, log *slog.Logger) []string {
	if log == nil {
		log = slog.Default()
	}
	accounts := watchAccounts(client)
	if len(accounts) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for i, a := range accounts {
		if i > 0 {
			time.Sleep(touchStagger)
		}
		mid := zcode.StableDeviceMid(claimStateDir(), a.JWT)
		dels, err := zcode.TouchPlans(claimHTTP, a.JWT, mid)
		if err != nil {
			log.Warn("touch 轮询失败", slog.String("account", a.Label), slog.String("err", err.Error()))
			continue
		}
		for _, d := range dels {
			for _, pid := range d.PlanIDs {
				seen[pid] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for pid := range seen {
		out = append(out, pid)
	}
	sort.Strings(out)
	return out
}

// buildClaimSummary 抢批结果播报文案（wecom text 单条，600 rune 内）。
func buildClaimSummary(planIDs []string, results []zcode.ClaimResult) string {
	var b strings.Builder
	b.WriteString("【抢批】投放 plan_ids=" + strings.Join(planIDs, ",") + "\n")
	for _, r := range results {
		mark := "•"
		if !r.OK {
			mark = "✖"
		}
		b.WriteString(mark + " " + r.Account + ": " + r.Message + "\n")
	}
	return b.String()
}

// WatchDeliveryLoop 投放监视主循环（常驻 goroutine，启动即探测一轮）。
func WatchDeliveryLoop(cfg *config.Config, client *zcode.Client, log *slog.Logger) {
	if os.Getenv("ZCODE_CLAIM_WATCH") == "0" {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	interval := defaultWatchInterval
	if v := os.Getenv("ZCODE_WATCH_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 30 {
			interval = time.Duration(n) * time.Second
		}
	}
	parallel := defaultClaimParallel
	if v := os.Getenv("ZCODE_CLAIM_PARALLEL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			parallel = n
		}
	}
	webhook := os.Getenv("ZCODE_WEIXIN_WORK_WEBHOOK")

	log.Info("投放监视已启动",
		slog.Duration("interval", interval),
		slog.Int("parallel", parallel),
		slog.Bool("wecom", webhook != ""))

	for {
		planIDs := detectDelivery(client, log)
		if len(planIDs) > 0 {
			if watchBatchRunning.Load() {
				log.Info("投放在批，跳过本轮", slog.Any("plan_ids", planIDs))
			} else if jwts := func() []string {
				accs := watchAccounts(client)
				out := make([]string, 0, len(accs))
				for _, a := range accs {
					out = append(out, a.JWT)
				}
				return out
			}(); zcode.AllClaimedToday(jwts, planIDs) {
				log.Info("投放仍在但全池已领（台账跳过）", slog.Any("plan_ids", planIDs))
			} else {
				log.Info("检测到投放，立即抢批", slog.Any("plan_ids", planIDs), slog.Int("parallel", parallel))
				if watchBatchRunning.CompareAndSwap(false, true) {
					results := ClaimAllConcurrent(cfg, client, "", parallel, nil)
					watchBatchRunning.Store(false)
					summary := buildClaimSummary(planIDs, results)
					log.Info("抢批完成\n" + summary)
					if err := NotifyWeCom(webhook, summary); err != nil {
						log.Warn("企业微信播报失败", slog.String("err", err.Error()))
					}
					time.Sleep(claimBatchCooldown)
				}
			}
		}
		time.Sleep(interval)
	}
}
