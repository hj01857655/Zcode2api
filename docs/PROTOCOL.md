# ZCode Start Plan 协议（实测锁定）

> 三通道索引：**免费模型请求** → PROTOCOL.md ｜ **OAuth 登录** → OAUTH.md ｜ **付费 coding-plan + V4 签名** → PAID.md

> 2026-10-26，ZCode 3.16.3 CLI 真机抓包 + 26 发消融矩阵。协议如变需重新校准。

## 端点

| 用途 | 端点 | 认证 |
|---|---|---|
| 模型请求 | POST https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages | Bearer zcodejwttoken |
| 额度查询 | GET https://zcode.z.ai/api/v1/zcode-plan/billing/balance?app_version=3.16.3 | Bearer + Content-Type + X-Device-Mid |
| 客户端配置 | GET https://zcode.z.ai/api/v1/client/configs?app_version=3.16.3&platform=win32-x64 | 仅 X-Device-Mid |
| OAuth | https://zcode.z.ai/api/v1/oauth/cli/{init,poll/{flow_id}} | 见 docs/OAUTH.md |

## 请求头

必需：Authorization: Bearer <zcodejwttoken>、Content-Type: application/json。
非必需：anthropic-version、User-Agent、X-Title、X-ZCode-*、x-request-id、x-zcode-trace-id。
不发送：x-api-key、X-Device-Mid、任何签名头（模型请求端点）。

## 请求体

```json
{
  "model": "GLM-5.3-Flash",
  "messages": [{"role": "user", "content": "..."}],
  "max_tokens": 4096,
  "stream": true,
  "system": [
    {"type": "text", "text": "You are ZCode, an interactive coding agent"},
    {"type": "text", "text": "<官方主提示词 2313 字符>"}
  ],
  "output_config": {"effort": "low"},
  "metadata": {"user_id": "{\"device_id\":\"<uuid>\",\"account_uuid\":\"\",\"session_id\":\"\"}"}
}
```

model 大小写敏感：GLM-5.3-Flash、GLM-5.3 可用。
metadata.user_id 是内嵌 JSON 字符串；device_id 取 telemetry-state.json 的 deviceMid，任意 uuid 可过。
output_config.effort：low=纯文本、high/max=带思考块；不传=无思考。

## 风控

唯一键 = system 前缀白名单。system 必须以官方 seg0+seg1 开头。
前缀匹配：seg1 改末尾字符可过、其后可追加自定义段、cache_control 可省。
仅 seg0 或无 system → 405 + `{"code":3012,"msg":"request has been blocked due to unusual activity."}`。
不校验：HTTP 头、UA、TLS 指纹、device_id、metadata、IP、频率。

## 三种对外协议入口

| 端点 | 入站协议 | 出站 |
|---|---|---|
| POST /v1/chat/completions | OpenAI Chat Completions | OpenAI chunk / completion |
| POST /v1/responses | OpenAI Responses | response 对象 / response.* 事件 |
| POST /v1/messages | Anthropic Messages | 原生 message / SSE 透传 |

三者共用上游 zcode-plan/anthropic/v1/messages 与 system 白名单前缀；非流式由网关聚合上游 SSE
（上游对非流式 JSON 会回 br 压缩，零依赖 Go 侧不解压，故统一走流式聚合）。

### 工具调用（三协议）

| 协议 | 入站 tools | 出站工具结果 |
|---|---|---|
| chat/completions | `[{type:"function",function:{name,description,parameters}}]` | `message.tool_calls[]` + `finish_reason:"tool_calls"`；流式按 `tool_calls[].index` 分帧（首帧 id/name，后续 arguments 增量） |
| responses | `[{type:"function",name,description,parameters}]` | `output[]` 里 `{type:"function_call",call_id,name,arguments}`；流式 `response.function_call_arguments.delta` |
| messages | Anthropic 原生 `[{name,description,input_schema}]` | 原生 `{type:"tool_use",id,name,input}` 块 + `stop_reason:"tool_use"` |

上游事件对应：`content_block_start(type=tool_use)` 给 id/name，`content_block_delta(input_json_delta)` 给
`partial_json` 增量，按 `index` 累积。tool_result 回传：chat 用 `role:"tool"`+`tool_call_id`，
messages 用 `{type:"tool_result",tool_use_id}`，responses 用 `function_call` + `function_call_output` 输入项。
三协议多轮闭环均已实测。

### 结构化输出与 tool_choice

- `/v1/responses` 的 `text.format`（`json_schema` / `json_object`）转成提示词约束注入 system
  追加段（上游无原生 structured output）。实测 `json_schema` 返回合法 JSON。
- `tool_choice` 映射：`"auto"`→`{"type":"auto"}`、`"required"`→`{"type":"any"}`、
  `{"type":"function","function":{"name":X}}`→`{"type":"tool","name":X}`、`"none"` 不发。
- **上游限制**：zcode-plan 端点忽略 `tool_choice`（`type:tool` / `type:any` / 不传三种均返回纯文本
  `end_turn`），无法强制模型调用指定工具；网关侧转换正确，属上游行为。
- 未实现：`previous_response_id` / `conversation`（有状态会话，本网关无会话存储）。

## 响应

非流式（stream:false）返回完整 JSON：content 数组含 text/thinking 块，usage.input_tokens/output_tokens 为真实值。
流式为 SSE：message_start → content_block_start → content_block_delta(text_delta|thinking_delta) → message_delta(stop_reason, usage) → message_stop。

## 错误码

| 状态 | code | 含义 |
|---|---|---|
| 400 | 1210 | thinking 格式错（已过风控）：显式 thinking.disabled 无 tools |
| 400 | 3001 | parameter error：billing 接口缺 Content-Type 或 X-Device-Mid |
| 401 | - | zcodejwttoken 过期或无效（空 body） |
| 405 | 3012 | system 白名单前缀失效 |
| 429 | 1113 | 限流/额度（付费侧） |
| - | 1005 | Start Plan 额度耗尽（不重试） |
| - | 3007 | 安全验证被拒（阿里云挑战） |
| - | 3008/3009/3010 | 并发上限（"系统繁忙"） |

## 额度结构

billing/balance 返回 plans[]（entitlements[].capabilities = 模型白名单）+ balances[]（total_units/used_units/remaining_units/expires_at）。
实测样本 plan_id=zcode-v3-start-plan-1024：GLM-5.3-Flash 300M、GLM-5.3 200M，一天期。
计量注意：2.7MB/1412 条消息的抓包原样重放累计消耗约 70M 单位。

## 消融记录

| # | 变量 | 结果 |
|---|---|---|
| B0-B4 | 极简 body（无 system）+ 头变体 | 全 405/3012 |
| B5 | oauth:bigmodel:access_token 打 messages | 401（token 不通用） |
| C1 | 极简 body + 抓包 device_id | 405（device 不绑） |
| D1 | 原 system+tools + 最后 10 条 messages | 200（messages 历史无关） |
| D2 | D1 去 tools | 400/1210（过风控） |
| D3 | D2 去 system | 405/3012（system 是风控键） |
| D4 | 原 system + 单条消息 | 400/1210（seg2/tools 可省） |
| E1 | seg0+seg1 | 400/1210 |
| E2 | 仅 seg0 | 405/3012（seg1 主体必需） |
| E3 | seg1 末尾改一字符 | 400/1210（非精确哈希） |
| E4 | seg0+seg1+自定义段 | 400/1210（可追加） |
| E5 | 去 cache_control | 400/1210 |
| F1 | output_config.effort=low | 200 纯文本 |
| F2 | thinking.type=high | 400/1210（位置错） |
| F3 | thinking.disabled + 哨兵 tools | 200 |
| F4 | 随机 device_id | 200 |
| F6 | 全剥 X-头 + python UA | 200（头不校验） |
| G1 | effort=low | 200 纯文本 |
| G2/G4 | effort=high / max | 200 带思考块 |
| G3 | 无思考字段 | 200 无思考（API 默认） |

## 凭据

~/.zcode/v2/credentials.json，值 enc:v1:b64url(iv).b64url(tag).b64url(ct)，AES-256-GCM，
key=SHA256("zcode-credential-fallback:{platform}:{home}:{username}")，iv 12B，tag 16B。
zcodejwttoken 亦明文存于 ~/.zcode/v2/config.json 的 provider["builtin:bigmodel-start-plan"].options.apiKey。
zcodejwttoken 短期有效，过期 = 401；重登 ZCode CLI/GUI 刷新，或走 docs/OAUTH.md。

## 官方文档（repo 快照已删，结论留存）

- 两家 Start Plan（zai/bigmodel）intentionally reuse the same zcode-plan runtime endpoint，凭证按 provider id 隔离
- builtinModels[].reasoning：levels.low/high/max → {"path":["output_config","effort"],"value":"..."}
- 业务码 1005/3007/3008/3009/3010 的 UI 语义（额度耗尽不重试 / 安全验证 / 并发上限可短退避）
- metadata.user_id 事实规范：device_id=deviceMid、account_uuid 恒空、session_id 剥 sess_/subagent_agent_ 前缀
- captcha 配置：sceneId=11xygtvd、region=cn、prefix=no8xfe、skip_model_request=true
