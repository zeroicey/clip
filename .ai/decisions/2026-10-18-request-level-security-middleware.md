# 请求级安全中间件：全局限流、封 IP、安全响应头

**Status:** ✅ ACCEPTED · **Date:** 2026-10-18 · **Deciders:** lead（用户要求「加一些安全的中间件，比如限流、封 IP」） · **Supersedes:** none

## Context

服务已公网运行（`https://clip.hcyj.xyz`，2026-10-16 上线）。翻线上 journald（2026-10-18，近 3 天
2962 条请求）得到的事实，不是推测：

| 事实 | 数字 |
| --- | --- |
| 不同路径数 | **907** |
| 404 / 400 / 401 | 1241 / 461 / 231 |
| 明显的密钥扫描路径 | `/config/.env`(20)、`/app/.env`(20)、`/.aws/config`(16)、`/.vscode/mcp.json`(13)、`/phpinfo.php`(13)、`/static../.git/config`(13)、`/.cursor/mcp.json`(13) … |

已有防线只有两条，且都只覆盖**写**或**口令**：

- `limiter`：按 IP 的写预算（30/h），在**每个写 handler 里**各判一次。
- `unlockLimiter`：按 IP + 后缀 / share token 的**口令失败**预算（20/h）。

也就是说：**读路径完全不限流**。一个地址可以无限打 `GET /<后缀>`、`/raw`、`/api/clips/<后缀>`，
每次都落到 Postgres；长轮询（`?wait=25`）还能把 hub 的 1024 个等待位占满。
被限流之后也没有升级手段——一直 429，一直重试，服务一直陪着算。

另有一处不一致：安全响应头是**逐个 handler 手写**的（`renderPage`/`renderSharePage`/
`renderLanding`/`serveRawText`/`serveRawAsset`），`/api/*` 的响应和全部错误路径（400/404/405/429）
一个都没有；HSTS 全站没有。错误路径恰恰是最容易漏的地方。

## Options

| Option | Upside | Downside | Reversibility |
| --- | --- | --- | --- |
| A 继续把限流写进每个 handler | 改动小 | 每加一条路由就多一个漏点；读路径仍然裸奔；错误路径仍然没有安全头 | easy |
| B 请求级中间件：全局每 IP 预算 + 拒绝升级为临时封禁 + 全局安全头 | 一处生效，新增路由自动被覆盖；读路径也被覆盖；错误路径也有头 | 新状态（ban list）需要内存上限与过期；配错可能拒绝所有人 | easy（全部可用环境变量关掉） |
| C 只靠 Caddy 层（rate limit / IP 过滤） | 不碰 Go 代码 | 改共享主机上别的服务也在用的 Caddy；本仓库无法测试；与本项目「闸门在仓库内」的约定相悖 | hard（跨仓库、跨主机） |

## Decision

**选 B。** 三条规则：

1. **限流键永远是 `clientip.ClientIP`，且分层。** 请求级每 IP **每分钟 240**（`CLIP_REQUEST_PER_MINUTE`），
   覆盖读与写；写预算（30/h）与口令失败预算（20/h）保持在 handler 里不动——它们知道「在尝试什么」，
   中间件只知道「来了一个请求」。
2. **只有 429 计一次违规（strike）。** 10 分钟内累计 5 次 → 临时封禁 15 分钟，之后每次翻倍、
   上限 24 小时（`CLIP_BAN_AFTER/WINDOW/FOR/MAX_FOR`）。**401 绝不计费**：未解锁就打开一个上锁的
   clip 是正常页面加载，打错一次密码的人不该被推向封禁。
3. **可信对端永不封禁**（`clientip.IsTrusted`：loopback / 私网 / 隧道入口）。
   本服务的直接对端是本地 frpc，**没有转发头的请求会归到 127.0.0.1** —— 封它等于一封封掉全网。

另外：全局安全响应头中间件（nosniff / X-Frame-Options / Referrer-Policy / X-Robots-Tag /
Permissions-Policy / COOP / CORP / CSP 基线；**HTTPS 公网源才发 HSTS**），
请求体总上限中间件，`MaxHeaderBytes=64KiB`，每 IP 长轮询并发上限（`CLIP_MAX_POLLS_PER_IP=8`，
超了**立即作答**而不是 429），以及 access log 增加解析后的客户端 IP（运维封禁列表需要它）。

## Consequences

- **容易了**：新增路由自动获得安全头与全局限流；错误路径不再裸奔；扫描器的噪声会在几次之后
  变成 403，而不是永远 400/404。
- **变难了**：进程内多了两份状态（ban list、并发计数）。它们**重启即清空**（与既有 limiter 一致），
  且都有上限：ban list 最多 10 万条、超限**失败开放**（不封而不是爆内存）。
- **必须成立**：`clientip.IsTrusted` 必须继续把 loopback / 私网判为可信；否则封禁会误伤全网。
  反向验证过：把它改成永远 `false`，`TestATrustedPeerIsNeverBanned` 立刻红。
- **已知边界（照实写）**：按 IP 的封禁挡不住多地址（或一个 IPv6 /64）攻击者——这与既有 limiter
  的边界是同一条，真正的兜底仍是 store 里的全局配额。封禁也可能误伤共享 NAT 后的多人；
  这是 IP 封禁的固有代价，`CLIP_BAN_AFTER=0` 可整体关闭。
- 中间件的拒绝形状必须按命名空间区分：`/api/*` 走封套，其余 `text/plain`。
  写错是**静默**的（见 pitfalls 里那条 `/api` 忘了封套）。

## Revisit when

- 线上出现「合法用户被封」的实例（NAT / 移动网络）→ 提高 `CLIP_BAN_AFTER` 或改为只封更长窗口。
- 请求级 240/min 在真实使用中触发（浏览器验证脚本或人）→ 提高它，或把长轮询从预算里剔除。
- 多地址扫描成为主要噪声 → 需要前缀级（IPv6 /64）或 Caddy 层配合，届时另开 decision。
