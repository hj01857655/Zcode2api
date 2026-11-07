package zcode

// 多账号池：token.json 存账号数组，按顺序选号，失败账号进入冷却。
// 兼容旧格式（单对象 {provider,token,saved_at}）自动升级为单账号池。

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Account 一个已授权账号。
type Account struct {
	Label    string `json:"label"`
	Provider string `json:"provider"`
	Token    string `json:"token"`
	SavedAt  string `json:"saved_at"`
}

// Pool 账号池（并发安全）。
type Pool struct {
	mu       sync.Mutex
	Accounts []Account
	Active   string
	cool     map[string]time.Time // label → 冷却截止
	cursor   int
	// 熔断双闸（对齐 zcode-switch 网关 pool.rs）：
	// hist —— 成功率熔断窗口：最近 tripWindow 次收尾（true=成功），
	//         错误率 ≥ 80% 落 tripCooldown 长冷却（单次短冷却压不住的慢性错误）；
	// failStreak —— 连败闸：连续失败 tripFailStreak 次同样落长冷却。
	hist       map[string][]bool
	failStreak map[string]int
	// globalUntil 全局退避闸：撞 3012/405（IP 级风控）后所有号 fail-fast，
	// 避免同 IP 其余号排队挨个撞墙（对齐 Rust 网关 is_global_risk 语义）。
	globalUntil time.Time
	// sticky 会话亲和：key（metadata.user_id）→ label。绑定号可用则固定
	// 同号（上下文缓存命中率高），失效（冷却/删除）才轮转重绑。
	sticky map[string]string
	// stickyOrder LRU 淘汰序：按绑定时间排列的 key，表满时逐出最旧绑定
	// （替代整体清空——避免所有活跃会话同时丢缓存亲和重绑）。
	stickyOrder []string
	// path token.json 路径：池模式 401 时热重读（CLI/GUI 在别的进程重登
	// 后凭据已刷新，网关无需重启）。
	path string
	// reloadOn401 池模式热重读开关：单账号模式走 Client.Token 路径，不启用
	reloadOn401 bool
}

// 熔断参数（对齐 Rust：TRIP_WINDOW=10 / 80% / TRIP_COOLDOWN=10min / 连败 5）。
const (
	tripWindow     = 10
	tripCooldown   = 10 * time.Minute
	tripFailStreak = 5
)

// poolFile token.json 的持久化形状。
type poolFile struct {
	Accounts []Account `json:"accounts"`
	Active   string    `json:"active,omitempty"`
}

// LoadPool 读 token.json；文件不存在返回空池（不报错）。
// 兼容两种形状：新 {"accounts":[...]} 与旧 {"provider","token","saved_at"}。
// path 记入池：池模式 401 时热重读（ReloadIfPoolStale）。
func LoadPool(path string) *Pool {
	p := &Pool{cool: map[string]time.Time{}, hist: map[string][]bool{}, failStreak: map[string]int{}, sticky: map[string]string{}, path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	var pf poolFile
	if json.Unmarshal(raw, &pf) == nil && len(pf.Accounts) > 0 {
		p.Accounts = pf.Accounts
		p.Active = pf.Active
		return p
	}
	var one Account
	if json.Unmarshal(raw, &one) == nil && strings.TrimSpace(one.Token) != "" {
		if one.Label == "" {
			one.Label = defaultLabel(one.Provider)
		}
		p.Accounts = []Account{one}
		p.Active = one.Label
	}
	return p
}

// ReloadPoolStale 公开入口（锁内调 ReloadIfPoolStale）：池模式 401 热重读。
func (p *Pool) ReloadPoolStale() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ReloadIfPoolStale()
}

// ReloadIfPoolStale 池模式 401 热重读：磁盘 token.json 与当前池内容不同时
// 整体替换账号表（CLI/GUI 在别的进程重登后凭据已刷新）。返回是否替换。
// 只在池模式调用（单账号走 Client.Token 路径）。调用方须持 p.mu。
func (p *Pool) ReloadIfPoolStale() bool {
	if p.path == "" {
		return false
	}
	raw, err := os.ReadFile(p.path)
	if err != nil {
		return false
	}
	var pf poolFile
	if json.Unmarshal(raw, &pf) != nil || len(pf.Accounts) == 0 {
		return false
	}
	if len(pf.Accounts) == len(p.Accounts) {
		same := true
		for i := range pf.Accounts {
			if pf.Accounts[i].Label != p.Accounts[i].Label || pf.Accounts[i].Token != p.Accounts[i].Token {
				same = false
				break
			}
		}
		if same {
			return false // 无变化：不是吊销，是别的问题
		}
	}
	p.Accounts = pf.Accounts
	p.Active = pf.Active
	p.cool = map[string]time.Time{} // 新凭据新状态
	p.hist = map[string][]bool{}
	p.failStreak = map[string]int{}
	// sticky 保留 label 映射（同 label 的会话继续命中，凭据已在 Accounts 里换新）
	return true
}

func defaultLabel(provider string) string {
	if provider == "" {
		return "default"
	}
	return provider
}

// Size 账号数。
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Accounts)
}

// Upsert 按 label 新增或替换账号。
func (p *Pool) Upsert(acc Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if acc.Label == "" {
		acc.Label = defaultLabel(acc.Provider)
	}
	for i := range p.Accounts {
		if p.Accounts[i].Label == acc.Label {
			p.Accounts[i] = acc
			p.Active = acc.Label
			return
		}
	}
	p.Accounts = append(p.Accounts, acc)
	p.Active = acc.Label
}

// Remove 删除指定 label 的账号。
func (p *Pool) Remove(label string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.Accounts {
		if p.Accounts[i].Label == label {
			p.Accounts = append(p.Accounts[:i], p.Accounts[i+1:]...)
			delete(p.cool, label)
			if p.Active == label {
				p.Active = ""
			}
			return true
		}
	}
	return false
}

// SetGlobalBackoff 全局退避：duration 内 Pick 直接 fail-fast。
func (p *Pool) SetGlobalBackoff(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d <= 0 {
		return
	}
	until := time.Now().Add(d)
	if until.After(p.globalUntil) {
		p.globalUntil = until
	}
}

// GlobalBackoffRemaining 全局退避剩余时长（0 = 无）。
func (p *Pool) GlobalBackoffRemaining() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d := time.Until(p.globalUntil); d > 0 {
		return d
	}
	return 0
}

// Pick 取下一个可用账号（轮转起点 + 跳过冷却 + 全局退避闸）。
// 返回 ok=false 的两种原因由 GlobalBackoffRemaining 区分。
func (p *Pool) Pick() (Account, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.Accounts) == 0 {
		return Account{}, false
	}
	if time.Now().Before(p.globalUntil) {
		return Account{}, false // 全局风控闸：直接 fail-fast，不烧重试轮次
	}
	now := time.Now()
	n := len(p.Accounts)
	for i := 0; i < n; i++ {
		idx := (p.cursor + i) % n
		a := p.Accounts[idx]
		if until, bad := p.cool[a.Label]; bad && now.Before(until) {
			continue
		}
		p.cursor = (idx + 1) % n
		return a, true
	}
	// 全冷却：取最早解冻的那个（避免整体不可用）
	best, bestUntil := 0, time.Time{}
	for i, a := range p.Accounts {
		if until, bad := p.cool[a.Label]; bad {
			if bestUntil.IsZero() || until.Before(bestUntil) {
				best, bestUntil = i, until
			}
		}
	}
	p.cursor = (best + 1) % n
	return p.Accounts[best], true
}

// stickyLimit 绑定表上限：超过即整体清空（防长驻进程无限增长；
// 重建代价只是各会话重新绑定一次）。
const stickyLimit = 1024

// PickSticky 会话亲和选号：key 为空退化为普通轮转。
// 绑定命中（账号存在且不在冷却/退避）直接复用；否则轮转取号并重绑。
func (p *Pool) PickSticky(key string) (Account, bool) {
	if key == "" {
		return p.Pick()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.Accounts) == 0 {
		return Account{}, false
	}
	if time.Now().Before(p.globalUntil) {
		return Account{}, false
	}
	// 命中校验：绑定号仍存在且可用
	if label, bad := p.sticky[key]; bad {
		for _, a := range p.Accounts {
			if a.Label != label {
				continue
			}
			if until, cool := p.cool[a.Label]; cool && time.Now().Before(until) {
				break // 绑定号在冷却，走轮转重绑
			}
			return a, true // 复用绑定号（cursor 不动）
		}
	}
	// 轮转取号
	now := time.Now()
	n := len(p.Accounts)
	for i := 0; i < n; i++ {
		idx := (p.cursor + i) % n
		a := p.Accounts[idx]
		if until, cool := p.cool[a.Label]; cool && now.Before(until) {
			continue
		}
		p.cursor = (idx + 1) % n
		p.bindSticky(key, a.Label)
		return a, true
	}
	// 全冷却：从最早解冻的号起轮转绑定（雷群效应修复）——所有新会话
	// 落在不同号上，解冻瞬间不会全部挤到同一个号
	best, bestUntil := 0, time.Time{}
	for i, a := range p.Accounts {
		if until, cool := p.cool[a.Label]; cool {
			if bestUntil.IsZero() || until.Before(bestUntil) {
				best, bestUntil = i, until
			}
		}
	}
	a := p.Accounts[(best+len(p.sticky))%n] // 按 sticky 数量偏移：新会话错开落号
	p.cursor = (best + 1) % n
	p.bindSticky(key, a.Label)
	return a, true
}

// bindSticky 写绑定表：LRU 逐出最旧绑定（stickyOrder 维护插入序）。
// 调用方须持 p.mu。
func (p *Pool) bindSticky(key, label string) {
	if _, exists := p.sticky[key]; !exists {
		p.stickyOrder = append(p.stickyOrder, key)
	}
	p.sticky[key] = label
	for len(p.sticky) > stickyLimit && len(p.stickyOrder) > 0 {
		oldest := p.stickyOrder[0]
		p.stickyOrder = p.stickyOrder[1:]
		if _, ok := p.sticky[oldest]; ok {
			delete(p.sticky, oldest)
		}
	}
}

// StickyCount 返回绑定表拷贝（key→label），诊断观测用：直通路径的亲和
// 表应该只有客户端真实 user_id 进来，混入其它形状的键说明有调用点传错了键。
func (p *Pool) StickyCount() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]string, len(p.sticky))
	for k, v := range p.sticky {
		out[k] = v
	}
	return out
}

// Cooldown 让某账号冷却一段时间。
func (p *Pool) Cooldown(label string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d <= 0 {
		return
	}
	p.cool[label] = time.Now().Add(d)
}

// Settle 请求收尾记账：ok=false 时推进成功率窗口与连败计数，
// 触发双闸任一即落 tripCooldown 长冷却并清连败（避免重复触发）。
// 返回本次是否触发了熔断（调用方可用于日志）。
func (p *Pool) Settle(label string, ok bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ok {
		delete(p.failStreak, label)
		h := p.hist[label]
		h = append(h, true)
		if len(h) > tripWindow {
			h = h[len(h)-tripWindow:]
		}
		p.hist[label] = h
		return false
	}
	streak := p.failStreak[label] + 1
	p.failStreak[label] = streak
	h := append(p.hist[label], false)
	if len(h) > tripWindow {
		h = h[len(h)-tripWindow:]
	}
	p.hist[label] = h
	rateBad := len(h) == tripWindow && countFalse(h)*5 >= tripWindow*4
	streakBad := streak >= tripFailStreak
	if rateBad || streakBad {
		p.cool[label] = time.Now().Add(tripCooldown)
		delete(p.failStreak, label) // 长冷却已生效，重新计连败
		p.hist[label] = nil
		return true
	}
	return false
}

func countFalse(h []bool) int {
	n := 0
	for _, v := range h {
		if !v {
			n++
		}
	}
	return n
}

// CooldownRemaining 账号剩余冷却时长（0 = 不在冷却）。
func (p *Pool) CooldownRemaining(label string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, bad := p.cool[label]
	if !bad {
		return 0
	}
	if d := time.Until(until); d > 0 {
		return d
	}
	return 0
}

// Snapshot 账号概要（label/provider/是否冷却），用于 /health。
func (p *Pool) Snapshot() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.Accounts))
	now := time.Now()
	for _, a := range p.Accounts {
		item := map[string]any{
			"label": a.Label, "provider": a.Provider,
			"token_chars": len(a.Token), "saved_at": a.SavedAt,
		}
		// JWT 过期时间（CST8）随快照下发，面板据此做红黄分级预警
		if exp, ok := jwtExp(a.Token); ok {
			left := int(exp.Sub(now).Seconds())
			item["exp_at"] = exp.In(CST8).Format("01-02 15:04")
			item["expired"] = left < 0
			item["exp_left_s"] = left
		}
		if until, bad := p.cool[a.Label]; bad && now.Before(until) {
			item["cooldown_s"] = int(time.Until(until).Seconds())
		}
		out = append(out, item)
	}
	return out
}

// Save 落盘（0600，原子替换）。
func (p *Pool) Save(path string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	sort.SliceStable(p.Accounts, func(i, j int) bool { return p.Accounts[i].Label < p.Accounts[j].Label })
	b, err := json.MarshalIndent(poolFile{Accounts: p.Accounts, Active: p.Active}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return fmt.Errorf("写 %s: %w", tmp, err)
	}
	return os.Rename(tmp, path)
}
