# Current task

**Status:** 🟡 **待人类裁决一次部署** —— 线上仍是 `57dd925-dirty`（拆分前的脏构建）；
本仓库里已修好一个路由缺陷（HEAD 掉进 NoRoute），闸门 + 真库测试 + 真进程验证全绿，
但**未提交、未部署**（两件事都要先问人类）。
**Updated:** 2026-10-16（第二会话）· **交接文档：** `.ai/sessions/2026-10-16-clip-split-and-launch-handoff.md`

teleport 的重写在 `/data/dev/teleport-gin`，从那里的 `.ai/CURRENT_TASK.md` 开始读。

---

## 本轮（照交接文档跑那三条连通性检查）

交接文档给新会话的职责只有一条：证明 hpcore → hcyj 通（三条命令，都要 200 / `verify=0`）。

| 检查 | 结果 |
| --- | --- |
| b) 证书 `curl -w '%{ssl_verify_result} %{http_code}'` | ✅ `verify=0 http=200` |
| c) 隧道那一跳（从 hcyj curl `172.17.0.1:8790`） | ✅ `200` |
| a) `curl -sI https://clip.hcyj.xyz/api/health` | ❌ **404**（同一路径 GET 是 200） |

**根因不是网络**。Gin 的方法匹配是精确的：HEAD 不匹配用 GET 注册的路由，于是掉进
`NoRoute` 的公共路径分发器，第一个路径段被当成后缀。同一根因下 `/robots.txt`、
`/favicon.ico`、`/favicon.svg` 的 HEAD 返回 **400**（后缀不合法），而 `POST /<后缀>`
返回 **200 + 一页 HTML**。也就是：**监控/CDN/`curl -I` 这类客户端看到的是「坏了」**。
细节与守卫：`.ai/pitfalls/cases/2026-10-16-head-is-not-get-in-gin-routing.md`。

**已修（工作区，未提交）**：`internal/clip/api.go` —— 每条 GET 路由同时登记 HEAD；
公共命名空间只服务 GET/HEAD，其余 405 + `Allow: GET, HEAD`；`/api` 分支保持方法无关。

**验证（都是实跑，不是推断）**：

- `scripts/check.sh` 全绿；`scripts/test-pg.sh`（真库 `relay_test`）全绿。
- 两个新守卫都验过**反向**：stash 掉 `api.go` 后它们按线上现象失败。
- 真进程：`/tmp/clip-fixed` 起在 `127.0.0.1:8791`（对 `relay_test`），与线上跑同一张
  方法/状态码矩阵 —— **只有 7 个单元格变化（4 个 HEAD + 3 个 405），其余全同**，
  即修复没有附带行为变化，也反过来证明线上那版与 HEAD 在行为上是一致的。

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
9. **HEAD/405 修复待部署**（本轮已修、已验证，见上）。部署需要人类同意；
   部署后 `curl -sI https://clip.hcyj.xyz/api/health` 才会从 404 变 200。
10. **线上二进制无法追溯**：`version` 是 `57dd925-dirty`，本仓库历史里没有这个提交
    （拆分重写过历史 + 脏工作区构建）。嵌入页面与依赖和仓库一致、行为只差本轮修的两处，
    但它**无法从任何提交复现**。规则已写进 runbook：只从干净树构建 + 部署后核对 `version`。
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
- **后缀是公屏不是密码**；**页面内容永远按文本渲染**；**图片类型从字节嗅探**（无 SVG）。
- **unknown 后缀的页面是 200 + 空编辑器**，不是 404（第一次访问的预期动作是创建）。
- **响应 snake_case / 请求 camelCase** —— 有跨语言测试钉住，写错是静默的。
- **限流键必须用 `internal/clientip`**；**时间一律 epoch 毫秒**，`expires_at=0` = 永不过期。
- **部署形态**：本机 systemd + hcyj Caddy + frp 隧道（由本机拨出）。**hcyj 永远连不到本机的入向端口。**
- `git commit` / `push` / 部署前先问人类。

## 验收要点

- **涉及存储语义必须对真库跑** `scripts/test-pg.sh`（mock 会让所有权/配额/过期的守卫全部形同虚设）。
- **涉及页面必须用真实浏览器**（`scripts/verify-clip-browser.cjs`）：真输入、真点击、看网络响应。
  而且**两个标签页不是两台机器**（共享 localStorage，见 pitfalls）。
- 大陆观测点：hcyj 本机（阿里云广州）、`bjbuwe`（121.9.113.26）、`koma`（124.221.144.97）。
