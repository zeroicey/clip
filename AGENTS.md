# clip — agent instructions

**clip** 是一个**网络剪贴板**：在 `https://clip.hcyj.xyz/<后缀>` 打开一个文本框，写下内容保存，
然后在**另一台机器**的浏览器打开同一个网址即可读到（也可 `curl <网址>/raw`）。
场景是 Linux 运维时两台机器之间临时传文本或截图 —— 纯给人用，**不面向 AI**，不需要账号。

它曾经是 `teleport` 仓库里的一个模块，2026-10-16 拆成独立仓库：**本仓库只有 clip**；
teleport（AI 报告发布）在它自己的仓库里，并按同样的技术栈重写。

## 命令

没有 Node 工具链 —— 这个仓库是纯 Go，页面是 `//go:embed` 的单文件 HTML。

```bash
scripts/check.sh                 # 闸门：gofmt + go vet + sqlc 漂移 + go test
go test ./...                    # 只跑测试（无数据库时 Postgres 那批会 SKIP）
TEST_PG_DSN='postgres://relay:PW@127.0.0.1:5432/relay_test?sslmode=disable' scripts/test-pg.sh   # 含真库的完整测试
scripts/build.sh                 # 发布构建 → bin/clip-linux-amd64
sqlc generate                    # 改过 internal/clip/queries/*.sql 之后（需要 sqlc）
```

`GOPROXY=https://goproxy.cn,direct`（大陆网络下必须）。

## 目录

| 路径 | 职责 |
| --- | --- |
| `cmd/clip/main.go` | 入口：`serve`（默认）/ `migrate` / `sweep` / `version` |
| `internal/clip/` | 这个服务的全部功能：配置、后缀规则、存储、API、页面、long-poll hub |
| `internal/clip/share.go` | 只读分享链接：token/取件码生成、`Share` 类型、分享的读写语句 |
| `internal/clip/api_shares.go` | 分享的主入口（`/api/clips/:suffix/shares`）与访客读侧（`/s/<token>`、`/api/shares/:token`） |
| `internal/clip/migrations/` | 迁移 = schema 的**唯一真源**（sqlc 也读这里） |
| `internal/clip/queries/` + `clipdb/` | sqlc 的输入与生成物（生成物入库，靠 `sqlc diff` 防漂移） |
| `internal/clientip/` | 客户端 IP 判定（限流键）。从 teleport 抽出，规则与测试一并带过来 |
| `internal/ratelimit/` | 按 IP 的写限流（固定窗口，进程内） |
| `deploy/` | systemd unit 与 env 模板 |
| `scripts/` | 闸门、构建、带库测试 |

## 硬约定

- **密码是唯一的凭据；没有密码就是公开的。** `password_hash IS NULL` = 任何人可读、可写、可给它设密码；
  非 NULL = 只有知道密码的人能读、能写、能换密码。**没有 owner token 了**（2026-10-16 废除，
  见 `.ai/decisions/2026-10-16-clip-open-edit-and-ownership.md`）。密码只存 argon2id 的 PHC 字符串，
  明文永不入库、永不回显、永不进日志。
- **写判定必须一次语句完成。** argon2 只能在 Go 里算，但**判定仍然发生在写语句的 `WHERE` 里**：
  调用方把「刚校验过的那个哈希」当 `@proof_hash` 传进去。中间密码被改过 → 0 行 → 404。
  **绝不退化成 read-then-write**（那正是要防的竞态）。
- **密码挡全部 4 条读路径**：页面、`/<后缀>/raw`、`/<后缀>/raw/N`、`GET /api/clips/:suffix`。
  漏掉任何一条，密码就只是装饰。**上锁的页面绝不把正文渲进 HTML**（渲成壳，解锁后走 API 拉）。
- **分享链接（`/s/<token>`）天生只读、口令独立**：`/s/...` 只挂 GET/HEAD（写形状 → 405）；
  分享口令**不是**主密码 —— 读 `/s/...` 时不套 clip 自己的密码闸门（分歧二 → a）。
  分享**不复制内容**（指同一份），撤销 = 删行，`on delete cascade` 跟 clip 一起消失；
  **但接管（`ClaimSuffix` 的 DO UPDATE）不会触发 cascade，必须显式清 `clip_shares`**
  （与 `clip_assets` 同一个坑，`store.go` 的 `Create` 里一起清）。上锁的分享页同样不渲正文，
  且**不回显后缀**（`/s/<token>` 只认 token）。
- **密码口令用 header（`X-Clip-Password`），绝不用 query string 或 URL 路径**——会进访问日志、
  浏览器历史与 Referer。解锁用 `POST /api/clips/:suffix/unlock`。
- **忘密码不可恢复**：没有找回流程，只能等过期或换后缀。
- **无密码的 clip 仍然是公屏**；设了密码才是「不全公开」。面向用户的说明两边都要讲清楚
  （未设密码时**谁都能改**，且**任何人都能给它设密码把它变成自己的**）。
- **内容一律按文本渲染**（`textContent`，绝不 `innerHTML`）；CSP 用 per-response nonce，
  不允许 `unsafe-inline`/`unsafe-eval`。作者完全不可信，因为写入口是匿名的。
- **上传的图片类型从字节嗅探，绝不采信声明**；只允许 PNG/JPEG/GIF/WebP（**不含 SVG**：它是可执行文档）。
- **未知名后缀的页面返回 200 + 空编辑器**，不是 404 —— 第一次访问的预期动作就是创建。
  代价是页面层不区分「空」与「不存在」，可接受：两者响应体形状完全一致，不泄漏任何东西。
- **限流键必须用 `clientip.ClientIP`，绝不用任何 RealIP 式实现**（转发头默认可伪造）。
  解锁失败**同时**按 `ip:<地址>` 与 `suffix:<后缀>` 两个桶计费，且**只对失败计费**
  （正确的密码永远不消耗预算；否则长轮询会把自己锁死）。桶键必须带前缀，否则 IP 串与后缀
  可能撞进同一个桶 —— 见 `.ai/pitfalls/cases/2026-10-10-rate-limit-collapsed-to-one-bucket.md`。
- **时间一律 epoch 毫秒**，`expires_at = 0` 表示永不过期（与 teleport 同一约定）。
- **响应封套**：`{ok:true,data,requestId}` / `{ok:false,error:{code,message},requestId}`；
  **请求体 camelCase、响应 snake_case** —— 这条跨语言契约有 Go 测试钉住
  （`TestClientReadsTheFieldNamesTheServerWrites`），因为写错是**静默**的。
  页面路径（`/`、`/<后缀>`、`/raw`）的错误是 `text/plain`，`/api/*` 一律是封套。
- **密钥永不入库**：`DATABASE_URL`（含口令）只在 `/data/services/clip/clip.env`（600 root:root）。
- **部署不在本机**：服务跑在 hpcore（`127.0.0.1:8790`），公网入口是 hcyj 上的 Caddy +
  frp 隧道（hpcore 主动拨出）。**hcyj → hpcore 的入向 TCP 是死的**（见 pitfalls），
  所以任何「由 hcyj 主动连本机」的方案都不成立。
- `git commit` / `git push` / 线上部署前先问人类。

## 完成定义

1. `scripts/check.sh` 全绿；改过 SQL 时 `sqlc diff` 无漂移。
2. 涉及存储语义的改动，必须跑 `scripts/test-pg.sh`（对着**真库**，不是 mock）。
3. 涉及页面/脚本的改动，必须用**真实浏览器**核对（`scripts/verify-clip-browser.cjs`）：
   类型检查通过 ≠ 页面能看 ≠ 点击真的能保存。
4. 涉及线上行为时，从**大陆观测点**验证（hcyj 本机 / bjbuwe / koma）。
5. 行为、决策、陷阱的变更在**同一次改动**里写回 `.ai/`。

<!-- Pointer to the sibling project. The two repositories are deliberately
     independent (see .ai/decisions/2026-10-16-two-repositories.md), but a change
     to the shared host's Caddy routing or to the frp tunnel touches both. -->
**相关仓库**：teleport（AI 报告发布，同一个 hcyj 主机、同一个 Postgres 实例）在它自己的仓库里。
两边共享的只有：hcyj 上的 Caddy 站点配置、本机 Postgres 实例、frp 隧道机制。

_Read by: pi._

<!-- engram:managed -->

<!-- engram:contract:start v1 -->
## Project memory bank (`.ai/`) — required contract

This repository keeps a portable, tool-independent memory bank in `.ai/`. It outranks chat
history and your own recollection. Read it on demand with file tools; never paste it wholesale.

### Read before you act

1. `.ai/CURRENT_TASK.md` — the current goal, in-progress state and blockers. Read this first.
2. Newest-first scan of `.ai/decisions/` for `💭 PROPOSAL` / `✅ ACCEPTED` entries that touch
   your task. An accepted decision is binding; a proposal needs a verdict, not silent adoption.

### Read on demand (trigger → file, not a bulk preload)

| Trigger | Read |
| --- | --- |
| changing architecture, stack, module boundaries | `.ai/ARCHITECTURE.md` |
| deploy, env vars, CI, incidents, ops | `.ai/runbooks/<topic>.md` |
| a bug that feels familiar / "we fixed this before" | `.ai/pitfalls/cases/<case>.md` |
| resuming after context loss, compaction or a long session | newest `.ai/sessions/*-handoff.md` |
| brainstorming a direction, before writing code | `.ai/decisions/` (write a `💭 PROPOSAL`) |

### Write back (not optional)

| Event | Write |
| --- | --- |
| a choice that is expensive to reverse | `.ai/decisions/YYYY-MM-DD-<topic>.md` via `/remember-decision` |
| a pitfall you hit, or a silent failure mode you decoded | `.ai/pitfalls/cases/<case>.md` via `/remember-pitfall` |
| ending a session with unfinished or fragile work | `.ai/sessions/YYYY-MM-DD-<topic>-handoff.md` via `/handoff` |
| any change to goal, state or blockers | `.ai/CURRENT_TASK.md` (same edit turn, never "later") |

Rules of the bank:

- One file per topic, `YYYY-MM-DD-<kebab-topic>.md`, status header on line 1 of the body:
  `💭 PROPOSAL` → `✅ ACCEPTED` → `🪦 REJECTED`. Flip the status, never rewrite history.
- Keep entries short and factual. The bank is an index of decisions, not a diary.
- Canonical skill specs live in `.ai/skills/*.md`. If your tool can load them, load them; if
  it cannot, this table is the fallback contract — follow it literally.
- If the bank and the code disagree, the bank is stale: fix the bank in the same change.
<!-- engram:contract:end -->
