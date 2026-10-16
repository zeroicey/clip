# Current task

**Status:** ✅ **步骤 ② 已上线并验证** —— 定位 = **webnote.cc 的开源版本**。
线上 = **`172ea54`**（干净工作区构建，可追溯）。
步骤 ①（密码 + MIT LICENSE + 自动保存）与步骤 ②（只读分享链接 `/s/<token>`）都已上线。
**下一步 = 未排期**（见下方「未做」：`pg_restore` 恢复演练仍最大缺口）。
**Updated:** 2026-10-16（第三会话）

### 步骤 ② 部署后的线上验证（都是实跑）

- 迁移已应用：`schema_migrations` = `0001` + `0002` + **`0003`**；`relay` 库里
  `clip_shares` 已存在（PK、`suffix` 索引、部分 `expires_at` 索引、
  `suffix → clips ON DELETE CASCADE`）。
- **runbook (a)-(d)**：本机 loopback 200；hcyj `curl 172.17.0.1:8790` = 200；
  caddy 容器 `wget` 可过；`HEAD https://clip.hcyj.xyz/api/health` = **200**；
  契约形状：`/api/nope` = JSON、`/a/b/c` = text/plain、`/neversuch` = 200、
  `robots.txt` = 200、`POST /ops1` = 405、`POST /s/<token>` = **405**、
  `/s/<未知 token>` = **404**。
- **runbook (e)+(f) 产品码 33/33**（`/tmp/verify-shares-prod.cjs`）：密码那套 9 项全绿；
  分享那套 24 项全绿 —— 包含**分享口令能开、主密码打不开**、锁壳不含正文、
  不回显后缀、`POST/PUT /s/...` = 405、撤销后立即 404、取件码 6 位且正确码可开/
  错误码 401、列表不回显取件码、删 clip 后分享一起死。
- **真浏览器 75/75 连跑 2 次全绿**（打线上 `https://clip.hcyj.xyz`）。
- 大陆观测点 hcyj / koma：`HEAD=200 verify=0`，`/s/<未知>` = 404。
- 冷备：`/data/backups/clip-20261016-172707.dump`（`pg_restore -l` 可读，15 TOC 项，`dbname: relay`）。

### 部署时抓到并修掉的真问题（都是部署后才暴露的）

本地真浏览器 3 次全绿，**打线上第一次就红**：`the share link has the /s/<token> shape`
→ `Cannot navigate to invalid URL`。不是产品缺陷，是**脚本与页面的时序竞速**：
页面先写“已创建”状态、再异步刷新列表，而脚本看到状态文本就立即读 `#sharelist a`。
线上 RTT ~100ms → `href` 还是空 → `Page.navigate("")` 报 invalid URL。
修法：页面 `await loadShares()` 再报成功（同时也是 UX 修复），脚本改成
等锚点出现。提交 `172ea54`，线上 75/75×2。详见
`.ai/pitfalls/cases/2026-10-16-share-page-unlocked-into-an-editor.md` 的「同族」一节。

**注（刻意为之）**：生产 `clip.env` **没有**显式写 `CLIP_UNLOCK_PER_HOUR` /
`CLIP_SHARE_CODE_TTL`，它们用代码默认值（口令猜测预算 20 次失败/小时/桶、
取件码有效期上界 30 分钟）。`deploy/clip.env.example` 已把两个变量写进去并注明含义；
改它们时记得 `systemctl restart clip`。

### 步骤 ① 部署后的线上验证（都是实跑，当时线上 `9126d5c`）

- 三条连通性检查：a) HEAD 200、b) `verify=0 http=200`、c) 隧道 200。
- 密码那套（线上真跑）：无口令 `/raw` 401 且不泄正文、带口令 200、页面是锁态、
  **页面 HTML 里搜不到正文**、`GET /api` 无口令 401、错口令 401、对口号 unlock 200、
  无口令不能删、带口令删除 204。公开 clip 仍然公开（`/raw` 直读、页面可编辑）。
- **真浏览器 43/43**，连跑 4 次全绿（修复重叠解锁竞态之前是间歇性 41/42）。
- 迁移已应用：`schema_migrations` = `0001` + `0002`；`clips` 有 `password_hash`、
  **没有 `owner_hash`**。
- 部署产物可追溯：线上 `version` = `9126d5c` = HEAD，工作区干净。

### 部署时抓到并修掉的真问题

真浏览器验证间歇性失败（约 1/3）：输入正确密码后页面**先解锁、再把自己锁上**。
线上日志显示两次 unlock 相隔 **1ms**。根因：页面把「当前口令」放在一个单值变量里，
而过期那次的失败分支会 `password = storedPw()` —— 此刻 storage 还空着，
于是把刚被接受的口令擦成了空串，随后的轮询用空凭据 → 401 → 切回锁壳。
**用户双击解锁、或手快改密码就会撞上。** 修法：unlock 带 generation，
只有最新一次能改状态。详见 `.ai/pitfalls/cases/2026-10-16-overlapping-unlock-attempts.md`。

另一个自测陷阱：`CLIP_WRITE_PER_HOUR=30` 是**按源地址、所有后缀一个桶**，
而验证脚本现在一次跑约 **10** 次写（步骤 ② 又加了两次「建分享」）—— 一小时内跑三次以上
就把自己限了，并伪装成无关的功能故障。脚本会识别并以退出码 3 说明；
限流器是进程内的，`systemctl restart clip` 即清空。（本次部署就每轮前重启一次。）

## 步骤 ① 落地了什么（密码 / 归属 / 自动保存 / LICENSE）

三份 decision 已 ✅ ACCEPTED 并已实现：

- 迁移 `0002_password.sql`：加 `password_hash TEXT`（NULL = 公开），**删 `owner_hash`**；
  存量行一律 NULL，所以对已有内容是语义中性的。
- `password.go`：argon2id（19 MiB / t=2 / p=1）的 PHC 哈希与解析；`hashPassword`/`verifyPassword`；
  长度 4–128。
- 写权限：**没有 owner token 了**。无密码 = 谁都能写；有密码 = 要密码。
  判定仍写在写语句的 `WHERE` 里：把「刚校验过的哈希」当 `@proof_hash` 传进去，
  中途密码被改 → 0 行 → 404（`TestReKeyInvalidatesAnInFlightProof`）。
- **四条读路径全上锁**（页面 / `/raw` / `/raw/N` / `GET /api/clips/:suffix`）；
  上锁的页面**不把正文渲进 HTML**。
- 接口：`POST .../unlock`、`PUT .../password`、`DELETE .../password`；口令走
  `X-Clip-Password` 头或请求体，**从不进 URL**。
- 限流：`CLIP_UNLOCK_PER_HOUR`（默认 20），按 `ip:` 与 `suffix:` 两个桶，**只对失败计费**。
- 页面：解锁壳 + 设/改/去密码 + **防抖自动保存**（webnote 对齐）；密码存 **sessionStorage**（按标签页，
  `localStorage` 里的凭据已彻底去掉）。
- **MIT LICENSE** 已加（仓库之前一个 license 文件都没有）。
- 新增陷阱记录：`.ai/pitfalls/cases/2026-10-16-parked-poll-after-rekey.md`
  （挂起的长轮询在密码被设上的瞬间会 401，页面必须据此切到锁并**清掉已显示的正文**）。

**随机后缀生成明确不做**（用户：“我本身就需要一个专属我自己的名字”）。

## 步骤 ② 已实现（待部署）：分享链接

裁决：`.ai/decisions/2026-10-16-clip-share-links.md`（✅ ACCEPTED + 2026-10-16 落地补充）。

| 面 | 落地 |
| --- | --- |
| 迁移 | `0003_clip_shares.sql`：`clip_shares(token PK, suffix → clips ON DELETE CASCADE, password_hash, code_hash, expires_at, created_at)` + `suffix` / 部分 `expires_at` 索引 |
| 代码 | `share.go`（token 22 字符 base64url、6 位取件码、`Share` 类型）、`api_shares.go`（主入口 + 访客读侧）、`queries/shares.sql` |
| 主入口 | `POST/GET /api/clips/:suffix/shares`、`DELETE .../shares/:token`（都要主密码；写判定在语句里） |
| 访客侧 | `/s/<token>`、`/s/<token>/raw`、`/s/<token>/raw/<n>`（**只挂 GET/HEAD**，写形状 405），加 `GET /api/shares/:token` 与 `POST .../unlock`（锁壳换内容用） |
| 三种口令形态 | 仅链接 / 自设分享密码 / 服务端 6 位取件码（只回显一次，TTL 上界 `CLIP_SHARE_CODE_TTL`=30m） |
| 限流 | 复用 `unlockLimiter`，键 `ip:<地址>` + `share:<token>`，只对失败计费 |
| 撤销 | 删行即失效；同时 `hub.publish(suffix)` 让在看的长轮询立刻醒来；分享页看见 404 会清掉内容 |
| 接管 | `Create` 里**显式**清 `clip_shares`（cascade 在 `DO UPDATE` 上不触发；与 `clip_assets` 同一个坑） |
| 页面 | `page.html` 多一个只读 share 状态（无写入口、不回显后缀）+ 主人页的「分享」区（创建/复制/撤销/一次性取件码） |

**验证（都是实跑）**：

- 真库 `scripts/test-pg.sh -count=1` 全绿（新增 `share_test.go` 21 个测试：只读性、口令独立性、
  每个读入口不带口令拿不到正文、撤销、cascade、接管清分享、过期/清扫、限流按 token、不回显后缀、
  取件码只回显一次）。
- **真浏览器 74/74，连跑 3 次全绿**（每次前重启进程清掉进程内写限流器）。
- 两条关键守卫**反向验证过**：`shareDecision` 改成放行 → 读路径守卫红；
  去掉接管时的分享清理 → `TestTakeoverDropsTheOldOwnersShares` 红；恢复后都绿。
- 真浏览器抓到并修掉一个真 bug：解锁后 `writable = true` 让**只读分享变成了编辑器**
  （服务端 405 拦住了写入，所以是页面在撒谎）。见
  `.ai/pitfalls/cases/2026-10-16-share-page-unlocked-into-an-editor.md`。
  另一个坑：`testStore` 手写 Config 没给 `ShareCodeTTL`，取件码分享生下来就过期 → 正确口令也 404。
  见 `.ai/pitfalls/cases/2026-10-16-test-config-zero-duration-share-ttl.md`。

**未做 / 待办**：

- [x] **提交 + 部署 + 线上验证** ✅（`9872d16` + `172ea54`，线上 75/75×2）。
- [x] 真库/真浏览器/runbook (e)(f)/大陆观测点 全绿。
- [ ] `pg_restore` 恢复演练仍未做（仍是最大运维缺口）。
- [ ] 生产 `clip.env` 里显式写 `CLIP_UNLOCK_PER_HOUR` / `CLIP_SHARE_CODE_TTL`（可选，现用默认值）。

---

（更早的会话：步骤 ① 与仓库拆分，交接文档
`.ai/sessions/2026-10-16-clip-split-and-launch-handoff.md`；更早的步骤 ① 全过程见
`.ai/sessions/2026-10-16-share-links-handoff.md`。）

teleport 的重写在 `/data/dev/teleport-gin`，从那里的 `.ai/CURRENT_TASK.md` 开始读。

---

## 本轮（照交接文档跑那三条连通性检查）

交接文档给新会话的职责只有一条：证明 hpcore → hcyj 通（三条命令，都要 200 / `verify=0`）。

| 检查 | 结果 |
| --- | --- |
| b) 证书 `curl -w '%{ssl_verify_result} %{http_code}'` | ✅ `verify=0 http=200` |
| c) 隧道那一跳（从 hcyj curl `172.17.0.1:8790`） | ✅ `200` |
| a) `curl -sI https://clip.hcyj.xyz/api/health` | ❌ **404**（同一路径 GET 是 200）→ ✅ **已修并部署，现为 200** |

**根因不是网络**。Gin 的方法匹配是精确的：HEAD 不匹配用 GET 注册的路由，于是掉进
`NoRoute` 的公共路径分发器，第一个路径段被当成后缀。同一根因下 `/robots.txt`、
`/favicon.ico`、`/favicon.svg` 的 HEAD 返回 **400**（后缀不合法），而 `POST /<后缀>`
返回 **200 + 一页 HTML**。也就是：**监控/CDN/`curl -I` 这类客户端看到的是「坏了」**。
细节与守卫：`.ai/pitfalls/cases/2026-10-16-head-is-not-get-in-gin-routing.md`。

**已部署**（提交 `13ad95d`）：`internal/clip/api.go` —— 每条 GET 路由同时登记 HEAD；
公共命名空间只服务 GET/HEAD，其余 405 + `Allow: GET, HEAD`；`/api` 分支保持方法无关。

**验证（都是实跑，不是推断）**：

- `scripts/check.sh` 全绿；`scripts/test-pg.sh`（真库 `relay_test`）全绿。
- 两个新守卫都验过**反向**：stash 掉 `api.go` 后它们按线上现象失败。
- 真进程：`/tmp/clip-fixed` 起在 `127.0.0.1:8791`（对 `relay_test`），与线上跑同一张
  方法/状态码矩阵 —— **只有 7 个单元格变化（4 个 HEAD + 3 个 405），其余全同**，
  即修复没有附带行为变化，也反过来证明线上那版与 HEAD 在行为上是一致的。
- **部署后重跑**：三条检查全绿（a) HEAD = 200）；runbook 的 d) 契约形状 9/9；
  **真浏览器 27/27**（`scripts/verify-clip-browser.cjs`，打线上）；
  大陆观测点 hcyj / koma 均 `HEAD=200 verify=0`；hcyj 容器内 wget 隧道口 `REACHABLE`。
- 部署前的冷备已做并**验过能解析**（`pg_restore -l`：15 个 TOC 项、`dbname: relay`），
  回滚用的旧二进制存为 `/data/backups/clip-binary-57dd925-dirty`（sha256 `5893fd10…`）。

---

## 定位：webnote.cc 的开源版本（2026-10-16 用户指定）

用户原话：「https://webnote.cc/ 就是我们的定位就是想做一个这个的开源版本 我们已经大致还原了吧
下一步无非就是设计一下密码之类的 就不是全公开了 可以选择给这个后缀加密码了」

**对齐情况（逐项核过 webnote.cc 自己的功能说明，不是推测）**：

| webnote.cc | clip |
| --- | --- |
| URL = 名字、免注册、跨设备同步、HTTPS | ✅ 有（long-poll 同步） |
| **设置访问密码** | ✅ **已实现并上线** |
| 只读模式 | ✅ 已实现为另一个设计：**分享链接「天然只读」**（`/s/<token>`，步骤 ②），不做全局只读开关 |
| 自动保存 | ✅ 已实现（防抖） |
| 系统生成随机长链接 | ⛔ **明确不做**（用户要自己的名字，宁愿加密码） |
| 中文名字 | ❌ 只能 `a-z 0-9 - _`（webnote 支持中文） |
| 阅后即焚 / 手动销毁 | ⚠️ 有删除，没有「看一次就销毁」 |
| 全格式文件传送 | ⚠️ 只收图片（4 种，**故意不收 SVG**） |
| — | ➕ **分享链接**：只读 + 独立口令（自设密码 / 6 位取件码）+ 可撤销 + TTL（步骤 ②） |
| — | ➕ **`/raw`**：`curl` 直接拿文本（本项目最强的那个功能） |
| **LICENSE** | ✅ **MIT 已加**（步骤 ①） |

### ⚠️ 定位变了就要改的硬约定

两条都已经改完（步骤 ①）：

1. 「**后缀是公屏，不是密码**」—— 现在只对**无密码的 clip** 继续成立。
   AGENTS.md、README、页面文案、本文件已一起改。
2. 「**写要 owner token**」—— 已**整条废除**。无密码的 clip 谁都能改，密码是唯一凭据。
   `owner_token` 已从代码、迁移、契约测试、页面里彻底移除。（分享链接是第三条入口：
   `/s/<token>` **只读**，见步骤 ②。）

---

## 上一轮做了什么（拆分与上线）

用户原话（三轮）：

> 「hcyj 服务器域名现在是泛解析了 你看看处理一下他的那个 caddy 搞一个我们的子域名 但是我现在又想加一个功能，
> 可以单开一个项目，类似于一个网络剪切板……这就类似于一个共享工具，大家可以输入自己想要的后缀，
> 把文本或者图片传过去……我觉得现在加了一个新模块，这个功能是给人用的，是一个纯前端的操作，不需要 AI 操作……
> 之前的 Teleport 偏向于给 AI 使用的功能……是不是需要重新想一个项目名？」
>
> 「数据库：可能不再使用 sqlite……现在可能会换成 PostgreSQL。后端框架：不要手搓了（Gin）。
> 数据库操作：要不要用 ORM。接口获取：需不需要 WebSocket，不过不断轮询应该也可以。」
>
> 「然后我们那个后端我是想部署到我们现在这台电脑 不放到 hcyj 去了 hcyj 只做流量转发 请求转发就好了」
>
> 「我想了一下 还是分成两个仓库 不要塞在一个项目里面去了 你把 teleport 重写成 gin 那一套就行」

### 交付状态

| 项 | 状态 |
| --- | --- |
| clip 服务（Gin + pgx/sqlc + PostgreSQL） | ✅ 已上线，systemd `clip.service`，`127.0.0.1:8790` |
| 转发链路（hpcore 主动拨 frp 到 hcyj） | ✅ 实测 hcyj → frp:8790 → hpcore，103ms，Caddy 容器可达 |
| hcyj Caddy `clip.hcyj.xyz` 站点块 | ✅ 已加、已校验（`caddy adapt` + md5 比对）、已重启；既有服务不受影响 |
| `clip.hcyj.xyz` 证书 | ⏳ **等 DNS**：`*.hcyj.xyz` 仍指向 `81.148.233.134`（不可达），ACME 校验失败 |
| 数据层 | ✅ PostgreSQL 本机容器，database/role `relay`；测试用独立的 `relay_test` 库 |
| 验证 | ✅ `scripts/check.sh` 全绿；42/42 HTTP 冒烟；**打线上** 27/27 真实浏览器；大陆观测点全绿 |
| 证书 | ✅ Let's Encrypt 已签发（TLS1.3，`Verify return code: 0`），Caddy 自动续期 |
| 线上抓出的缺口 | ✅ 未匹配的 `/api/*` 曾返回 `text/plain`（形状与封套不一致）—— 已修并上线，见 pitfalls |
| 仓库拆分 | ✅ 本仓库已变成纯 Go 的 clip；teleport 已导出到 `/data/dev/teleport-gin`（243 个测试随行） |
| 旧 teleport | 🅿️ **仍在 hcyj 上正常服务**，不动；等重写版就绪再切（用户裁决） |

### 本轮抓出来的真 bug（都由验证而非推断发现）

| 现象 | 根因 | 守卫 |
| --- | --- | --- |
| 页面渲染正确但状态码是 **404** | Gin 的 `NoRoute` 会先把状态置成 404，再调用 handler | `TestPublicPagesAreNot404` |
| 创建成功但浏览器**静默失去写权限** | 服务端发 `owner_token`（响应 snake_case），页面读 `ownerToken` | `TestClientReadsTheFieldNamesTheServerWrites` |
| 每次访问都有一条 400 控制台错误 | 浏览器隐式请求 `/favicon.ico`，落到后缀校验里（含 `.` 非法） | `TestFaviconICODoesNotReachTheSuffixHandler` |
| 测试 **30% 概率**失败 | nonce 用标准 base64，`html/template` 把属性里的 `+` 转义成 `&#43;` | `TestNonceSurvivesHTMLAttributeEscaping` |
| 生产里每次启动刷路由表 | Gin 跑在 debug 模式 | 代码按 `ENVIRONMENT` 设 release |
| 监听失败被 systemd 当成功 | 主程序记日志后正常退出（`status=0/SUCCESS`） | 非零退出 + `serveErr` 通道 |

### 基础设施实测（都不是推论）

- **`*.hcyj.xyz` 泛解析指向 `81.148.233.134`，不是 hcyj 的 `8.148.233.134`**：
  两个公共解析器一致，`:80`/`:443` 全 timeout，从 hcyj ping 它 216ms（异地机房）。**须在阿里云 DNS 修**。
  本机与 hcyj 都没有 `aliyun` 凭据。
- **hcyj → hpcore 的入向 TCP 是死的**：6 个端口全 timeout，而 `hpcore → hcyj` 通、
  `tailscale ping` 有 pong（DERP "hpnet"，70ms，"direct connection not established"）。
  → 所以转发必须由 hpcore 主动拨出（frp 正好如此）。
- hcyj 上 `:443` 是 **docker 容器 `caddy`**、`:80` 是 nginx；本机 `:8788` 曾被我占用过。

---

## ⚠️ 未做 / 需要人类动作

1. ~~DNS 泛解析~~ ✅ **已修**（原记录是 `81.148.233.134` ＝ **英国电信 BT 的地址**；现已指向 hcyj）。
   证书已签发，`docker restart caddy` 后约 40 秒生效。
2. **GitHub 改名 + 本地目录改名**（见上方「下一步」）。
3. **Postgres 备份未排期** —— 现已承载两个服务的数据（`relay` 库 + teleport 的 `teleport` 库），
   只有部署前的手工 `pg_dump`，而且 **`pg_restore` 从未演练过**。这是最大的运维缺口。
4. **Postgres 凭据只在本机**：`/data/services/clip/clip.env`（600 root:root），不在任何备份里。
5. **两个仓库都是 public**，而记忆库记录了完整拓扑（IP/端口/库名/路径，**无凭据**）——
   如果不想公开这份「侦察地图」，`gh repo edit --visibility private` 即可。
6. `bjbuwe`（121.9.113.26）观测点 ssh 不可达，大陆观测点只剩 hcyj 与 koma。
7. `scripts/verify-clip-browser.cjs` 里写死了 `chromium-1248` 的路径（沿用既有脚本的做法）。
8. clip 的 **QR/二维码**、纯文本一键分享、以及「后缀被占」时的一键换名，都没做。
9. ~~HEAD/405 修复待部署~~ ✅ **已部署并验证**（提交 `13ad95d`，线上 `HEAD /api/health` = 200）。
10. ~~线上二进制无法追溯~~ ✅ **已解决**：线上现为 `13ad95d`，由**干净工作区**构建，可追溯到本仓库
    的提交。旧的那版（`57dd925-dirty`）留在 `/data/backups/clip-binary-57dd925-dirty`。
    规则已写进 runbook：只从干净树构建 + 部署后核对 `version`（用「`version` 之后没有代码改动」
    而不是「等于 HEAD」，因为部署后的纯文档提交本来就会让两者不等）。
11. **两条旧「下一步」已经完成**（记录里曾是待办）：`origin` 已是 `github.com/zeroicey/clip`
    且已推送；工作目录已是 `/data/dev/clip`。
12. 公共命名空间对**非 GET/HEAD** 一律 405，但 `/api/clips/:suffix` 上的错方法
    （如 `POST`）仍是 `404 + 封套`：要变成 405 + `Allow` 需要打开 Gin 的
    `HandleMethodNotAllowed` 并自己算 `Allow`。现在这套是**刻意**的（`/api` 的未知路径
    规则是方法无关的 404 + 封套），但如果以后要严格，就要在那儿一次做完。

### 老 teleport 服务：已停并禁用（2026-10-16，用户确认无人使用）

| 项 | 值 |
| --- | --- |
| 停之前 | 版本 `dbca734`，二进制 26 MB，`teleport.db` 307200 字节 + 10 份备份 + WAL/SHM |
| 停之后 | `:8788` 已释放；公网 `/yeciorez/teleport/*` 返回 **502**（Caddy 块**刻意保留**，P6 指回来即可） |
| 数据 | 二进制、库、`teleport.env`（含三个密钥）**原样留在 hcyj `/data/services/teleport/`**，P5 搬运与回滚都要用 |
| 恢复 | `ssh hcyj 'systemctl enable --now teleport'` |

---

## 长期约束（改动前必须知道）

- **cliff 的所有权语义**：判定写在 SQL 的 `WHERE` 里；不存在/非所有者/已过期一律 404。
- **后缀是公屏不是密码**（对无密码的 clip）；**页面内容永远按文本渲染**；**图片类型从字节嗅探**（无 SVG）。
- **分享链接 `/s/<token>` 只读且口令独立于主密码**；`on delete cascade` 跟 clip 走，
 **但接管（DO UPDATE）不触发 cascade，必须显式清 `clip_shares`**。
- **unknown 后缀的页面是 200 + 空编辑器**，不是 404（第一次访问的预期动作是创建）。
- **响应 snake_case / 请求 camelCase** —— 有跨语言测试钉住，写错是静默的。
- **限流键必须用 `internal/clientip`**；**时间一律 epoch 毫秒**，`expires_at=0` = 永不过期。
- **部署形态**：本机 systemd + hcyj Caddy + frp 隧道（由本机拨出）。**hcyj 永远连不到本机的入向端口。**
- `git commit` / `push` / 部署前先问人类。

## 验收要点

- **涉及存储语义必须对真库跑** `scripts/test-pg.sh`（mock 会让所有权/配额/过期的守卫全部形同虚设）。
- **涉及页面必须用真实浏览器**（`scripts/verify-clip-browser.cjs`）：真输入、真点击、看网络响应。
  而且**两个标签页不是两台机器**（共享 localStorage，见 pitfalls）。
- 大陆观测点：hcyj 本机（阿里云广州）、`koma`（124.221.144.97）。
  `bjbuwe`（121.9.113.26）的 ssh 已不可达（`Could not resolve hostname`），从 2026-10-16 起只剩两个。
