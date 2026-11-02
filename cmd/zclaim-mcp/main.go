// zclaim-mcp：把 zcode-switch 账号池的臭鸡蛋（限时套餐）查询/领取包成 MCP server，
// stdio 传输，Breezell 等 MCP 客户端按 mcpServers 配置拉起。
// 复用 internal/zcode：账号解密（accounts.go）+ 领取协议（claim.go/captcha.go）。
// 安全边界：凭据只在本机解密，JWT/device_mid 全程不出进程——返回值只含
// uid、邮箱、套餐名、额度数字等摘要字段。
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"zcode2api/internal/zcode"
)

// onlyArgs 所有工具共用的账号过滤参数（文件名子串，ZCODE_ONLY 同语义）。
type onlyArgs struct {
	Only string `json:"only,omitempty" jsonschema:"只处理文件名包含这些子串的账号（逗号分隔）；留空处理全部账号"`
}

var (
	accountsDir string // -accounts 覆盖；空则回落 zcode.AccountsDir()（含 env ZCODE_ACCOUNTS_DIR）
	httpClient  = &http.Client{Timeout: 30 * time.Second}
)

func splitOnly(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func loadAccounts(only string) []zcode.SwitchAccount {
	dir := accountsDir
	if dir == "" {
		dir = zcode.AccountsDir()
	}
	return zcode.LoadAccounts(dir, splitOnly(only))
}

// human 额度单位口语化（token 计数：亿/万）。
func human(n int64) string {
	switch {
	case n >= 1e8:
		return fmt.Sprintf("%.2f亿", float64(n)/1e8)
	case n >= 1e4:
		return fmt.Sprintf("%.1f万", float64(n)/1e4)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func balanceTool(ctx context.Context, req *mcp.CallToolRequest, args onlyArgs) (*mcp.CallToolResult, string, error) {
	var b strings.Builder
	for _, a := range loadAccounts(args.Only) {
		res, err := zcode.FetchBalance(httpClient, a)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "── %s [%s] 查询失败: %v\n", a.Name, a.File, err)
		case res.Err != "":
			fmt.Fprintf(&b, "── %s [%s] ✗ %s\n", res.Account, res.File, res.Err)
		default:
			fmt.Fprintf(&b, "── %s [%s] uid=%s\n", res.Account, res.File, res.UID)
			for _, e := range res.Entries {
				fmt.Fprintf(&b, "   %s %s 剩 %s/%s（已用 %s）到期 %s\n",
					e.Plan, e.Model, human(e.Remaining), human(e.Total), human(e.Used), e.ExpiresAt)
			}
		}
	}
	if b.Len() == 0 {
		return nil, "账号目录为空或过滤条件没命中任何账号", nil
	}
	return nil, strings.TrimRight(b.String(), "\n"), nil
}

func claimTool(ctx context.Context, req *mcp.CallToolRequest, args onlyArgs) (*mcp.CallToolResult, string, error) {
	var b strings.Builder
	for _, a := range loadAccounts(args.Only) {
		if a.Err != "" {
			fmt.Fprintf(&b, "── %s [%s] 跳过: %s\n", a.Name, a.File, a.Err)
			continue
		}
		mid := a.DeviceMid
		if mid == "" {
			// 账号文件没带 virtual_device_mid 时兜底生成并落盘固定（3001 防线）。
			mid = zcode.StableDeviceMid(accountsDir, a.JWT)
		}
		msg, _ := zcode.ClaimBestForToken(httpClient, a.JWT, mid)
		fmt.Fprintf(&b, "── %s [%s] %s\n", a.Name, a.File, msg)
	}
	if b.Len() == 0 {
		return nil, "账号目录为空或过滤条件没命中任何账号", nil
	}
	return nil, strings.TrimRight(b.String(), "\n"), nil
}

func identTool(ctx context.Context, req *mcp.CallToolRequest, args onlyArgs) (*mcp.CallToolResult, string, error) {
	var b strings.Builder
	for _, a := range loadAccounts(args.Only) {
		if a.Err != "" {
			fmt.Fprintf(&b, "── %s [%s] ✗ %s\n", a.Name, a.File, a.Err)
			continue
		}
		midf := a.DeviceMid
		if len(midf) > 8 {
			midf = midf[:8]
		}
		exp := "?"
		if !a.ExpAt.IsZero() {
			exp = a.ExpAt.In(zcode.CST8).Format("2006-01-02 15:04")
		}
		fmt.Fprintf(&b, "── %s [%s] uid=%s email=%s exp=%s mid=%s…\n",
			a.Name, a.File, a.UID, a.Email, exp, midf)
	}
	if b.Len() == 0 {
		return nil, "账号目录为空或过滤条件没命中任何账号", nil
	}
	return nil, strings.TrimRight(b.String(), "\n"), nil
}

func main() {
	flag.StringVar(&accountsDir, "accounts", "",
		"zcode-switch 账号目录（默认 ~/.zcode-switch/accounts，env ZCODE_ACCOUNTS_DIR 可覆盖）")
	flag.Parse()

	server := mcp.NewServer(&mcp.Implementation{Name: "zclaim", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "zclaim_balance",
		Description: "查询 zcode 账号池的臭鸡蛋余额：每号当前批次、模型额度剩余/总量、到期时间",
	}, balanceTool)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "zclaim_claim",
		Description: "对账号池领取当日臭鸡蛋（幂等：1003=已领算成功；自动补报激活事件、处理阿里云验证码）",
	}, claimTool)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "zclaim_ident",
		Description: "账号池身份核对：uid/邮箱/JWT 过期时间/device_mid 指纹（不含任何 token）",
	}, identTool)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "zclaim-mcp:", err)
		os.Exit(1)
	}
}
