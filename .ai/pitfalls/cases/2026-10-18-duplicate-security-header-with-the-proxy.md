# 应用与反代同时下发同一个安全头：线上每响应出现两个 `Strict-Transport-Security`

**Severity:** 🟡（不致命，但是**静默**的：浏览器只处理第一个，功能看似正常）
**First hit:** 2026-10-18（安全中间件部署后，**线上实测**才发现；本地测试全绿）· **Hits since:** 1（同族：CSP，见 `.ai/runbooks/caddy-routing-on-shared-host.md`）

## 症状

安全中间件上线后，从大陆观测点 `curl -D- https://clip.hcyj.xyz/api/health`：

```
strict-transport-security: max-age=63072000; includeSubDomains; preload   ← Caddy
strict-transport-security: max-age=31536000; includeSubDomains            ← 应用（新加）
```

**同一个响应两个同名头。** 本地 `testServer` 全绿——因为它只测应用自己，看不到 Caddy。

## 根因

**HSTS 属于 TLS 层，不属于应用层。** hcyj 的 Caddyfile 里已经写死：

```caddyfile
# The Go service sends its own, stricter set (nonce-based CSP, nosniff,
# noindex, no-store). Only HSTS is added here, because it belongs to the TLS
# layer rather than the application.
header {
    Strict-Transport-Security "max-age=63072000; includeSubDomains; preload"
}
```

中间件在「公网源是 https」时又加了一条更弱的。应用**看不到**反代追加的头（反代在
proxy 之后才加），所以它无法去重，只能「不要发」。

RFC 6797 §6.1：UA 只处理第一个 STS 头。所以效果上无害，但：
- 一份策略两个值，任何审计工具都会报冲突；
- 谁更强取决于顺序，而顺序由中间件链决定 —— 这是把安全策略变成偶然。

同族先例：Caddy 站点级安全头与后端 CSP 同时下发（runbook 里那条「两份同时下发会重复且
互相冲突」）。

## 修复

`internal/clip/middleware.go` 的 `securityHeaders` **删掉 HSTS**，并在注释里写明原因
（TLS 层拥有它）。应用只负责 CSP / nosniff / X-Frame-Options / Referrer-Policy /
X-Robots-Tag / Permissions-Policy / COOP / CORP。

## 守卫

- `internal/clip/middleware_test.go` 的 `TestNoHSTSFromTheApplication`：
  **两种公网源（http/https）都必须没有** `Strict-Transport-Security`。
- 部署验证必须包含「线上响应头逐个形状实测」这一步（本地测试无法覆盖代理层）。

## 教训

**反代和应用共同面对同一个响应时，每一类头都必须有唯一归属。** 定归属的判据是「它属于哪一层」：
- TLS 层（HSTS、证书）→ 反代
- 内容层（CSP、nosniff、X-Frame-Options、Referrer-Policy）→ 应用
本地测试永远看不到代理加的那一半，所以**代理层的重复只能靠线上实测发现**。
