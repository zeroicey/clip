# Architecture

## 1. 目的

clip 是一个**网络剪贴板**：`https://clip.hcyj.xyz/<后缀>` 打开一块写字板，写下内容保存，
在另一台机器打开同一个网址即可读到。场景是 Linux 运维时两台机器之间临时传文本/截图。

**显式不做的事**：不做账号、不做协作编辑、不做全文检索、不做多用户、不面向 AI、不做 API key。
读者永远匿名（这是目标而不是缺陷），作者用创建时拿到的 owner token 证明身份。

关键取舍：**短链接 > 功能**。整个子域名存在的理由是「后缀就是路径的第一个段」，
所以 URL 短到能从一个屏幕念到另一个屏幕。

## 2. 技术栈

| 层 | 选型 | 为什么 |
| --- | --- | --- |
| HTTP | Go 1.26 + **Gin** | 路由/中间件省事；但响应封套、错误码、`requestId` 自己控制，不用 Gin 的默认 JSON 形状 |
| 数据库 | **PostgreSQL**（本机容器） + **pgx/v5** | 并发写、`BYTEA`（图片）、`ON CONFLICT ... RETURNING`（原子的后缀占用/接管）、advisory lock（配额） |
| SQL | **sqlc**（不是 ORM） | 查询写成 SQL、生成类型安全代码。所有权判定必须写在 `WHERE` 里，ORM 的糖会把它藏起来 |
| 页面 | **服务端渲染的单文件 HTML**（`//go:embed`）+ nonce 内联脚本 | 没有构建步骤、没有打包器、没有「产物陈旧」这一类故障；运维场景要求「随便一台机器的浏览器都能用」 |
| 实时 | **长轮询 + `rev`**（不引依赖） | 主交互是「A 存、B 看」的一次状态跃迁，不是流。WebSocket 会多出一整套连接生命周期 |
| 部署 | systemd + hcyj 上的 Caddy（TLS/转发）+ frp 隧道 | 本机在 NAT 后，入向不可达 |

> **没有 Node 工具链。** 页面是自包含的 HTML/CSS/JS，由 Go 直接发。这是刻意的：见
> `.ai/decisions/2026-10-15-clip-service-design.md` 的 D8/D9。

## 3. 仓库地图

| 路径 | 职责 |
| --- | --- |
| `cmd/clip/main.go` | 进程入口、信号、`migrate`/`sweep`/`version` 子命令、janitor goroutine |
| `internal/clip/config.go` | 环境变量解析与校验（`DATABASE_URL`、限额、kill switch） |
| `internal/clip/suffix.go` | 后缀语法与保留字 |
| `internal/clip/token.go` | owner token 的生成与 `sha256` 存储形式 |
| `internal/clip/store.go` | 连接池、迁移、全部存储语义（所有权、过期、配额、资产） |
| `internal/clip/queries/clips.sql` + `clipdb/` | sqlc 输入与生成物 |
| `internal/clip/api.go` | Gin 路由、封套、JSON/multipart 写入、公共路径分发 |
| `internal/clip/page.go` + `page.html` | 服务端渲染页面、CSP/nonce、落地页、robots/favicon |
| `internal/clip/hub.go` | 进程内 long-poll hub（按后缀的唤醒） |
| `internal/ratelimit/` | 按 IP 的固定窗口写限流 |
| `internal/clientip/` | 客户端 IP 判定（限流键）—— 从 teleport 抽出，规则与回归测试一并带过来 |
| `scripts/` | `check.sh`（闸门）、`build.sh`、`check-sqlc-sync.sh`、`test-pg.sh` |
| `bin/e2e/` | 真实浏览器验证脚本（不进 git） |

## 4. 拓扑

```text
浏览器 ──https──> clip.hcyj.xyz
                    │
   hcyj (8.148.233.134, 阿里云广州)
     Caddy(:443, docker 容器)  ── 终止 TLS、加 HSTS、只做转发
                    │  reverse_proxy 172.17.0.1:8790
     frps(:7000 控制口 + :8790 转发口)
                    ▲
                    │  隧道由 hpcore 主动拨出（见 §6）
   hpcore (100.64.0.1, 佛山电信)
     frpc ──> 127.0.0.1:8790  clip (systemd, User=relay)
                    │
     PostgreSQL 容器(127.0.0.1:5432)  database=relay, role=relay
```

**只有一条入口、一个来源**：页面、JSON API、raw 视图、图片全部由同一个进程在同一个 origin 上提供，
所以 `script-src 'nonce-…'`、`connect-src 'self'`、`img-src 'self'` 都真的有意义。

**四条关键流**：

1. **创建/更新**：`PUT /api/clips/{suffix}` → 校验后缀 → 限流 →（有 token 先试 owner 限定的
   `UPDATE`，没有/未命中则 `INSERT ... ON CONFLICT DO UPDATE WHERE expires_at <= now`）→ 提交后
   `hub.publish(suffix)` 唤醒等待者 → 返回（创建时**仅此一次**带 `owner_token`）。
2. **读取**：`GET /api/clips/{suffix}` → `WHERE expires_at = 0 OR expires_at > now`。
   不存在返回 `200 {exists:false}`（第一次访问的预期动作是创建）。
3. **长轮询**：`GET /api/clips/{suffix}?rev=N&wait=25` —— 已经最新则注册到 hub 等待，
   被唤醒或超时后重读返回。落后的调用者**立即**返回。
4. **图片**：`POST /api/clips/{suffix}/assets`（multipart，要 token）整体替换，
   位置即地址（`/raw/{n}`）；类型从字节嗅探。

## 5. 不变量

1. **所有权判定在任何写入之前**，且写在一条 SQL 的 `WHERE` 里（`owner_hash = $n`）。
   改/删时「不存在」「不是你的」「已过期」三者一律 404 —— 不做存在性 oracle。
   **Guarded**：`TestUpdateRequiresTheOwnerToken`（断言内容与 rev 都没动，不只是「返回了错误」）。
2. **过期的后缀可被接管**：`ClaimSuffix` 的 `DO UPDATE` 只在 `expires_at <= now` 时命中。
   否则一个刚过期、janitor 还没删的行会永久占住这个后缀。**Guarded**：`TestExpiredSuffixBecomesReusable`。
3. **接管必须清掉前一位所有者的图片**（接管是 UPDATE，`ON DELETE CASCADE` 不会触发）。
   **Guarded**：`TestTakeoverClearsTheOldOwnersImages`。
4. **`rev` 每次成功的写入都递增**，即使内容一模一样 —— 读者等的是这个数。
   **Guarded**：`TestRevChangesEvenWhenTheBodyDoesNot`。
5. **配额在事务里、advisory lock 之后检查**（读总量→比较→插入三步并发下会一起通过）。
   **Guarded**：`TestQuotaStopsWrites`（并断言被拒的写入**没有落库**）。
6. **后缀是保留字安全的**：`/api/...`、`/favicon.ico`、`/raw` 等不可能成为可写的后缀。
   **Guarded**：`TestReservedSuffixesAreNotAddressable`（遍历保留字表）。
7. **页面内容永远是文本**：`html/template` 转义 + per-response nonce CSP + 无 `unsafe-inline`。
   **Guarded**：`TestPageEscapesTheBody`。
8. **图片类型来自字节嗅探**，只允许 4 种光栅格式（无 SVG）。**Guarded**：
   `TestAssetTypeIsSniffedNotDeclared`、`TestUploadRejectsNonImages`。
9. **公共页面不是 404**（Gin 的 `NoRoute` 会预置 404，见 pitfalls）。
   **Guarded**：`TestPublicPagesAreNot404`。
10. **跨语言字段名契约**：响应用 snake_case，请求用 camelCase；页面读的每个字段名都有测试钉住。
    **Guarded**：`TestClientReadsTheFieldNamesTheServerWrites`。
11. **限流键来自 `clientip.ClientIP`**，绝不采信直接的转发头。
    **Guarded**：`internal/clientip` 的 4 个回归测试（含 Caddy `{remote}` 的 `host:port` 格式）。
12. **schema 不漂移**：迁移目录是唯一真源，sqlc 读同一批文件，`sqlc diff` 在闸门里。

## 6. 边界与已知代价

- **可用性 = 本机的可用性。** 后端跑在家里的机器上，公网入口只是转发。本机关机/断网，
  整个剪贴板就不可用（hcyj 上不会有一个降级页面）。
- **hcyj → hpcore 的入向 TCP 是死的。** 实测：`hcyj → 100.64.0.1:{5432,6379,3000,...}` 全部
  超时，而 `hpcore → hcyj` 通、`tailscale ping` 有回（DERP 70ms）。所以隧道只能由本机拨出。
  见 `.ai/pitfalls/cases/hcyj-cannot-reach-hpcore-inbound.md`。
- **延迟多一跳**：大陆用户 → 阿里云广州 → 隧道 → 佛山电信，实测约 100ms。对剪贴板可以接受。
- **无索引/无列表**：后缀就是全部命名空间，服务端不提供枚举。这是隐私上的取舍，也是防滥用的取舍。
- **配额满了是「关闭写入」**，不是「静默丢数据」。`/api/health` 会显示 `writes_enabled`。
- **图片存 Postgres `BYTEA`**（TOAST 出主表）。总量超过 ~1 GiB 时应该改走对象存储
  （hpcore 上已有 MinIO，但需要独立凭据）。
- **没有账号体系**：删掉浏览器 localStorage 里的 token 就等于永久失去那一条的写权限。
  这是刻意的（那是唯一的凭据），但值得在 UX 上说明。
