# ZCode OAuth

> 三通道索引：**免费模型请求** → PROTOCOL.md ｜ **OAuth 登录** → OAUTH.md ｜ **付费 coding-plan + V4 签名** → PAID.md
> 配套：**领取协议** → CLAIM.md ｜ **token 生命周期** → TOKEN.md ｜ **协议分析报告** → REPORT.md

## 品牌

| | BigModel | Z.AI |
|---|---|---|
| provider 参数 | bigmodel | zai |
| authorize_url | https://bigmodel.cn/login?appId=zcode&redirect=https://zcode.z.ai/api/v1/oauth/cli/callback/bigmodel | https://chat.z.ai/api/oauth/authorize?client_id=client_P8X5CMWmlaRO9gyO-KSqtg&redirect_uri=https://zcode.z.ai/api/v1/oauth/cli/callback/zai |
| 凭据键 | oauth:bigmodel:* | oauth:zai:* |

两品牌共用 zcode-plan 模型端点；zcodejwttoken 品牌无关；额度桶按品牌独立。

## init

POST https://zcode.z.ai/api/v1/oauth/cli/init
Authorization: Bearer <poll_token>
Content-Type: application/json

{"provider": "bigmodel" | "zai"}

poll_token 客户端生成（32 字节 hex）。响应 data：flow_id、authorize_url、poll_token（服务端下发时必须改用）。

## poll

GET https://zcode.z.ai/api/v1/oauth/cli/poll/{flow_id}
Authorization: Bearer <poll_token>

data.status: pending | ready | failed

ready 时 data：

{
  "token": "<zcodejwttoken>",
  "user": {"user_id": "...", "email": "...", "name": "..."},
  "<provider>": {"access_token": "...", "refresh_token": "..."}
}

data.token 即 zcodejwttoken，无二次交换。

## 网关实现

```
zcode2api-go.exe login [zai|bigmodel] [label]
```

流程：init → 打印 authorize_url（并调系统浏览器）→ 2s 间隔轮询 poll（上限 150 次）→
ready 取 data.token → 写入 **token.json 账号池**（`{accounts:[{label,provider,token,saved_at}]}`，
0600 原子写；label 默认品牌名，同名 Upsert）。

**不写 `~/.zcode` 的任何文件**（旧版写回 config.json apiKey + credentials.json 的行为已移除，
`auth.go` 仅留过时注释）。运行中的网关遇 401 会重读 token 源并重试一次；池模式则冷却换号
——详见 docs/TOKEN.md。

## 实测（2026-10-26）

- bigmodel init → 200，flow_id 0bd455291e426ee047728b86af019a86
- zai init → 200，flow_id a8d193f2fb76797fb46dbc4750476c7c
- poll pending 正常流转
- ready 后 token 池化落盘：Go 侧离线测试覆盖（auth_test.go），线上真登未执行

## 边界

- zcodejwttoken 短期有效，过期 = 模型请求 401 空 body（失效处理全图见 docs/TOKEN.md）
- refresh_token 已随 poll 数据可见，静默续期未实现——过期唯一出路是重新 login
- 2026-09 起官方协议：poll 必须用服务端 init 下发的 poll_token（auth.go 已对齐）
