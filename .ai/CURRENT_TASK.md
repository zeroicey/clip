# Current task

**Status:** ✅ **UI 大换皮已上线并验证** —— 线上 = **`fc88ac7`**（干净树构建，可追溯）。
本轮只改了视觉层（未改任何 id、未改任何被断言的文案、未改行为）。
**更新：** 2026-10-16（第四会话）· **设计决策：** `.ai/decisions/2026-10-16-ui-visual-direction.md`

## 本轮：UI 大换皮（「工作台仪表」）

**动过的文件只有两个**：`internal/clip/page.html`（`<style nonce>` + 少量标记）、
`internal/clip/page.go`（`landingHTML`、`faviconSVG` 颜色）。

### 装了哪些技能（用户要求：先调研、自己装、再用）

装在 **Pi 用户级** `~/.pi/agent/skills/`（**故意不入库** —— 这个仓库是纯 Go，不该塞 Node/Python 树），
来源是 UI Skills 注册表 `https://www.ui-skills.com/skills/registry.json`，清单见
`~/.pi/agent/skills/INSTALLED.md`：

| skill | 来源 | 用途 |
| --- | --- | --- |
| `frontend-design` | anthropics/skills（官方） | 定视觉方向、反 AI 默认套路 |
| `ui-ux-pro-max` | nextlevelbuilder | 设计情报（调色板/风格/UX 规则，Python + 数据） |
| `better-ui` | jakubkrehel/skills | 抛光细节（同心圆角、阴影/边框、动效精确值） |
| `baseline-ui` | ibelick/ui-skills | 去 slop（间距/层级/字型） |
| `improve-ui` | ibelick/ui-skills | 审计既有界面 → 实施计划 |
| `fixing-accessibility` | ibelick/ui-skills | WCAG/ARIA/焦点修复 |

**注意**：Pi 只在启动时扫描技能目录，所以这 6 个**本轮没有出现在我的系统提示里** ——
我是直接读 `SKILL.md` 内容并照着做的。下次会话 `/reload` 后它们会自动进入路由。

### 设计系统（详见 decision）

- **配色**：浅色 = 绘图纸（`#f2f4f4`），暗色 = 冷墨（`#0e1113`），**两套独立设计**，都走 `:root` 变量。
- **两组灰不能互换**：`--seam` 只做分隔线（视觉性）；`--edge` 是**控件边界，实测 ≥3:1**（WCAG 1.4.11）。
- **单一信号色**（青绿 `#0d6e69` / `#35c9c0`）承担「活着」语义；**琥珀只表示警告**；`--err` 只表示错误。
- **等宽只给载荷与地址**（textarea、`#addr`、分享链接、取件码、`code`）；
  **版本/过期这类读数用无衬线 + `tabular-nums`**。
- **圆角同心**：`--r-ctl:8px` → `--r-panel:19px` → `--r-panel-lg:23px`（= 内 + padding + 边框）。
- **唯一的「大胆之处」**：状态行左侧的状态灯（色 + 脉冲，脉冲只在 `prefers-reduced-motion:no-preference` 下跑）。
- **触控**：桌面 36px（保既有密度），`@media (pointer:coarse)` 抬到 44px。
- **焦点环**：`:where(a,button,input,textarea,summary,label.file):focus-visible` 一处声明，
  `outline-offset:2px` 是承重的（否则主按钮上的环会消失进自己的强调色里）。

### 本轮**没有**改的东西（这是刻意的）

- **id 一个没动**（`body content hint lockbox lockstatus pw save setpw del sharebox sharecodeout
  sharecodevalue sharelist sharestatus status` 全部原样）。
- **被断言的中文串一个没动**（`已自动保存`/`已设密码`/`解锁中`/`公开`/`密码`/`频繁`/`已创建`/
  `已撤销`/`失效`/`还没有`）。
- **`data-state` / `data-can-write` / `writable = !isShare;` 全未动**；`verify-clip-browser.cjs` **无需修改**。
- **`[hidden]` 规则改成 `[hidden]{display:none!important}`**（比原来更牢，不是更松）。
- **`white-space:pre` 保留**（移动端 textarea 横向滚动是预期行为，不是溢出）。

### 顺手修掉的（都是真问题，不是风格）

1. **`#sharepw` 一直是浏览器默认的 ~23px 输入框** —— 分享面板里所有控件都是 36px，
   只有它被漏掉（旧 CSS 只给 `.keybox input` 和 `#lockbox input[type=password]` 写了规则，
   `.row` 里的 `#sharepw` 谁都没管）。现在文本输入框是**一条规则**，按容器只调宽度。
   这个缺陷**任何只查 id 的测试都抓不到**，是本轮 DOM 几何审计抓出来的。
2. **分享列表链接的下划线画在 `--seam` 上（1.4:1，等于看不见）** —— 只剩颜色区分链接，
   违反 WCAG 1.4.1。改为用 `currentColor` 画（6.08:1）。
3. **`button` 没有显式的 `:focus-visible`** —— 一直吃浏览器默认环（各家引擎不同色不同粗细），
   与设计不一致。现在全站一处声明。
4. **`details` 里的 `code` 被 `.92em` 缩到约 12px** —— 那是要读者照抄的 `curl` 一行命令。
   面板内改为 `1em`。
5. **剪贴板页原本没有 `<h1>`** —— 品牌名包进 `<h1 class="brandline">`，给屏幕阅读器一个落点。

### 验证（全部实跑，不是推断）

| 项 | 结果 |
| --- | --- |
| `scripts/check.sh` | ✅ 全绿（gofmt + vet + `sqlc diff` 无漂移 + `go test`） |
| 真库 `scripts/test-pg.sh -count=1` | ✅ 全绿 |
| 真浏览器 `verify-clip-browser.cjs` 打本机 | ✅ **75/75**，含 **0 CSP violation / 0 console error** |
| **本机 DOM 实测对比度**（自写探针） | ✅ 4 个页面状态 × 明暗 = **106 组文本/底色对，0 失败**（逐元素算有效底色，不是只查 token） |
| **Tab 顺序 + 焦点环**（真实按键，非 `el.focus()`） | ✅ 每态 3–11 站，顺序合理（品牌 → 正文 → 主操作 → 次操作 → 折叠区），**焦点环全部画出** |
| DOM 几何审计 | ✅ 无页面级横向溢出；无文字被裁；唯一的「小目标」是 checkbox（其**包裹 label 是 36px**，误报） |
| 截图 | `bin/e2e/uifinal/`（gitignore）与 `/tmp/ui-shots/`：landing / 编辑器（空·有内容）/ 锁定 / 分享 × 明暗 × 桌面+390px，共 26 张 |
| **人类过目截图** | ✅ 裁决「Looks good as-is」（2026-10-16） |

**关键取证（本机 8797，真库 `relay_test`）**：

```bash
go build -o /tmp/relayui3 ./cmd/clip
DATABASE_URL="postgres://relay:$PW@127.0.0.1:5432/relay_test?sslmode=disable" \
  PUBLIC_BASE_URL=http://127.0.0.1:8797 LISTEN_ADDR=127.0.0.1:8797 ENVIRONMENT=production \
  setsid nohup /tmp/relayui3 > /tmp/relayui3.log 2>&1 < /dev/null &
node scripts/verify-clip-browser.cjs http://127.0.0.1:8797   # 75/75
```

（**注意**：`127.0.0.1:8799` 被隔壁 teleport 的进程占着 —— 换 8797。）

### ✅ 已部署并线上验证（2026-10-16，提交 `fc88ac7`）

从**干净工作区**构建（构建前 `git status --porcelain` 为空），线上 `version` = **`fc88ac7`** = HEAD。

| 项 | 结果 |
| --- | --- |
| 升级前冷备 | ✅ `/data/backups/clip-20261016-203338.dump`，`pg_restore -l` 可读（**21 个 TOC 项**，`dbname: relay`） |
| 回滚副本 | ✅ `/data/backups/clip-binary-prefc88ac7`（部署前那份二进制） |
| runbook (a) 本机直连 | ✅ 200 |
| runbook (b) 隧道那一跳 + caddy wget | ✅ 200 / `REACHABLE` |
| runbook (c) 公网 HEAD + 证书 | ✅ `verify=0 http=200` |
| runbook (d) 契约形状 | ✅ `/api/nope`=JSON、`/a/b/c`=text/plain、`/neversuch`=200、`HEAD /robots.txt`=200、`POST /ops1`=405 |
| runbook (e)+(f) 产品码 | ✅ **19/19**（密码 8 项 + 分享 9 项 + 清理 2 项）—— 含**分享口令能开、主密码打不开**、锁壳不含正文、不回显后缀、`POST /s/...`=405、撤销即死 404 |
| runbook (h) **真浏览器打线上** | ✅ **75/75 连跑 2 次全绿**（每次前 `systemctl restart clip` 清写限流器）；两次都 0 失败项 |
| runbook (i) 可追溯 | ✅ `version` 在历史里；`fc88ac7..HEAD` 无 `cmd/ internal/ scripts/` 改动 |
| 大陆观测点 hcyj / koma | ✅ 均 `verify=0 http=200`，`/s/<未知>` = 404 |
| 线上 CSP 未松动 | ✅ 仍是 `nonce-…`，`unsafe-inline` 出现 **0** 次 |
| **线上确实跑的是新 UI** | ✅ 新 token 全部命中（`--edge`/`--seam`/`tabular-nums`/`class="dot"`/`class="bar"`/`class="brandline"`/`:where(...):focus-visible`），旧 `#1f6feb` 出现 **0** 次 |

**一次真实故障（不是产品缺陷）**：线上真浏览器第一次跑在 “save button stayed visible” 之后
以 `verification crashed: fetch failed` 挂了（Node 自己的 fetch 传输层错误，不是断言失败）。
当时从本机 `node fetch` 三个路径都 200，重启进程后连跑两次均 **75/75**。
即：**经 frp 隧道的一次瞬时传输失败**，与页面无关。
（下次碰到：先看退出码与最后一条 PASS；`fetch failed` 不是断言。）

### ⚠️ 未做 / 需要人类动作

1. ✅ **人眼已看截图并裁决「Looks good as-is」**（2026-10-16，用户原话）。
   先前的局限如实记录：本会话可用模型**没有视觉能力**（`read` 图片被丢弃；openrouter 额度不足、
   cheaperai 的视觉端点上游 404），所以派出的视觉审查子代理只能做 ImageMagick 像素采样 + 静态分析
   （它自己也标注了这点）。因此**视觉方向的人工确认发生在数学/几何证据之外**，且是必须的一步 ——
   已做。子代理提的 4 条建议里我采纳 3 条，驳回 1 条：它说主按钮的强调色焦点环不可见，
   **判错了** —— `outline-offset:2px` 让环对着页面底色测量（5.51:1），而它建议的改法
   （换成 on-accent 色）反而会降到 1.05:1。
2. ✅ **已问过人类并获批准**：commit 本换皮（**不动**锁定页那个既有缺陷）→ 再构建/部署/线上验证。
3. ✅ **部署后已拿同一份脚本再打一次线上** —— **75/75 连跑 2 次**（详见下方「已部署并线上验证」）。
4. **既有缺陷（不是本轮引入，未修 —— 用户裁决留作后续）**：锁定态下 `<details class="meta">` 仍渲染
   「新密码 / 设置 / 去掉密码」—— 此刻点任何一个都必然 401。修它要动 `paint()` 状态机
   （锁定/解锁转换），而那个区域是本仓库陷阱最密集的地方，故本轮不动，只记录。
5. `pg_restore` 恢复演练仍未做（最大运维缺口）。

---

## 上一轮：步骤 ②（只读分享链接）已上线并验证

定位 = **webnote.cc 的开源版本**。线上 = **`172ea54`**（干净工作区构建，可追溯）。
步骤 ①（密码 + MIT LICENSE + 自动保存）与步骤 ②（只读分享链接 `/s/<token>`）都已上线。

### 步骤 ② 部署后的线上验证（都是实跑）

- 迁移已应用：`schema_migrations` = `0001` + `0002` + **`0003`**；`relay` 库里
  `clip_shares` 已存在（PK、`suffix` 索引、部分 `expires_at` 索引、
  `suffix → clips ON DELETE CASCADE`）。
- **runbook (a)-(d)**：本机 loopback 200；hcyj `curl 172.17.0.1:8790` = 200；
  caddy 容器 `wget` 可过；`HEAD https://clip.hcyj.xyz/api/health` = **200**；
  契约形状：`/api/nope` = JSON、`/a/b/c` = text/plain、`/neversuch` = 200、
  `robots.txt` = 200、`POST /ops1` = 405、`POST /s/<token>` = **405**、
  `/s/<未知 token>` = **404**。
- **runbook (e)+(f) 产品码 33/33**（当时用一个临时 Node 脚本逐项断言，
  runbook (e)/(f) 里的 curl 命令就是它的等价体）：密码那套 9 项全绿；
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
2. ~~GitHub 改名 + 本地目录改名~~ ✅ **已完成**（已核实）：`origin` = `github.com/zeroicey/clip`、
   仓库名已是 `clip`、本地目录 `/data/dev/clip`。
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
    （后来又部署过两次，**当前线上版本见本文件顶部**。）
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
