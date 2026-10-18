# 封 IP 会误封隧道入口：没有转发头时 `ClientIP` 返回 127.0.0.1，封它就是封全网

**Severity:** 🔴（fail-closed 且是**全站级**：一次误封让所有客户端同时 403，直到进程重启）
**First hit:** 2026-10-18（在写请求级安全中间件时**预先**解码，未在生产触发）· **Hits since:** 0

## 症状（如果没防住会是什么样）

给公开服务加「重复被限流就临时封禁来源地址」之后，某一刻开始**所有**客户端都拿到
`403 banned`（不是 429，不是 5xx），健康检查也一起红。日志里被封的地址是 `127.0.0.1`。

## 根因

本服务的部署形态是：`Caddy(hcyj) → frp 隧道 → hpcore:127.0.0.1:8790`。
**直接对端永远是本机 frpc**，真实客户端地址只能来自 Caddy 写的 `X-Forwarded-For`。

`clientip.ClientIP` 的规则是「立即对端可信才采信转发头」。于是：

- 正常请求（Caddy 带了 `X-Forwarded-For`）→ 解析出真实公网 IP → 封它没问题。
- **任何没有转发头的请求**（本机 curl、Caddy 配置漏了、探测、将来换代理）→ 返回对端
  `127.0.0.1`。

`127.0.0.1` 是**所有这类请求共用的一个键**。按它计数、按它封禁，等价于把整个互联网
当成一个人封掉。这和限流那个坑是同一族：**键错了，失败方向还是静默的**——
限流是「全世界共用一个桶」，封禁是「全世界共用一个 403」。

## 修复

封禁层在计费与拦截两处都先问 `clientip.IsTrusted(ip)`：loopback / 私网 / 隧道入口
**永不封禁**（`internal/clip/middleware.go` 的 `guard` 与 `chargeStrike`）。
`IsTrusted` 对**无法解析**的值返回 `true`（视为可信 = 不封），与 `ClientIP` 把所有不可解析
情况塌缩成一个桶的方向一致：宁可少封，不可错封。

## 守卫

- `internal/clientip/clientip_test.go` 的 `TestIsTrustedIsTheBanGuard`：
  逐个钉住 `127.0.0.1` / `::1` / RFC1918 / `fc00::/7` / 不可解析值必须是可信，
  而公网地址（含 `host:port` 形式）必须不可信。
- `internal/clip/middleware_test.go` 的 `TestATrustedPeerIsNeverBanned`：
  用 `RemoteAddr=127.0.0.1` 打 10 次、`CLIP_BAN_AFTER=1`，断言**永远不出现 403**。
- **反向验证过**：把 `IsTrusted` 改成永远返回 `false`，上面那条测试第 3 次请求即红
  （`request 3 from a trusted peer was banned; this refuses the whole internet`）。

## 教训

凡是「能主动拒绝一个来源」的机制，先问一句：**这个键在代理链上代表谁？**
如果答案可能是「一个大家共用的地址」，那这个机制要么不作用于它，要么先把它排除。
