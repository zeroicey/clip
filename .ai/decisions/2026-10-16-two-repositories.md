# 两个仓库：clip 独立，teleport 单独重写

**Status:** ✅ ACCEPTED · **Date:** 2026-10-16 · **Deciders:** 用户（"我想了一下 还是分成两个仓库
不要塞在一个项目里面去了 你把 teleport 重写成 gin 那一套就行"）
· **Supersedes:** `2026-10-15-relay-umbrella-and-clip-module.md`（部分：D1 伞名、D2 迁移范围、D16 改名范围）
· **Related:** `2026-10-15-clip-service-design.md`

## 背景

2026-10-15 的裁决把两个功能放在一个伞名（Relay）下、一个仓库里、分两期迁移。
一天后用户改判：**拆成两个仓库**，并且 teleport 要按 clip 那套技术栈（Gin + Postgres）重写。

改判的合理之处：两个服务的**受众、风险面、发布节奏完全不同**。

| | clip | teleport |
| --- | --- | --- |
| 受众 | 任意人（匿名读写入口） | 单管理员 + 已授权的 AI |
| 风险面 | 公开匿名写（垃圾/滥用/存储） | 凭据管理、在野链接契约 |
| 前端 | 服务端渲染一个 HTML 文件 | Vue SPA + 自托管字体 + Tailwind |
| 发布节奏 | 独立 | 独立（且被 243 个测试守着） |
| 共享 | 只有：hcyj 的 Caddy、本机 Postgres 实例、frp 机制 | 同左 |

一个仓库里放两个受众与风险面都不同的服务，会强迫每一次发布都同时承担两者的风险 ——
而 2026-10-15 那版"分两期"其实已经承认了这一点（一期不动 teleport），只是没有把它落到仓库边界上。

## 裁决

| # | 问题 | 裁决 | 依据 |
| --- | --- | --- | --- |
| S1 | 仓库划分 | **本仓库 = clip**（纯 Go），**teleport 另开仓库** | 用户裁决 |
| S2 | clip 仓库名 | **`clip`**，Go module `github.com/zeroicey/clip` | 与子域名 `clip.hcyj.xyz` 一致，直白 |
| S3 | clip 的前端 | **删掉整条 Node 工具链**（pnpm/Vite/Tailwind/字体全部移出） | clip 的页面是自包含 SSR HTML + nonce 内联脚本，没有构建产物可陈旧 |
| S4 | clip 的 module 布局 | Go module 提到仓库根（原来是 `backend/` 子目录） | 纯 Go 仓库不需要为前端留子目录 |
| S5 | 共享代码 | `ClientIP` **复制**成 `internal/clientip`（含回归测试），不做跨仓库依赖 | 两个独立部署的程序共享一个文件，不值得引入依赖关系 |
| S6 | teleport 仓库的起点 | 用 `git archive HEAD` **忠实导入线上那版**（243 个测试随行），再在仓库内重写 | 测试与 pitfall 守卫就是重写的规格说明；从空文件重写等于先把它们清零 |
| S7 | teleport 的数据库 | **同一个本机 Postgres 实例、独立 database `teleport`** | 用户裁决；备份/迁移各自独立一份，不引第三个容器 |
| S8 | 旧版 teleport 的去向 | **留在 hcyj 不动**，等重写版验证通过后一次性切换 | 用户裁决；不搬即将被替换的代码，也不多一次停机窗口 |
| S9 | 共享的真相 | 两边各自有自己的 `.ai/`；host 层的知识（Caddy、frp、Postgres、大陆可达性）**两边都留** | 一个服务的部署文档不该在另一个仓库里才能查到 |
| S10 | `TrustCloudflare` 开关 | **不带到 clip** | 路径里永远没有 CDN；一个只可能被错误打开的开关，删掉比写文档更可靠（`internal/clientip` 里有测试钉住「永远忽略该头」） |

## 后果

**更容易的**

- 两个服务的发布、回滚、依赖升级互不牵连；clip 的仓库只有 Go 与一个 SQL 目录。
- clip 不再需要 Node → 构建从「Vite + Go」变成一次 `go build`，CI/闸门也随之变简单。
- teleport 的重写有了明确的起点与 oracle（现版代码 + 243 个测试）。

**变难的 / 必须记住的**

- **共享基础设施的改动要动两边**：hcyj 的 Caddyfile 同时服务两个站点；本机 Postgres 实例的
  版本升级/备份策略影响两个库。没有共享代码可以帮上忙，只有 `AGENTS.md` 里的互相指路。
- teleport 重写期间，**在野契约必须逐字节保持**（`agentkey` 的 derivation label、
  `teleport_session`、`X-Teleport-*`、`/yeciorez/teleport/*`、404/410 语义、封套）。
  改了会让已发出的 agent 密钥与分享链接全部失效 —— 这条在重写仓库的 ADR 里另记。
- 本仓库的**目录名暂时仍是 `/data/dev/teleport`**（会话中途改 cwd 会打断工作），
  GitHub 上的仓库也还叫 `teleport`；两处重命名都由人类完成。

## 何时重开

1. 若两个服务开始需要共享**代码**（不是基础设施）→ 重新考虑一个共享 module 或 monorepo。
2. 若 clip 需要 Vue 面板（例如做管理页）→ S3 的"删掉 Node"就要重新论证。
3. 若 hcyj 上的服务数继续增长 → 站点配置应该从手写 Caddyfile 抽成模板。
