# 上游 3012 判定矩阵（messages 推理通道）

`POST https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages` 返回 HTTP 405 + `{"code":3012,"msg":"request has been blocked due to unusual activity"}` 时，存在**两条独立触发分支**，任一命中即拒。2026-11-04/05 十一发对照矩阵实测锤定。

## 十一发矩阵（版本号 × 头形态 × system）

| # | system | 头形态 | UA/版本头 | 出口 | 结果 |
|---|---|---|---|---|---|
| 1 | 种子伪三块 | 满配伪装头 | 3.17.2 | 本机 | 3012 |
| 2 | 官方全文三块 | 满配伪装头 | 3.17.2 | 本机 | 3012 |
| 3 | seg0+seg1 | 满配伪装头 | 3.17.2 | 本机 | 3012 |
| 4 | seg0+seg1 | 满配伪装头 | 3.17.2 | 服务器 | 3012 |
| 5 | seg0+seg1 | 四头极简 | 3.16.3 | 本机 | **200** |
| 6a | 官方全文三块 | 四头极简 | 3.16.3 | 本机 | **200** |
| 6b | 种子伪三块 | 四头极简 | 3.16.3 | 本机 | 3012 |
| 7 | seg0+seg1 | 满配伪装头 | **3.16.3** | 本机 | **200** |
| 8 | seg0+seg1 | 满配伪装头 | **3.14.0** | 本机 | **200** |
| 9 | seg0+seg1 | 四头极简 | 3.17.2 | 本机 | 3012 |
| 10 | seg0+seg1 | 四头极简 | **3.14.0** | 本机 | **200** |

## 分支一：版本档（本环境主因，2026-11-05 修正归因）

**声明 3.17.2 → 必 3012；声明 3.16.3 / 3.14.0 → 必 200，与头形态无关**（#7/#8/#10 满头也 200，#9 四头也 3012）。头集合满配/极简均可行。

机制：3.17.2 是强制客户端签名/PoW 的版本档（见 docs APP-VERSION / 客户端签名记录）——声明 3.17.2 却不带签名头，统一落 3012。要过 3.17.2 档只有两条路：补齐签名/PoW 头，或钉旧版本档。**生产选择钉 3.16.3**（`internal/zcode/client.go` Headers：UA `ZCode/3.16.3 ai-sdk/anthropic/3.0.81`）。

注意：版本档黑名单是服务端动态灰度的（外部环境 3.17.2 曾有 200 记录，2026-11-04 深夜起我们环境 3.17.2 全拒）。若未来 3.16.3 也被清，按预案补签名或降档重测。

## 分支二：system 模板指纹

system 须为 **2–3 个 text 块数组**（分支一排掉后才轮到它）：

- 块0 = 官方模板，字节级一致，必须在首位
- 块1 = 官方模板前缀（开头一致，可在末尾追加）
- 用户 system 内容：合并成一块追加在后（网关 `InjectSystemPrefix` 的做法）
- ≥4 块、篡改块0/块1 开头、用户内容前置 → 3012

白名单含**多代模板**（均实测 200）：

| 代际 | 块0 | 状态 |
|---|---|---|
| 旧代 | `You are ZCode, an interactive coding agent`（42 字符） | 生产在用（`internal/zcode/assets/prefix.json` seg0+seg1），2026-11-04 实测 200 |
| 新代 | `You are ZCode, an interactive agent`（35 字） | 3.17.2 客户端（zcode.cjs 2026-11-03）已只含此代，旧代从代码删除 |

cjs 里的模板常量只有种子短句（97/94 字），全文是运行时拼接的——**提取模板以 dump 为准，不以 cjs 为准**。

## 3012 排查顺序

1. 先看版本号：声明 3.17.2 且没带签名头？→ 换 3.16.3（或补签名/PoW）
2. 再对 system：块0 字节级？块1 官方前缀？块数 ≤3？dump 提取全文对照
3. 最后才考虑换出口 IP

## 预案：上游收紧白名单只留新代时

1. 确认 `~/.zcode/v2/setting.json` `"modelIoFullRetentionEnabled": true`（默认 false，关着 dump 不含 system——这是"重放官方请求仍 3012、无法重建"的根因）
2. 升级官方客户端到 3.17.x 后，从 `~/.zcode/cli/rollout/model-io-*.jsonl` 头部提取新代三块全文
3. 替换 `internal/zcode/assets/prefix.json` 的 `system_prefix`（前缀语义：seg1 取官方块1 开头截断即可），重编重发

## 2026-11-05 对照实验：官方三块（新代）实测 3012

同头同版本（3.16.3 极简头族、同一账号同一出口）双发对照：

| system | 结果 |
|---|---|
| A：现行 seg0+seg1（旧代 42 字块0） | **200** |
| B：官方三块全文（新代 35 字块0，dump `model-io-sess_5f8ec065` 提取） | **3012** |

**结论**：新代三块模板**不在当前白名单内**——白名单按版本档灰度而非最新档，
现行 prefix.json（旧代）继续留守，不替换。dump 已留 `official_system_3block.json`
（工作区，不入库），上游若切新代白名单按 §预案 走。

### 交叉项补测（2026-11-05）：矩阵四格全满

3.17.2 版本头 × 旧代模板（3101 全文版 prefix），满头/四头各一发，均 **3012**。
与实验 A/B、本次矩阵合成完整四象限：

| 版本档 \ 模板 | 旧代（42 字块0） | 新代（35 字块0） |
|---|---|---|
| 3.16.3（免签） | **200**（生产） | 3012（实验 B） |
| 3.17.2（签名档） | **3012**（本次双发） | 200（外部环境） |

最终结构：**两道门串联**——第一道版本档（3.17.2 无签名一票否决，模板无关）；
第二道模板白名单（各档只认自己注册的代际：3.16.3 档认旧代，3.17.2 档认新代）。
免签可走的组合只有 3.16.3 × 旧代，即现行生产形态；要进 3.17.2 档只有补签名/PoW 一条路。

**补充（同日决定性对照）**：新代三块 × 3.14.0 头（矩阵 #10 同款头，旧代时 200）→ **仍 3012**。
排除版本头变量——3012 由 system 指纹内容触发：新代三块整体不在白名单，与头形态/版本档无关。

## 2026-11-05 复验：完整三块（块2=3833）仍 3012——与 XinOrigin 报告矛盾未收口

cv-superding/zcode-auto-resume#1 评论（XinOrigin，2026-11-04）：官方三块 + 3.17.2 头无签名 → 200，
8 账号全过。我们用块2 完整版（3833 字，dump sess_9ef3d2b1 提取）双发复验：

| 组 | 头 | 结果 |
|---|---|---|
| 完整三块 × 3.16.3 极简头 | 同前 3012 那发 | **3012** |
| 完整三块 × 3.17.2 满配头族（无签名） | XinOrigin 同款 | **3012** |

排除项：块2 截断（已换 3833 完整版）、版本头、头形态。仍未复现 200，待排查差异维度：
①三块全文逐字节差异（他块1 3115 字 vs 我们 3061/3119——可能存在多版本块1）；
②出口 IP 段（他 macOS 26 vs 我们 win32-x64 自述，或 IP 信誉）；
③ JWT 代际（他是桌面端导出，我们 farm 号库同源，理应一致）。
结论维持：生产留守旧代 seg0+seg1（稳定 200）；新代三块不替换。

**原样重放（2026-11-05 00:25 CST）**：按 XinOrigin body 原样（两块=块0+块1、stream=true、
3.17.2 满配头族）Python 重放 → **3012**。同账号同源 JWT、同出口代理，排除 body 形状
（两块/三块、stream 与否）与头族差异后仍不复现。剩余唯一可辨维度：**出口 IP 信誉**
（报告复现环境为 macOS 26 家宽，我们走数据中心代理）与 JWT 所在号段的服务端画像。
结论最终维持：3012 的门=版本档×模板代际×出口环境三者复合，非单一指纹规则；
生产留守旧代 seg0+seg1。

**终局排除（2026-11-05 00:27 CST）**：XinOrigin 头族原样（darwin-arm64/macos/Darwin 25.0）、
块1 空格开头形态（3119 字变体）、messages.content 数组形态——三发全部 **3012**。
body/头族两侧可枚举变量穷尽，矛盾定性：**出口环境维度**（他家宽 IP + 桌面端导出 JWT 的
设备指纹画像 vs 我们数据中心代理 + farm 号池），非请求内容可修复项。
XinOrigin 的「指纹校验与 IP/账号无关」结论在他的环境成立，在我们环境不成立——
门是复合的：版本档 × 模板代际 × 出口画像。生产留守旧代 seg0+seg1（本环境稳定 200）。

## 3.17.2 签名子系统全貌（2026-11-05 完整逆向 + Python 复现实测）

依据本机安装的 zcode.cjs（15.4MB，`OfficialRequestSecurity` @ offset ~3860000–3876000，
只读取证）逐函数逆向，Python 全链实现并实测到凭据层。原文函数名→语义对照在括号内。

### 凭据解析（nPr parseClientSigningCredential）

输入 apiKey 字符串，二选一：

1. **3 段 JWT**：header 须 `{alg:"HS256", typ:"JWT", sign_type:"SIGN"}`、payload 须
   `{token_use:"project_access", api_key:"非空"}`（zod `m.literal` 严格校验，任一不符整体失败）。
   通过后 `apiKeyId=payload.api_key`，`secret=credential=整个 JWT 串`，握手字段名 `accessToken`。
2. **两段式 `id.secret`**（恰好一个 `.`）：`apiKeyId=id`，`secret=secret 段`，`credential=全串`，握手字段名 `apiKey`。

**farm 登录 JWT（header 无 sign_type，payload iat/sub/token_version/user_id）过不了 schema**
——官方客户端根本不会用它走签名。实测见下文握手节。

### KDF 与加密原语（Coo 模块）

- `deriveBytes(secret, info)`：HKDF-SHA256，salt=**`WD_CLIENT_SIGN_KDF_SALT`**，256 bits。
  info 按用途：握手 HMAC key=`getSignKey_hmac`，私钥解密 AES key=`ed25519_priv`。
- 握手签名（Soo）：`base64(HMAC-SHA256(key, "get_sign_key\n{apiKeyId}\n{ts}\n{nonce}"))`。
- 私钥解密（woo）：`privateCipher`=base64(IV12 ‖ ciphertext ‖ TAG16)，AES-256-GCM，
  **additionalData=apiKeyId（UTF8）**，明文=base64(PKCS8 Ed25519 私钥)。
- 业务签名（koo）：`base64(Ed25519.sign(UTF8(sigMsg)))`，
  sigMsg=`{apiKeyId}\n{ts}\n{clientVersion}\n{sessionId}\n{nonce}`。
- PoW（xoo）：seed=sha256(`{apiKeyId}\nzcode\n{sessionId}\n{ts}`).hex()[:32]；
  candidate=24hex 随机前缀+8hex 计数器，穷举至 sha256(seed+candidate) 前 powBits 位为 0
  （默认 powBits=8，参数化 0–32）；X-Client-Pow=candidate。
- nonce=16 随机字节的 hex（32 字符）；ts=毫秒字符串；appId=`zcode`。

### 握手（performHandshake）

`POST {业务origin}/api/paas/c1f3a7e2/v2/client`，body `{[handshakeField]: credential, nonce, sig, ts}`，
头 `Authorization: <credential>`（raw，无 Bearer），超时 10s。
响应信封 `{code, msg, data:{privateCipher}}`；code=500 特判，非 200 取 `HANDSHAKE_*` reason。
私钥仅内存持有（keyState），两次 VERIFY 拒绝后刷新握手，第三次失败进 `bypassSigning` 永久免签（failOpen）。

**实测（2026-11-05，farm JWT + 正确按上述参数计算的 sig）**：

| 目标 | 结果 |
|---|---|
| `https://zcode.z.ai/api/paas/c1f3a7e2/v2/client` | HTTP 404 `404 page not found`（路由不存在） |
| `https://api.z.ai/api/paas/c1f3a7e2/v2/client` | HTTP 200 `{"code":401,"msg":"token expired or incorrect"}` |
| 同上 + 坏 sig（全 0） | 同 401，逐字节同响应 |
| 同上 + apiKeyId 换 user_id | 同 401 |

归因：A/B/C 三发 sig 内容完全不影响响应 → **凭据层先拒，sig 校验未轮到**。
握手端点实际活在 **api.z.ai**；`JVs` 按 baseURL origin 拼路径，官方 3.17.2 客户端的
业务 origin 若为 zcode.z.ai 则其握手也该 404——说明强签流量（coding plan / chatglm 档）
的业务 origin 并非 zcode.z.ai，或网关按域灰度路由。zcode.chatglm.site / api.chatglm.site
对本机 Python urllib 直接断 TLS（UNEXPECTED_EOF，疑似 TLS 指纹防护），未测到路由层。

### 业务请求头（sendSigned）

请求须自带 `X-Session-Id`（CLI 侧 session id，`sess_` 前缀，格式 `^[A-Za-z0-9_.:-]{1,128}$`），
否则直接抛 invalid-config 不发。七件签名头：
`X-Client-Ts`、`X-Client-Version`（取 provider 头 `X-ZCode-App-Version`，缺省=构建版本）、
`X-Client-Sig`、`X-Session-Id`、`X-Client-Nonce`、`X-App-Id: zcode`、`X-Client-Pow`。
sanitize 列表（重发前剥离）：X-Client-Ts/Version/Sig/Nonce/Pow/App-Id/Sign-Verified。

### 触发判定（YVs requiresClientRequestSigning）

- **免签**：zhipu-account + mode ∈ {start-plan, off-peak, highspeed}（我们的 farm 号在此档）
- **强签**：zhipu-coding-plan-api-key；zhipu-account + mode ∈ {individual-coding-plan, team-coding-plan}；
  baseURL host ∈ {api.chatglm.site, zcode.chatglm.site}
- messages baseURL 官方配置：`${origin}/api/v1/zcode-plan/anthropic`

### Python 参考实现（自洽可跑，凭据换 project_access JWT 即可端到端）

```python
# 依赖：cryptography>=42；凭据：任何满足 nPr schema 的 JWT 或 id.secret
import base64, hashlib, hmac, secrets, time
from cryptography.hazmat.primitives.kdf.hkdf import HKDF
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

KDF_SALT = b"WD_CLIENT_SIGN_KDF_SALT"
APP_ID, POW_BITS, VERSION = "zcode", 8, "3.17.2"

def hkdf(secret: bytes, info: bytes) -> bytes:
    return HKDF(algorithm=hashes.SHA256(), length=32, salt=KDF_SALT, info=info).derive(secret)

def handshake_sig(secret: str, api_key_id: str, ts: str, nonce: str) -> str:
    key = hkdf(secret.encode(), b"getSignKey_hmac")
    msg = f"get_sign_key\n{api_key_id}\n{ts}\n{nonce}".encode()
    return base64.b64encode(hmac.new(key, msg, hashlib.sha256).digest()).decode()

def decrypt_private_key(secret: str, api_key_id: str, private_cipher: str):
    c = base64.b64decode(private_cipher)
    aes = AESGCM(hkdf(secret.encode(), b"ed25519_priv"))
    pkcs8 = base64.b64decode(AESGCM.decrypt(aes, c[:12], c[12:], api_key_id.encode()).decode())
    return serialization.load_der_private_key(pkcs8, None)  # Ed25519PrivateKey

def solve_pow(api_key_id: str, session_id: str, ts: str) -> str:
    seed = hashlib.sha256(f"{api_key_id}\n{APP_ID}\n{session_id}\n{ts}".encode()).hexdigest()[:32]
    prefix = secrets.token_bytes(12).hex()
    for i in range(4294967296):
        cand = f"{prefix}{i:08x}"
        d = hashlib.sha256(f"{seed}{cand}".encode()).digest()
        if d[0] == 0:  # powBits=8：首字节为 0 即 8 个零位
            return cand
    raise RuntimeError("pow unsolvable")

def sign_business(key, api_key_id: str, session_id: str, nonce: str, version: str = VERSION):
    ts = str(int(time.time() * 1000))
    msg = f"{api_key_id}\n{ts}\n{version}\n{session_id}\n{nonce}".encode()
    return ts, base64.b64encode(key.sign(msg)).decode()
```

握手请求：`POST https://api.z.ai/api/paas/c1f3a7e2/v2/client`，
`{"accessToken": <jwt>, "nonce": <32hex>, "sig": <handshake_sig>, "ts": <ms>}`，
头 `Authorization: <jwt>` + `Content-Type: application/json`。
完整探针（含请求发送与脱敏输出）见会话 Temp/sign_probe.py 模式。

### 对本项目的意义

- 生产（3.16.3×旧代，farm 号=Start Plan 免签档）**无需签名**，现状不动。
- 若将来 Start Plan 也被强制签名：签名/PoW 算法层已完全打通（本节代码），
  唯一缺口是 **project_access 代际凭据**（coding plan API key）——farm 登录 JWT 实测不被
  握手端点接受。届时从号库渠道拿 coding-plan-api-key 塞入即可。
- 遇 `{"code":401,"msg":"token expired or incorrect"}`（api.z.ai 握手）→ 凭据代际问题，
  不要再调 sig 算法。
