// 套餐领取：POST /admin/api/claim 起后台任务（?account= 单号），立即返回
// task_id；GET /admin/api/claim/progress?id= 轮询逐号进度。领取链每号都要跑
// 激活上报→touch→验证码求解（node 子进程）→claim，全部账号串行可达分钟级，
// 同步 HTTP 等待会表现为前端永久"处理中"，因此任务化。
// ClaimAll/ClaimAllProgress 供 main 的每日自动领取复用。
package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/zcode"
)

// claimHTTP 领取协议的短超时 client（chat 上游的 300s 不适用这里）。
var claimHTTP = &http.Client{Timeout: 30 * time.Second}

// claimStateDir device_mid 落点：与 token.json 同目录（本项目的状态目录语义）。
var claimStateDir = func() string {
	return filepath.Dir(zcode.LocalTokenPath())
}

// claimTargets 领取目标账号列表（与 ClaimAll 的选择逻辑一致，用于预估 total）。
func claimTargets(client *zcode.Client, only string) []string {
	labels := []string{}
	if client.Pool != nil {
		for _, acc := range client.Pool.Accounts {
			if acc.Token != "" && (only == "" || acc.Label == only) {
				labels = append(labels, acc.Label)
			}
		}
	} else if client.Token != "" && only == "" {
		labels = append(labels, "default")
	}
	return labels
}

// poolJWT 按 label 取账号 JWT（池缺失时回退单账号 token，与 claimTargets 同构）。
func poolJWT(client *zcode.Client, label string) string {
	if client.Pool != nil {
		for _, acc := range client.Pool.Accounts {
			if acc.Label == label {
				return acc.Token
			}
		}
		return ""
	}
	return client.Token
}

// claimOneFunc 单号领取入口（测试替换接缝；串行与并发两条路径共用，
// 保证台账/验证码/退避语义只此一份）。
var claimOneFunc = func(jwt, mid string) string {
	msg, _ := zcode.ClaimBestForToken(claimHTTP, jwt, mid)
	return msg
}

// ClaimAllProgress 逐账号领取，每完成一个回调 res（面板任务用它更新进度）。
// only 非空时只跑指定 label；onResult 可为 nil。
func ClaimAllProgress(cfg *config.Config, client *zcode.Client, previewOnly bool, only string, onResult func(zcode.ClaimResult)) []zcode.ClaimResult {
	out := []zcode.ClaimResult{}
	emit := func(r zcode.ClaimResult) {
		out = append(out, r)
		if onResult != nil {
			onResult(r)
		}
	}
	targets := claimTargets(client, only)
	stateDir := claimStateDir()
	for _, label := range targets {
		var jwt string
		if client.Pool != nil {
			for _, acc := range client.Pool.Accounts {
				if acc.Label == label {
					jwt = acc.Token
				}
			}
		} else {
			jwt = client.Token
		}
		mid := zcode.StableDeviceMid(stateDir, jwt)
		if previewOnly {
			// 只看可领也走 touch：preview 通道已整体删除（zcode-hub 实测恒空）
			dels, err := zcode.TouchPlans(claimHTTP, jwt, mid)
			if err != nil {
				emit(zcode.ClaimResult{Account: label, OK: false, Message: "touch 失败: " + err.Error()})
				continue
			}
			n := 0
			var ids []string
			for _, d := range dels {
				ids = append(ids, d.PlanIDs...)
				n += len(d.PlanIDs)
			}
			msg := "现在没有可领取的套餐（touch 无投放）"
			if n > 0 {
				msg = "可领 " + strconv.Itoa(n) + " 个投放套餐: " + strings.Join(ids, ",")
			}
			emit(zcode.ClaimResult{Account: label, OK: true, Message: msg})
			continue
		}
		msg, _ := zcode.ClaimBestForToken(claimHTTP, jwt, mid)
		emit(zcode.ClaimResult{Account: label, OK: true, Message: msg})
	}
	return out
}

// ClaimAllConcurrent 有界并发领取（投放监视的抢批路径）。
// 并发上限默认 4：claim 限流是出口 IP 级（连领 4 个后第 5 个必撞，见
// ClaimBestForToken 提交循环注释），全池齐发只会把整个批次推进 429 重试波次；
// 有界并发让前 4 个号在验证码求解期间其余号排队，命中率与串行一致而墙钟时间
// 缩短到约 1/4。parallel<=0 时取默认值。
func ClaimAllConcurrent(cfg *config.Config, client *zcode.Client, only string, parallel int, onResult func(zcode.ClaimResult)) []zcode.ClaimResult {
	if parallel <= 0 {
		parallel = defaultClaimParallel
	}
	targets := claimTargets(client, only)
	stateDir := claimStateDir()
	sem := make(chan struct{}, parallel)
	var mu sync.Mutex
	out := []zcode.ClaimResult{}
	var wg sync.WaitGroup
	for _, label := range targets {
		wg.Add(1)
		go func(label string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			jwt := poolJWT(client, label)
			if jwt == "" {
				return
			}
			mid := zcode.StableDeviceMid(stateDir, jwt)
			r := zcode.ClaimResult{Account: label, OK: true, Message: claimOneFunc(jwt, mid)}
			mu.Lock()
			out = append(out, r)
			mu.Unlock()
			if onResult != nil {
				onResult(r)
			}
		}(label)
	}
	wg.Wait()
	return out
}

// ClaimAll 同步版本（main 的定时领取用，无进度回调）。
func ClaimAll(cfg *config.Config, client *zcode.Client, previewOnly bool, only string) []zcode.ClaimResult {
	return ClaimAllProgress(cfg, client, previewOnly, only, nil)
}

// claimTask 一次领取任务的运行状态。
type claimTask struct {
	id      string
	total   int
	created time.Time
	done    atomic.Int64
	running atomic.Bool
	mu      sync.Mutex
	results []zcode.ClaimResult
}

func (t *claimTask) add(r zcode.ClaimResult) {
	t.mu.Lock()
	t.results = append(t.results, r)
	t.mu.Unlock()
	t.done.Add(1)
}

func (t *claimTask) snapshot() ([]zcode.ClaimResult, int64, bool) {
	t.mu.Lock()
	res := append([]zcode.ClaimResult{}, t.results...)
	t.mu.Unlock()
	return res, t.done.Load(), t.running.Load()
}

// claimTasks 活任务表；新任务注册时懒清理 30 分钟以上的旧任务。
var claimTasks sync.Map

// ClaimHandler POST /admin/api/claim。?preview=1 只列不领；?account=label 只跑单号。
func ClaimHandler(cfg *config.Config, client *zcode.Client, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r, cfg) {
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		previewOnly := r.URL.Query().Get("preview") == "1"
		only := r.URL.Query().Get("account")
		total := len(claimTargets(client, only))
		t := &claimTask{id: zcode.UUIDv4()[:12], total: total, created: time.Now()}
		t.running.Store(true)
		now := time.Now()
		claimTasks.Range(func(k, v any) bool {
			if tk, ok := v.(*claimTask); ok && now.Sub(tk.created) > 30*time.Minute {
				claimTasks.Delete(k)
			}
			return true
		})
		claimTasks.Store(t.id, t)
		go func() {
			defer t.running.Store(false)
			ClaimAllProgress(cfg, client, previewOnly, only, t.add)
			if log != nil {
				log.Info("套餐领取任务完成", slog.String("task", t.id), slog.Bool("preview", previewOnly), slog.Int("accounts", total))
			}
		}()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "task_id": t.id, "total": total})
	}
}

// ClaimProgressHandler GET /admin/api/claim/progress?id=xx 轮询领取任务。
func ClaimProgressHandler(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r, cfg) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		v, ok := claimTasks.Load(r.URL.Query().Get("id"))
		if !ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "task not found"})
			return
		}
		t := v.(*claimTask)
		res, done, running := t.snapshot()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task_id": t.id, "total": t.total, "done": done,
			"running": running, "results": res,
		})
	}
}
