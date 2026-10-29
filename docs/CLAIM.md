# ZCode 限时套餐领取（billing/claim）

> 通道索引：**免费模型请求** → PROTOCOL.md ｜ **OAuth 登录** → OAUTH.md ｜ **付费 V4 签名** → PAID.md ｜ **领取协议** → 本文件 ｜ **token 生命周期** → TOKEN.md ｜ **分析报告** → REPORT.md

社区叫「签到」，实际是限时套餐领取：`billing/preview` 列套餐 → `billing/claim` 按优先级领取，
幂等（已领过返回 1003 算成功）。协议来源：社区青龙脚本 L0NE-6/ZCode-Daily 的抓包结论，
2026-10-27 用本机真实 JWT 实测 preview 与事件上报双 200 验证（当时活动无可领套餐，
端点与头组均正确）。Go 实现：`internal/zcode/claim.go`。

**入口由投放驱动，不是 preview**：官方界面的领取按钮由 `GET /api/v1/marketing/touch` 的
`deliveries` 决定（空则无入口），preview 只列「投放生效后能领什么」。`deliveries:[]` 区分不了
「平台无活动」与「有活动但没投给你」。完整投放/回执/能力检查链见知识库
`ZCode/docs/zcode-plan-11-签到领取接口.md`。

## 端点

| 用途 | 端点 | 方法 |
|---|---|---|
| 列可领套餐 | GET https://zcode.z.ai/api/v1/zcode-plan/billing/preview?app_version=3.14.0&platform=win32-x64 | GET |
| 领取 | POST https://zcode.z.ai/api/v1/zcode-plan/billing/claim，body `{"plan_id":"…"}` | POST |
| 激活补报 | POST https://zcode.z.ai/api/v1/event/report（app_launch / app_daily_active） | POST |
| 验证码配置 | GET https://zcode.z.ai/api/v1/client/configs（`data.configs.captcha`） | GET |

注意 `app_version=3.14.0`（领取通道固定 3.14.0，与模型请求侧的 3.16.3 不同）。
国内版/国际版同域名同路径，仅 JWT 签发方不同。

## 请求头

```
Authorization: Bearer <zcodejwttoken>
Content-Type: application/json
X-ZCode-App-Version: 3.14.0
X-Platform: win32-x64
X-Device-Mid: <device_mid>            # 领取通道必需；模型请求端点不校验，这里会 3001
X-Aliyun-Captcha-Verify-Param: <param>      # 仅当验证码开启
X-Aliyun-Captcha-Verify-Region: cn          # 空 region 回落 cn
```

## device_mid 稳定化

**每账号固定 device_mid**（uuid v4，落盘 `zcode_devices.json`，key = sha256(jwt)[:16]）。
换来换去会触发 3001——同一 JWT 始终带同一个 mid，与模型请求侧「任意 uuid 可过」相反。

## 时区与投放窗口

官方活动口径全部按 **北京时间（UTC+8）**，用固定偏移实现（不依赖 IANA tzdb）：

- 投放：每天 UTC 11:00 起 = 北京 19:00 开始可领
- 使用窗口：300M GLM-5.3-Flash 在 UTC 15:00–次日 01:00 = 北京 23:00–次日 09:00

网关自动领取按 19:05 / 20:05 / 21:05 / 22:05 各试一次覆盖投放延迟（`main.go` 的 `claimSlots`）；
claim 幂等，已领过（1003）与无可领套餐只记日志。`ZCODE_AUTO_CLAIM=0` 关闭。

## 验证码（阿里云无痕）

`client/configs` 动态下发 `captcha` 配置（`enabled / sceneId / region / prefix`），场景号动态
防换号失效。开启时 claim 必须带一次性 `captchaVerifyParam`，裸发吃 3007。

- 求解：node 子进程跑 `tools/captcha_solve.js`（happy-dom + 阿里云 SDK 完成 PoW/签名），
  随仓库分发 `tools/AliyunCaptcha.js`；偶发失败重试一次（`internal/zcode/captcha.go`）
- 策略：**先求解后 claim**（避免先裸发一轮吃 3007）；3007 现场重解一次再试——param 一次性，
  每次 claim 必须重新求
- 求解失败不阻断主流程（退回 3007 现场重试的兜底路径）

## 事件上报（领取资格前置）

claim 前补报 `app_launch` / `app_daily_active`（官方客户端启动/日活会报，网关进程替它补）。
`user_id` 从 JWT payload 解（`user_id` 字段，退 `sub`），解不出跳过上报，不阻断领取。

## 错误码

| code | 含义 |
|---|---|
| 0 | 领取成功 |
| 1001 | 没有这个套餐 |
| 1002 | 活动还没开始或已经结束 |
| 1003 | 该账号已经领过这一期（**幂等成功**） |
| 1004 | 当前账号或客户端版本不在活动范围内 |
| 1005 | 今天领取次数已经用完 |
| 3001 | 请求参数不对（device_mid） |
| 3007 | 验证码没通过 |
| 401 | 登录状态失效 |

## 选择与幂等策略

preview 返回的 plans 里取 `priority` 最高的领取（同优先级取最先出现的）。
1003 按成功处理；无可领套餐返回说明不报错。

## 实测（付费侧之外的本通道验证）

- 2026-10-27：preview 200 + 事件上报 200（当时活动无可领套餐，端点/头组/稳定 mid 均正确）
- claim 全链路（含验证码求解）未在真实活动窗口跑过 200——首次真实领取时回填本节
