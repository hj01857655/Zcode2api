# Super-agent 架构落地方案：ZCode 生态的 Router/Worker 双层实现

> 基于 DeepSeek Harness「Super-agent 设计探索」（2026-11-05 公众号）的架构思路，
> 落到我们已有的双项目资产上：zcode-hub（Tauri 桌面端 + 本地网关）与
> ZCode2api-go（服务器多账号池网关）。不是重新造轮子——是把已有的池/台账/
> 熔断升级成事件驱动的 Super-agent 骨架。

## -1. 官方原型已开源：deepseek-ai/dsh-superagent-prototype

公众号文章发布同日，官方把内部原型开源了（TypeScript + DeepSeek Harness
插件形态，README 明示 research prototype 非产品）。与文章五模块的对应与差异：

| 文章概念 | 原型实现 | 借鉴价值 |
|---|---|---|
| Router 层 | 主 Session（`dsh-superagent-router` preset，小工具集）+ **Assessor**（Eval 评估器，独立裁判） | 我们方案没有 Assessor——「要不要行动」直接 Router 一锤，官方拆成独立 Evals 部件（batching/deadline/replay），更干净 |
| Worker 层 | Executor Session（`worker-<ticket>`，每 Ticket 独立目录 + git worktree prepare） | 与我们 Worker 层同构；目录/worktree 隔离做法直接抄 |
| 事件驱动 | Source 插件（GitHub/Feishu）→ Trigger → Engine.Intake → fan-out | 接入形态比文章更具体：Trigger 入库、 disposition 记录、订阅 fan-out |
| 两级过滤 | admission.md prompt（Slow Path）+ 程序层白名单 | 同构 |
| 投机执行 | 文章 §3.4 **原型未实现**（README 明说部分设计是 future work） | 无需对齐 |
| Outbox/Passes | 单事务写 receipts + 每 2s pass 泵（Effect 在事务内入队、commit 后执行） | 经典 outbox 模式，可白嫖 |

**哲学篇（000）的核心原则值得全文照抄进我们的 Router prompt**：
「占用尽可能少的人类注意力，给出尽可能多的价值」——只在被要求或书面规则
允许时行动；只在人必须行动或能用结果时说话；一次只问一个具体问题；
按影响而非努力停止；诚实汇报。责任条款：从窄白名单起步、主动工作先只记录。

**实现启示修正**：官方原型跑在 DSH 插件进程里（pnpm + profile.yml patch），
不是独立 daemon——我们的形态（2api 独立进程 + hub 桌面端）反而更接近
文章的「计算/状态/环境分离」，移植成本比预想低。

## 0. 现状盘点：我们离 Super-agent 有多远

| DeepSeek 特征 | 我们已有的 | 缺口 |
|---|---|---|
| **连续**（跨信道同一 Agent） | hub 网关（OpenAI/Anthropic/Responses 三协议）已是对话入口 | 入口只有 API，无 IM/GitHub 接入 |
| **高效**（多任务并发） | 14 号池轮转 + sticky 亲和 + LRU，实测 ~14 路并发 | 并发已在，但任务编排靠人 |
| **主动**（事件驱动） | 19:05 四轮定时 touch 轮询（轮询 ≠ 事件驱动） | 无 Trigger 订阅、无 Fast/Slow Path |
| **持久**（跨设备在线 + 自愈） | 服务器 ECS 常驻 + 401 热重读 + 熔断双闸 + 台账持久化 | **基本达标**，缺 Watchdog 主动巡检 |

结论：**池与自愈已经是 Super-agent 的 Worker 层底座**。缺的是 Router 层（任务
分派 + 跨信道交互）和事件驱动（Trigger 订阅 + 两级过滤）。这两块是新增量。

## 1. 总体架构

```
                         ┌─────────────────────────────┐
   Trigger 源            │      Router 层（新增）        │
   ┌──────────┐          │  常驻会话（GLM-5.3-Flash）    │
   │ IM 群聊   │──msg──▶  │  · Trigger 两级过滤           │
   │ GitHub PR│──event─▶ │  · 任务分派 → Worker          │
   │ touch 投放│──signal─▶│  · 对外统一回复               │
   │ 余额/台账 │──cron───▶│  · 轻量模型，不配 Bash        │
   └──────────┘          └──────────┬──────────────────┘
                                    │ 任务分派（Thread 订阅直通）
                         ┌──────────▼──────────────────┐
                         │      Worker 层（已有底座）    │
                         │  · 2api 3011（14 号池）       │
                         │  · hub 网关 8081（池+台账）    │
                         │  · claim 链路（touch→captcha  │
                         │    →claim→report）            │
                         │  · 全量工具：git/curl/脚本     │
                         └─────────────────────────────┘
```

## 2. 模块拆解

### 2.1 Router 层（新增，go 项目内嵌或独立进程）

- **实现形态**：`internal/router/`——单常驻 LLM 会话，模型走自家网关
  （GLM-5.3-Flash，便宜且够用），**不配 Bash**，只有三个工具：
  `dispatch(task, worker)` / `reply(channel, text)` / `wait(event)`；
- **两级过滤**（Fast/Slow Path）：
  - Fast Path（确定性代码）：来源白名单、群组范围、关键词匹配——
    「领取成功」「池不可用」「CI 失败」这类已知信号直接走固定动作；
  - Slow Path（LLM 语义判断）：通过 Fast Path 的候选才进 Router 会话；
- **批处理**：推理期间积压的 Trigger 在下一轮合并注入（DeepSeek 实测
  积压越多单决定成本越低），我们 14 号池的吞吐足够。

### 2.2 Trigger 源接入（按收益排序）

| Trigger | 接入方式 | 触发动作 | 优先级 |
|---|---|---|---|
| **touch 投放出现** | 定时 5min 轮询 touch（已是现成轮询，加信号量判断） | 立即抢批（替代 19:05 整点等投） | P0 |
| **池健康异常** | `/health` 熔断/全冷却信号 | Router 通知 IM + 尝试自愈 | P0 |
| **领取结果** | 台账/日志尾部 tail | IM 播报（成功/失败/额度） | P1 |
| **IM 群聊指令** | 企业微信/飞书 bot webhook | 远程指挥：查余额/手动领取/重启 | P1 |
| **GitHub PR/CI** | gh CLI 轮询或 webhook | 评审评论自动回复、失败自动定位 | P2 |

### 2.3 Worker 层（已有底座，补两个口子）

- **任务队列**：Router 分派的任务落 `internal/worker/queue.jsonl`，
  Worker 按优先级消费（抢批任务优先级最高，插队）；
- **状态上报**：Worker 每步结束向 Router 发结构化事件（现成的
  `RequestLog` 形状复用），Router 决定是否对外播报。

### 2.4 Watchdog（补齐「持久」的最后一块）

- 独立 goroutine 每 10min 巡检：进程存活 / 池可用 / 磁盘 / 上游连通；
- 异常分级：可自愈（重启 worker）→ 自愈并通知 / 不可自愈（IM 告警人）。

## 3. 分期路线

**第一期（P0，2-3 天）：事件驱动抢批 —— 已落地（2026-11-07）**
- touch 5min 轮询 + 投放信号量 → 触发立即抢批（台账防重 + 有界并发 claim）；
  实现：`internal/handler/watch.go`（detectDelivery 全池逐号 400ms 错峰轮询 →
  AllClaimedToday 台账跳过 → ClaimAllConcurrent 抢批 → claimBatchCooldown 10min 静默）；
- 领取结果 IM 播报：`internal/handler/notify.go` 企业微信群机器人 webhook
  （`ZCODE_WEIXIN_WORK_WEBHOOK`，text 形态 600 rune 截断）；
- 并发口径：默认 4（claim 限流是出口 IP 级，连领 4 个后第 5 个必撞——
  全池齐发只会把批次推进 429 重试波次，有界并发命中率同串行而墙钟 ≈1/4）；
- 环境：`ZCODE_CLAIM_WATCH=0` 单独关监视 / `ZCODE_WATCH_INTERVAL=300` /
  `ZCODE_CLAIM_PARALLEL=4` / `ZCODE_WEIXIN_WORK_WEBHOOK`；
- 固定 slot（19:05/20:05/21:05/22:05）保留兜底，claim/台账幂等两者不冲突；
- 收益：新批次投放瞬间开始抢，不再等 19:05 整点——**11-03 批次如果有
  这个，9 个号都能抢到**。

**第二期（P1，1 周）**：Router 会话 + IM 指挥
- `internal/router/` 常驻会话 + Fast/Slow Path 过滤器；
- IM bot 双向：被动接收指令（查余额/手动领取/切上游）+ 主动播报；
- Watchdog 巡检。

**第三期（P2，按需）**：GitHub 集成
- PR 评审/CI Trigger 接入，Worker 直接提交修复 PR；
- 多信道连续性（IM 里说方案 A、PR 里改方案 B，Router 统一上下文）。

## 4. 与 DeepSeek 方案的差异点（我们的优势）

1. **模型成本**：Router 用自家池的 GLM-5.3-Flash（免费额度），Worker 也是
   池内资源——Super-agent 跑起来零边际成本；
2. **风控经验**：UPSTREAM-3012 的复合门结论、版本档灰度、台账防重——
   这些是 DeepSeek 文章没有的实战层；
3. **已有双端**：不需要从零搭 Router/Worker 的物理载体，hub（本机）+
   2api（服务器）天然就是两层架构的现成节点。

## 5. 风险与边界

- IM bot 权限：企业微信/飞书自建应用需要管理员审批，个人钉钉/Telegram
  bot 是零审批替代；
- 抢批并发：全池并发 claim 会短暂触发上游并发限制（3008），已有全局
  退避闸兜底——投放窗口的收益远大于冷却代价；
- Router 会话膨胀：长会话上下文管理照抄 DeepSeek 的 Inline & Spill 思路
  （高频事实内联、低频外存按需召回），第二期再实现。
