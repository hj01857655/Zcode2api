# ZCode2api-go

![Go](https://img.shields.io/badge/Go-1.22-00ADD8?logo=go&logoColor=white) ![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20Linux%20%7C%20macOS-lightgrey) ![Type](https://img.shields.io/badge/type-API%20Gateway-blue)

ZCode Start Plan 额度 → OpenAI / Anthropic 兼容网关。Go 标准库实现，零第三方依赖。

支持三种入站协议（OpenAI Chat Completions、OpenAI Responses、Anthropic Messages），
统一转发到上游 `POST https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages`。

## 快速开始

```bash
go build -o zcode2api-go.exe .
./zcode2api-go.exe                 # 默认 127.0.0.1:3010
```

token 解析顺序：

1. 环境变量 `ZCODE_TOKEN`
2. 本目录 `token.json`（`login` 子命令写入，格式 `{provider,token,saved_at}`）
3. `~/.zcode/v2/config.json` 中任一 `*-start-plan` provider 的 `options.apiKey`
   （优先 `builtin:zai-start-plan`，其次 `builtin:bigmodel-start-plan`，再按名称排序兜底）

## 三协议适配

| 端点 | 入站协议 | 出站 |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions | `chat.completion` / `chat.completion.chunk` |
| `POST /v1/responses` | OpenAI Responses | `response` 对象 / `response.*` SSE 事件 |
| `POST /v1/messages` | Anthropic Messages | 原生 `message` / 原生 SSE（流式透传） |
| `GET /v1/models` | — | `GLM-5.3-Flash` / `GLM-5.3` / `GLM-4.7` |
| `GET /health` | — | `{ok, token_chars, upstream, version}` |
| `GET /admin` | — | Web 控制台（内置，开箱即用）：凭据/账号池状态、模型列表、诊断 |

### 字段映射

| 能力 | chat/completions | responses | messages |
|---|---|---|---|
| 文本（流式/非流式） | `messages` → `messages` | `input`/`instructions` → `messages`/`system` | 原生透传 |
| system | `role:"system"` 追加到前缀之后 | `instructions` 追加到前缀之后 | 客户端 `system` 追加到前缀之后 |
| 工具调用 | `tools[].function` → `tools` | `tools[]`（`type:"function"`）→ `tools` | 原生 `tools` 透传 |
| 工具结果 | `role:"tool"` + `tool_call_id` | `function_call_output` 输入项 | `{type:"tool_result"}` |
| 思考档位 | `reasoning_effort` → `output_config.effort` | `reasoning.effort` → 同上 | `output_config.effort` 透传 |
| 结构化输出 | — | `text.format`（json_schema/json_object）→ 提示词约束 | — |
| 最大 token | `max_tokens` | `max_output_tokens` | `max_tokens` 透传 |
| 采样参数 | `temperature`/`top_p`/`stop` | `temperature`/`top_p` | 透传 |

### 实测状态

三种协议的文本、工具调用、工具多轮闭环均实测通过；chat 的并行工具（2 调用）与图片输入
（`image_url` data URI → base64 块）实测通过；responses 的 `json_schema` 返回合法 JSON。

上游限制：

- `tool_choice` 被上游忽略（`type:tool` / `type:any` / 不传三种均返回纯文本 `end_turn`），
  无法强制模型调用指定工具。网关侧映射正确。
- `input_tokens` 在流式 `message_start` 中上游恒报 0；`output_tokens` 正常。
- 上游对非流式 JSON 回 br 压缩，零依赖 Go 侧不解压，故网关对上游一律 `stream=true` 后自行聚合。

未实现：`previous_response_id` / `conversation`（有状态会话）。

## 上游协议约束

- system 必须以官方前缀 seg0+seg1 打头（`internal/zcode/assets/prefix.json`，前缀匹配，可追加自定义段）
- HTTP 头 / UA / TLS 指纹 / device_id / metadata 均不校验，头组保持极简
- 思考档位走 `output_config.effort`（`low`/`high`/`max`）；不传 = 无思考；
  显式 `thinking:{type:"disabled"}` 且无 tools 会被上游拒绝（code 1210）
- ZCode 客户端大版本更新后若返回 405 / code 3012，需重新提取 system 前缀覆盖 prefix.json

完整协议与 26 发消融记录见 `docs/PROTOCOL.md`。

## OAuth 重新授权

```bash
./zcode2api-go.exe login            # 默认品牌 zai
./zcode2api-go.exe login bigmodel   # 指定品牌
```

流程：`POST /api/v1/oauth/cli/init` → 打印 `authorize_url`（5 分钟内有效，state 一次性）
→ 浏览器授权 → `GET /oauth/cli/poll/{flow_id}` 每 2s 轮询 → ready 时 `data.token` 即 zcodejwttoken。

token 只写入本目录 `token.json`（0600），不修改 `~/.zcode` 下的 IDE 凭证。
运行中的网关在下一次请求或 401 时自动重读，无需重启。

协议细节见 `docs/OAUTH.md`。

## 配置

默认值 → 本目录 `config.json` → 环境变量。

| 项 | 默认 | 说明 |
|---|---|---|
| port | 3010 | 监听端口（绑定 127.0.0.1） |
| upstream | zcode-plan messages 端点 | 一般不改 |
| client_version | 3.16.3 | 进 UA 与额度查询参数 |
| default_model | GLM-5.3-Flash | 表外模型名的回落值 |
| models | GLM-5.3-Flash / GLM-5.3 / GLM-4.7 | /v1/models 输出 |
| api_key | 空 | 非空时要求 `Authorization: Bearer` 或 `x-api-key` |
| max_tokens_cap | 128000 | max_tokens 上限 |

环境变量：`ZCODE_TOKEN`、`DUMP=0`（关闭上游落盘）、`DUMP_MAX_FILES`。

## 测试

```bash
go test ./...      # 全离线：httptest 假上游锁协议形状
go vet ./...
```

覆盖：system 前缀注入、SSE 翻译（text/thinking/tool_use/input_json_delta）、工具累积、
错误映射、token 双品牌选择、OAuth init/poll、credentials 加密写回、模型别名。

## 目录

```
main.go                       入口（serve；login 子命令）
internal/config/              配置（默认值 + config.json + env）
internal/zcode/
  client.go                   上游客户端 + token 解析
  translate.go                OpenAI ↔ Anthropic 转换
  auth.go                     OAuth device flow + 本地 token 存储
  assets/prefix.json          system 白名单前缀
internal/server/
  server.go                   路由、鉴权、chat/completions、错误映射
  responses.go                /v1/responses（转换 + response.* 事件）
  anthropic.go                /v1/messages（前缀注入 + 聚合/透传）
internal/handler/
  panel.go                    /admin 内置控制台（embed panel.html）
  admin.go                    /admin/api/overview（凭据状态、模型、鉴权开关）
  claim.go                    /admin/api/claim（限时套餐领取）
internal/zcode/
  claim.go                    套餐领取协议（preview/claim/激活上报/device_mid）
docs/                         PROTOCOL.md / OAUTH.md / PAID.md
```

## 限时套餐领取（自动签到）

社区口径的「ZCode 签到」实际是限时套餐领取：`GET /api/v1/zcode-plan/billing/preview` 列当前可领套餐 → `POST /api/v1/zcode-plan/billing/claim` 按 priority 领取（协议来自社区青龙脚本 L0NE-6/ZCode-Daily 的抓包结论，已用真实 JWT 实测验证）。网关集成：

- **自动领取**：启动时跑一次 + 每 24h 一轮，对账号池每个账号：补报激活事件（app_launch/app_daily_active，领取资格前置）→ preview → 领优先级最高的套餐。幂等——已领过（1003）与无可领套餐都只记日志。`ZCODE_AUTO_CLAIM=0` 关闭
- **手动触发**：`POST /admin/api/claim`（`?preview=1` 只列不领），或面板概览页「限时套餐领取」按钮
- **device_mid**：每账号固定 UUID 落盘 `zcode_devices.json`（与 token.json 同目录），换 mid 会触发 3001

## 部署

```bash
docker compose up -d          # 需先配置 .env（见 .env.example）
```

镜像同时发布到 GHCR：`ghcr.io/hj01857655/zcode2api-go`（跟随 `v*` 标签发布，含 `latest` 与 `major.minor`，linux/amd64 + linux/arm64）。
二进制发版：推送 `v*` 标签、发布 Release 或手动触发 workflow_dispatch 后，
Actions 构建 linux / windows / darwin 的 amd64 + arm64 六平台 tar.gz 产物。
