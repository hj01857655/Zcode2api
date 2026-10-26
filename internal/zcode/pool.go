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
}

// poolFile token.json 的持久化形状。
type poolFile struct {
	Accounts []Account `json:"accounts"`
	Active   string    `json:"active,omitempty"`
}

// LoadPool 读 token.json；文件不存在返回空池（不报错）。
// 兼容两种形状：新 {"accounts":[...]} 与旧 {"provider","token","saved_at"}。
func LoadPool(path string) *Pool {
	p := &Pool{cool: map[string]time.Time{}}
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

// Pick 取下一个可用账号（轮转起点 + 跳过冷却）；全部冷却时返回 false。
func (p *Pool) Pick() (Account, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.Accounts) == 0 {
		return Account{}, false
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

// Cooldown 让某账号冷却一段时间。
func (p *Pool) Cooldown(label string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d <= 0 {
		return
	}
	p.cool[label] = time.Now().Add(d)
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
