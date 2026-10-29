# ZCode 付费通道（coding-plan + V4 签名）

> 三通道索引：**免费模型请求** → PROTOCOL.md ｜ **OAuth 登录** → OAUTH.md ｜ **付费 coding-plan + V4 签名** → PAID.md
> 配套：**领取协议** → CLAIM.md ｜ **token 生命周期** → TOKEN.md ｜ **协议分析报告** → REPORT.md

> 三条通道之二。另见 docs/PROTOCOL.md（start-plan 模型请求）、docs/OAUTH.md（OAuth 登录）。
> V4 签名官方未开源；以下取自 zcode.cjs 构建产物分析 + 第二实现（zcode-signing.ts）交叉验证。

## 通道定位

| | start-plan（免费） | coding-plan（付费） |
|---|---|---|
| 凭证 | zcodejwttoken，裸 Bearer | api-key 形如 `{apiKey}.{secret}` |
| 签名 | 无 | V4（远端开关 + Ed25519） |
| 端点 | zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages | 同路径 |
| 额度 | 免费桶（capability 白名单） | 订阅额度，超限 429/1113 |
| 网关实现 | 支持（server.py / zcode2api-go） | 未接入 |

## 凭证获取（两级交换）

```
POST api.z.ai/api/auth/z/login            {token: <oauth access_token>} → biz_token
GET  api/biz/customer/getCustomerInfo                                   → organizationId / projectId
GET  api/biz/v1/organization/{org}/projects/{proj}/api_keys             → apiKey
POST .../api_keys/{apiKeyId}/access_tokens  {clientType:"zcode", clientVersion} → accessToken
GET  .../api_keys/{apiKeyId}/copy/{apiKey}                              → secretKey
凭证 = "{apiKey}.{secretKey}"
```

## V4 签名协议

### 常量

```
握手路径   /api/paas/c1f3a7e2/v2/client      （origin 必须是 api.z.ai，zcode.z.ai 上 404）
握手 origin https://api.z.ai                 业务 origin https://zcode.z.ai
签名域分离 get_sign_key                      appId "zcode" → X-App-Id
HKDF salt  WD_CLIENT_SIGN_KDF_SALT
HKDF info  getSignKey_hmac（握手）/ ed25519_priv（私钥解密）
nonce      握手 16 字节 → 32 hex；PoW candidate 前缀 12 字节
PoW 难度   8 前导零 bit（candidate 首字节为 0）
握手超时   10s
```

### 握手（换一次性 Ed25519 私钥）

```
nonce = random_hex(16)
ts    = String(Date.now())
sig   = base64(HMAC-SHA256(HKDF-SHA256(secret, salt, info="getSignKey_hmac"),
                           "get_sign_key\n{apiKeyId}\n{ts}\n{nonce}"))

POST https://api.z.ai/api/paas/c1f3a7e2/v2/client
Authorization: <credential>
body: {apiKey, nonce, sig, ts}
→ 返回加密私钥

解密：key2 = HKDF-SHA256(secret, salt, info="ed25519_priv")
      iv = cipher[0:12], AAD = apiKeyId
      privateKey = PKCS8(AES-256-GCM 解密 cipher[12:])
```

### 请求签名

```
powSeed   = sha256("{apiKeyId}\n{appId}\n{sessionId}\n{ts}")[:32]
prefix    = random_hex(12)
candidate = prefix + counter:08x        // 满足 sha256("{powSeed}\n{candidate}") 首字节为 0

sigMsg = "{apiKeyId}\n{ts}\n{clientVersion}\n{sessionId}\n{nonce}"
sig    = base64(Ed25519.sign(privateKey, sigMsg))
```

附加头（发送前先剥离旧签名头，含 `X-Client-Sign-Verified`）：

| 头 | 值 |
|---|---|
| X-Client-Ts | 毫秒时间戳 |
| X-Client-Version | 客户端版本 |
| X-Client-Sig | Ed25519 签名 base64 |
| X-Session-Id | 会话 id（缺失抛 invalid-config） |
| X-Client-Nonce | 32 hex |
| X-App-Id | zcode |
| X-Client-Pow | PoW candidate |

### 失败与旁路

- 远端 feature gate 每 key 独立判定；未启用 → 不签名直发
- 请求 origin ≠ 握手 businessOrigin → `origin_mismatch`，明文发送
- 401 且 reason ∈ {VERIFY_SIGNATURE_INVALID, VERIFY_APIKEY_EXPIRED} → 作废私钥（epoch+1）→ 重新握手 → 重试一次
- 握手失败属 failOpenEligible → 明文发送（getKeyOrSendUnsigned）

## 实测（付费侧）

- 有 api-key 但无订阅额度 → 429 / code 1113
- 与 start-plan 共用 messages 端点；付费侧同样受 system 白名单约束（未单独验证）
