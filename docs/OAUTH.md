# ZCode OAuth

> 三通道索引：**免费模型请求** → PROTOCOL.md ｜ **OAuth 登录** → OAUTH.md ｜ **付费 coding-plan + V4 签名** → PAID.md

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
zcode2api-go.exe login
```

流程：init → 打印 authorize_url → 2s 间隔轮询 poll → ready 取 data.token → 写回两处：

- config.json 的 provider["builtin:bigmodel-start-plan"].options.apiKey（明文，网关下次启动读）
- ~/.zcode/v2/credentials.json 的 zcodejwttoken（enc:v1 加密，CLI/GUI 登录态同步）

写回语义：其它键原样保留，落盘 token 仅以密文出现，写前不破坏原文件结构。
运行中网关遇 401 自动重读 token 并重试一次，login 完成后无需重启。

## 实测（2026-10-26）

- bigmodel init → 200，flow_id 0bd455291e426ee047728b86af019a86
- zai init → 200，flow_id a8d193f2fb76797fb46dbc4750476c7c
- poll pending 正常流转
- ready 写回：Go 侧离线测试覆盖（auth_test.go），线上真登未执行

## 边界

- zcodejwttoken 短期有效，过期 = 模型请求 401 空 body
- refresh_token 已存储，静默续期未实现
- GUI 同时运行可能与写回产生竞态
