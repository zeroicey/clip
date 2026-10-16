# 把客户端可控的请求头当作可信输入（本轮出现两次）

**Severity:** 🔴 critical · **First hit:** 2026-10-10 · **Hits since:** 0（已修 + 已加守卫）

同一类错误在一次改动里出现了**两次**，所以记成一条：判断「某个头能不能信」时，
必须看**谁**能把它送到进程，而不是看它**叫什么名字**。

## 实例 1：`Cf-Access-Authenticated-User-Email` = 无条件管理员

`httpx.RequireSession` 里有这么一段：

```go
if email := r.Header.Get("Cf-Access-Authenticated-User-Email"); email != "" {
    s := &Session{Subject: email, Issued: now, Expires: now + 60}
    next.ServeHTTP(w, r.WithContext(WithSession(r.Context(), s)))
    return
}
```

**没有任何来源校验。** 代码注释还写着「Cloudflare Access is not used in the new
deployment, but … still honoured if a reverse proxy is **later** configured」——
那个「later」从来没到，而这段代码一直在生产上跑。

### 生产实测（只读，未做任何写操作）

```bash
$ curl -H 'Cf-Access-Authenticated-User-Email: anyone@example.invalid' \
       https://api.hcyj.xyz/yeciorez/teleport/api/admin/session
HTTP 200
{"ok":true,"data":{"authenticated":true,"subject":"anyone@example.invalid"}}
```

不带头是 401。**任何人都能凭空得到一个管理员会话。**

### 为什么这次才爆

漏洞**一直存在**，但以前伪造会话只能读报告列表、建分享链接。本轮上线的密钥管理面板
把它的后果放大成：批准任意申请 → **直接铸造密钥并把明文 token 交给攻击者** →
读取/撤销全部密钥。**新特性没有引入这个洞，它把一个旧洞接到了危险资产上。**

### 修法

与 `ClientIP` 的信任模型同构，**三道门必须同时成立**，任一不成立就回落 Cookie
（不是 401 —— 合法 Cookie 仍应通过）：

1. `TrustCFAccess`（包级变量）为真；
2. 直接 peer ∈ `trustedProxyPrefixes`（复用 `ClientIP` 的那套）；
3. 值合规：**在原始值上**扫控制字符 → trim → ≤320 字节 → `@` 两侧非空。

并且**刻意不提供环境变量开关**：开启必须改代码重新部署。一个「谁能发头谁就是管理员」
的捷径，不该只隔着一个 env 文件里的拼写错误。启动时 + 首次采用时各打一条 `slog.Warn`。

## 实例 2：`RealIP` 被当成了安全边界

`httpx.RealIP` 的注释自己写着「Used for logging only — **never for authorisation**」，
但公开端点 `POST /api/agent-keys/applications` 的限流（它唯一的减速手段）被挂在了它上面。

它无条件信任 `Cf-Connecting-Ip`，而**线上没有 Cloudflare**（`dig api.hcyj.xyz`
→ `8.148.233.134`，Alibaba 广州；响应头只有 `via: 1.1 Caddy`，无 `cf-ray`）。
于是 `Cf-Connecting-Ip: <随机值>` 一个头就能每次换一个新桶 —— **限流完全失效**。
次之，`RealIP` 对 XFF 取的是**最左**项 —— 按 RFC 约定，最左项是**客户端自己写进去**
的值，不是代理观察到的对端地址。

> ⚠️ **修正**：初稿这里写的是「Caddy `reverse_proxy` 会**追加** XFF」，**这是错的**。
> 本仓 Caddyfile 当时是 `header_up X-Forwarded-For {remote}`（**覆盖**；现已改为 `{remote_host}`）。结论不变，理由改正：
> 真正可利用的是 `Cf-Connecting-Ip` 那条腿（无条件信任 + 线上无 Cloudflare）。
> XFF 那条腿被 Caddy 的覆盖行为消掉了，但那是**部署巧合**而非代码保证 ——
> 能直连 `172.17.0.1:8788` 的进程对端即「可信」，仍可伪造 XFF。

修法：新增 `httpx.ClientIP(r)`（peer 不可信 → 忽略一切头；可信 → XFF **从右往左**
取第一个不可信项；`Cf-Connecting-Ip` 默认不看）。

> 一个值得记住的过程细节：**修复者第一版把 XFF 循环写成了从左往右 —— 正是漏洞本身**，
> 被他同一批新写的测试当场抓住。这证明那批测试在测行为，不是装饰。

## 为什么两个都容易漏

- **头名看起来像信任凭证。** `Cf-Access-*` 的语义是"Cloudflare 已认证"，但语义只在
  Cloudflare 真的在链路上时才成立。**名字不是来源保证。**
- **"以后会配"的注释会长期存活。** 一段为尚未存在的部署写的兼容代码，在部署始终没来时
  就变成了纯粹的缺口。**没有截止日期的兼容代码就是漏洞。**
- **两者都不产生任何异常信号。** 功能正常、测试全绿、日志干净。

## Guard（已落地）

1. `TestRequireSessionCFAccessHeaderIsInertByDefault`：用**生产上那条一模一样的**
   `lead-verify@example.invalid`，peer 覆盖 `1.2.3.4` / `203.0.113.7` / Caddy `172.17.0.1`
   / `127.0.0.1` → **全部 401 且无 subject**。
2. `TestClientIPSpoofingIsIgnored`：不可信 peer + 伪造 `Cf-Connecting-Ip`/XFF → 返回真实 peer。
3. `TestRateLimitUsesClientIPNotForwardedHeaders`：轮换伪造头**不换桶**。
4. **头信任面普查**（可复用）：`grep -n 'Header\.' internal/httpx/*.go`，逐个定性。
   本轮结论：除 `Authorization`（其值经 `ConstantTimeCompare`/sha256 查表**验证**，
   不是"信任"）外，已无第二处把头当权威输入派生身份/权限。`Cf-Ray`/`X-Request-Id`
   只用于日志关联 id（无权限语义），但已加 128 字节截断 + 控制字符拒绝，因为
   **1MB 的头可让未鉴权者写满 journal 把全站拖垮**，且含 `\n` 的值是日志注入原语。

## 一条元教训

写安全理由时，**先确认那个理由是可验证的**。我把「Caddy 追加 XFF」当成既定事实写进了
决策与陷阱文档，而仓库里就躺着一份能立刻推翻它的 Caddyfile —— 我引用的是**记忆**，
不是**文件**。审计员逐字比对后指出了这一点。

危险在于：**结论碰巧是对的，理由是错的**，而这种文档比没有文档更糟 —— 下一个人会照着
错的理由去推理，然后在另一种代理配置下得出错误的结论。凡是「某个外部组件的行为如何」
的论断，写进文档前先打开那个组件的配置文件看一眼。

## 推广

> **对每一个被读取的请求头问三句：谁能把它送进来？我怎么证明这一点？如果任何人都能送，
> 它还能派生权限吗？**
>
> 更要紧的是：**上线一个"能铸造凭据/授权"的新功能时，先回头审一遍它挂着的鉴权层。**
> 旧洞 × 新的危险资产 = 新的严重级别。本轮的 🔴 就是这样被"放大"出来的，
> 而不是被新代码写出来的。
