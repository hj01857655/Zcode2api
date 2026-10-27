// ZCode2api-go 入口：装配 config → token → 上游客户端 → HTTP 网关。
//
// 运行（工作目录或 config.json 所在目录执行）：
//
//	go build -o zcode2api-go.exe .
//	.\zcode2api-go.exe            # 默认 127.0.0.1:3010
//
// token 来源：env ZCODE_TOKEN > ~/.zcode/v2/config.json 的
// builtin:bigmodel-start-plan apiKey（与 Python 版 zk.token() 取值等价）。
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/handler"
	"zcode2api/internal/server"
	"zcode2api/internal/zcode"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "login" {
		runLogin()
		return
	}
	serve()
}

// runLogin OAuth device flow：浏览器授权 → 新 zcodejwttoken → 写回
// config.json apiKey + credentials.json（CLI/GUI 登录态同步恢复）。
func runLogin() {
	// 用法：zcode2api-go.exe login [zai|bigmodel]，默认 zai
	provider := "zai"
	if len(os.Args) > 2 {
		provider = strings.ToLower(strings.TrimSpace(os.Args[2]))
	}
	if provider != "zai" && provider != "bigmodel" {
		log.Fatalf("provider 只能是 zai 或 bigmodel，收到 %q", provider)
	}
	flow := &zcode.DeviceFlow{Out: os.Stdout, Provider: provider}
	token, err := flow.Login()
	if err != nil {
		log.Fatalf("登录失败: %v", err)
	}
	tokenPath := zcode.LocalTokenPath()
	label := provider
	if len(os.Args) > 3 {
		label = strings.TrimSpace(os.Args[3])
	}
	pool := zcode.LoadPool(tokenPath)
	pool.Upsert(zcode.Account{
		Label: label, Provider: provider, Token: token,
		SavedAt: time.Now().Format(time.RFC3339),
	})
	if err := pool.Save(tokenPath); err != nil {
		log.Fatalf("token 已取得但保存失败: %v", err)
	}
	fmt.Printf("完成：账号 %q（品牌 %s）已保存到 %s，当前池内 %d 个账号\n",
		label, provider, tokenPath, pool.Size())
	fmt.Println("不写入 ~/.zcode 的 IDE 凭证。")
	fmt.Println("运行中的网关会在下次请求时自动重读；重启亦可。")
}

func serve() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}
	// 账号池：token.json 多账号；env ZCODE_TOKEN 作为单账号覆盖
	pool := zcode.LoadPool(zcode.LocalTokenPath())
	token := zcode.LoadToken(cfg.TokenSource)
	var client *zcode.Client
	if token != "" && pool.Size() == 0 {
		// 无池时退回单账号（env 或 ~/.zcode/v2/config.json）
		client = zcode.New(cfg.Upstream, token)
		log.Printf("账号模式：单账号（token_chars=%d）", len(token))
	} else {
		client = zcode.New(cfg.Upstream, "")
		client.Pool = pool
		log.Printf("账号模式：账号池（%d 个账号，路径 %s）", pool.Size(), zcode.LocalTokenPath())
		if pool.Size() == 0 {
			log.Printf("警告：无可用账号，/health 将报 503；先执行 ./zcode2api-go.exe login")
		}
	}
	srv := server.New(cfg, client)

	// 自动领套餐（社区叫签到，实际是限时 billing/claim）。
	// 投放节奏（官方 2026-10-26 推文）：每天 UTC 11:00 起领取，300M GLM-5.3-Flash，
	// 使用时段 UTC 15:00–次日 01:00 = 北京时间 19:00 领 / 23:00–09:00 用。
	// 因此按 19:05/20:05/21:05/22:05 各试一次覆盖投放延迟——claim 幂等，
	// 已领过（1003）与无可领套餐都只记一行日志。ZCODE_AUTO_CLAIM=0 关闭。
	if os.Getenv("ZCODE_AUTO_CLAIM") != "0" {
		go func() {
			run := func(when string) {
				for _, res := range handler.ClaimAll(cfg, client, false) {
					log.Printf("自动领套餐[%s] %s: %s", when, res.Account, res.Message)
				}
			}
			run("启动")
			for {
				next := nextClaimSlot(time.Now())
				time.Sleep(time.Until(next))
				run("定时 " + next.Format("01-02 15:04"))
			}
		}()
	}

	addr := "127.0.0.1:" + strconv.Itoa(cfg.Port)
	log.Printf("ZCode2api-go 监听 http://%s （upstream=%s）", addr, cfg.Upstream)
	log.Fatal(http.ListenAndServe(addr, srv.Handler()))
}

// claimSlots 每天尝试领取的时刻（距 00:00 的分钟数，本地时区=北京时间）。
// 投放 19:00 起；多试几次覆盖投放延迟。
var claimSlots = []int{19*60 + 5, 20*60 + 5, 21*60 + 5, 22*60 + 5}

// nextClaimSlot 下一个领取尝试时刻（今天剩余的 slot，否则明天 19:05）。
func nextClaimSlot(now time.Time) time.Time {
	for day := 0; day < 2; day++ {
		base := time.Date(now.Year(), now.Month(), now.Day()+day, 0, 0, 0, 0, now.Location())
		for _, mins := range claimSlots {
			t := base.Add(time.Duration(mins) * time.Minute)
			if t.After(now) {
				return t
			}
		}
	}
	return now.Add(24 * time.Hour)
}
