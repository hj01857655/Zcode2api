# ZCode start-plan 通道协议分析报告

> 通道索引：**免费模型请求** → PROTOCOL.md ｜ **OAuth 登录** → OAUTH.md ｜ **付费 V4 签名** → PAID.md ｜ **领取协议** → CLAIM.md ｜ **token 生命周期** → TOKEN.md ｜ **分析报告** → 本文件

- 目标：`zcode.z.ai` start-plan 通道的完整协议事实（免费模型请求、OAuth、领取），及其在 Go 网关（ZCode2api-go）中的落点
- 结论日期：2026-10-26（模型请求主协议）；2026-10-27（领取通道）；2026-10-28（多号验证）
- 分析对象：ZCode 3.16.3 CLI 真机流量、`~/.zcode` 凭证存储、官方文档快照（repo 已删，结论留存于 PROTOCOL.md 末节）
- 本报告是方法论与证据链叙述；逐字段协议事实以 PROTOCOL.md / OAUTH.md / PAID.md / CLAIM.md 为准

## 1. 方法论

三步法：**真机抓包 → 原样重放 → 消融矩阵**。

1. **抓包**：ZCode 3.16.3 CLI 真机运行，落盘完整请求原文（Python 版仓库 `req_dump.log`，
   约 39KB）与 model-io rollout（`~/.zcode/cli/rollout/model-io-*.jsonl`，提供 4 个 surface 变体的
   官方 system 全文）。抓包文件只作证据留存，其中的 token 视为敏感值。
2. **重放**：2.7MB / 1412 条消息的抓包 body 原样重放 → 200。证明请求可在客户端外复现，
   同时暴露计量事实：这一轮重放累计消耗约 70M 单位（额度结构见 PROTOCOL.md「额度结构」）。
3. **消融**：26 发受控变量实验（B/C/D/E/F/G 组），每次只动一个变量，用错误码作判据。
   完整矩阵在 PROTOCOL.md「消融记录」，下文 §3 只引用编号。

付费侧 V4 签名另行分析：来源为 `zcode.cjs` 构建产物分析 + 第二实现（zcode-signing.ts）交叉
验证，全程见 PAID.md。**构建产物文件本身永不修改**（见 §6 铁律）。

更完整的子系统知识库（48+ 篇，按子系统编号：认证、计费、聊天接口、V4 签名、插件系统、
子代理运行时等）在本工作区上级目录 `../../docs/`（`zcode-plan-01` ~ `48` 系列，入口为同名
总览索引）。本报告只覆盖网关实现所需的通道协议面。

## 2. 五要件（一发 200 的必要条件）

每个要件都用「去掉它会得到哪个错误码」来定义——判据是错误码，不是猜测：

| # | 要件 | 反证（消融编号 → 结果） |
|---|---|---|
| 1 | `Authorization: Bearer <zcodejwttoken>` | 无效/过期 → **401 空 body**；oauth access_token 不通用（B5 → 401） |
| 2 | system 以官方白名单前缀开头（seg0+seg1） | 无 system / 仅 seg0 → **405 + 3012**（B0-B4、D3、E2）；seg1 末尾改一字符可过（E3）→ 前缀匹配非精确哈希 |
| 3 | `model` 用官方大小写 | `GLM-5.3-Flash`、`GLM-5.3` 可用（大小写敏感） |
| 4 | body 结构合法 | thinking 字段位置错（F2 → 400/1210）、显式 disabled 且无 tools（D2 → 400/1210）——已过风控的业务校验 |
| 5 | billing 类端点额外要求 `Content-Type` + `X-Device-Mid` | 缺任一 → **400 + 3001** |

明确**不在**要件里的（全部有反证）：HTTP 头组（F6 全剥 X 头 + python UA → 200）、UA/TLS 指纹
（F6）、device_id（F4 随机 → 200）、metadata.user_id（C1 抓包值可换）、cache_control（E5）、
messages 历史（D1 只发最后 10 条 → 200）。

## 3. 核心结论：Evidence → Finding → Path

| # | Evidence（证据） | Finding（结论） | Path（Go 网关实现落点） |
|---|---|---|---|
| E1 | 消融 D3/E2（system 缺失 → 405/3012）与 D4/E1（保留前缀 → 200） | 风控唯一键 = system 前缀白名单，前缀匹配、可微调可追加 | `internal/zcode/assets/prefix.json`（seg0+seg1 常量）→ `server.go` New() 注入 → `translate.go` 组装 system |
| E2 | E3/E4（改末尾字符、追加自定义段 → 200） | 非精确哈希，白名单按前缀匹配 | 同上；自定义段由 `translate.go` 追加 |
| E3 | F6（全剥 X 头 + python UA → 200）、F4（随机 device_id → 200） | 模型请求端点不校验任何头与设备指纹 | `client.go` Headers() 保持极简 4 头（UA 伪装段仅是惯性，非必需） |
| E4 | 非流式 JSON 上游回 br 压缩，零依赖 Go 侧不解压 | 统一走流式聚合：上游恒 `stream:true`，非流式由网关聚合 SSE | `client.go` Messages() 注释 + `server.go` 聚合逻辑 |
| E5 | 工具调用三协议闭环实测（PROTOCOL.md「工具调用」表） | 上游是 Anthropic SSE，三协议差异全部在网关转换层 | `server/anthropic.go`（透传）、`server/responses.go`（Responses 形状）、`zcode/translate.go`（chat/messages ↔ 上游） |
| E6 | tool_choice 三种取值均返回纯文本 end_turn | zcode-plan 端点忽略 tool_choice，属上游行为 | `translate.go` 转换正确实现，限制记录于 PROTOCOL.md |
| E7 | 凭证文件分析（PROTOCOL.md「凭据」节） | credentials.json = `enc:v1` AES-256-GCM，key 派生自本机路径+用户名；同一 token 在 config.json 有明文副本 | `zcode/auth.go` credentialsKeyFor()/sealCredential()；Go 网关读明文副本 |
| E8 | OAuth init/poll 真机抓包（OAUTH.md 实测节） | ready 的 data.token 即 zcodejwttoken，无二次交换；2026-09 起 poll 必须用服务端下发的 poll_token | `zcode/auth.go` DeviceFlow.Login()（auth_test.go 离线覆盖） |
| E9 | 领取通道抓包结论 + 本机 JWT 实测（CLAIM.md） | 领取是独立协议面：稳定 device_mid、动态验证码、幂等 claim | `zcode/claim.go`、`zcode/captcha.go`、`handler/claim.go`、`main.go` claimSlots |
| E10 | 多账号接入复验（2026-10-28，PROGRESS.md） | 新账号同一套风控（system 白名单），头/UA 依旧不校验 | `zcode/pool.go`（选号/冷却）+ `client.go` cooldownFor() |

## 4. token 生命周期

token 状态机、来源链、401 处理与冷却策略单独成文 → **docs/TOKEN.md**（含 Mermaid 状态图）。

## 5. 风险与边界

- **协议会漂移**：白名单前缀、端点、错误码均以当次实测为准；ZCode 大版本更新后前缀需重新提取
  （`assets/prefix.json` 注明 extracted_at）。备选前缀（model-io 完整版 3 段 8599c）见
  PROTOCOL.md「备选 system 前缀」。
- **计量代价**：重放与消融消耗真实额度（§1 第 2 步一轮 ≈70M 单位）；重复实验前先查
  `/v1/balance`。
- **敏感值**：抓包原文、jwt_override.txt、token.json 均含明文 token——已入 .gitignore，
  引用时一律脱敏。

## 6. 铁律

**ZCode 的安装包文件（zcode.cjs / app.asar）只分析、永不修改**：改了必被服务端 prompt 风控
硬拒（3012 / unusual activity，实测记录于 Agent/ZCode/zcode-purge docs/01 第五节）。
所有客户端定制只走官方通道文件（AGENTS.md / output-styles / skills / memories / hooks）。
本仓库所有工作都在协议面（HTTP 请求形状与凭证读取），不触碰安装包。

## 7. 复现

```bash
# 消融矩阵（Python 版仓库，需有效 token）
cd ../ZCode2api && python exp/ablation.py        # A1 200 / A2 405+3012 / B1-B3 档位

# Go 网关离线测试（不依赖真实 token）
cd ../ZCode2api-go && go test ./...

# 网关端到端
./zcode2api-go.exe login zai && ./zcode2api-go.exe
curl http://127.0.0.1:3010/health
```
