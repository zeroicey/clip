# UI 美化 — 交接（步骤 ② 已完成并上线）

**Date:** 2026-10-16 · **Branch/Commit:** `main @ f9202f2`（线上跑 `172ea54`）· **Duration:** 一个长会话（步骤 ② 全过程 + 部署）

## Where we ended

**步骤 ②（只读分享链接 `/s/<token>`）已实现、已上线、已验证并写回记忆库。**
线上 = `172ea54`（干净树构建）；HEAD 之后的提交全是文档。`scripts/check.sh` 全绿、
真库 `scripts/test-pg.sh` 全绿、真浏览器打线上 **75/75 连跑 2 次**、runbook (a)–(f) 全绿、
大陆观测点 hcyj / koma 都 `HEAD=200 verify=0`。数据库是空的（还没被真人用过）。

**下一个会话的任务：美化 UI。** 本文档的「Do not repeat」就是给这件事写的 ——
这套页面有一批**不显眼但会静默失败**的约束（CSP、`hidden`、两种页面形状、被脚本钉住的文案），
先把那一节读完再动手。

## What changed（本会话）

| 提交 | 内容 |
| --- | --- |
| `9872d16` | **步骤 ②**：迁移 `0003_clip_shares.sql`、`share.go`、`api_shares.go`、`queries/shares.sql`、`page.html` 的分享区与只读 share 视图 |
| `172ea54` | 修**分享列表时序竞速**（本地 3 次全绿、打线上第一次就红）——页面 `await loadShares()` 再报成功，脚本等锚点 |
| `d321ee4` `0603c0c` `31e25b7` `f9202f2` | 部署写回 + 记忆库纠偏（decision 状态、`ARCHITECTURE.md` 补分享、过期待办） |

关键文件：`internal/clip/{share.go,api_shares.go,page.go,page.html}`、
`migrations/0003_clip_shares.sql`、`queries/shares.sql`、`internal/clip/share_test.go`、
`scripts/verify-clip-browser.cjs`（+31 条断言）。

## UI 相关的现状（美化时要动的地方）

| 位置 | 是什么 |
| --- | --- |
| `internal/clip/page.html` | **一个模板两种形态**：`<style nonce>` 在 L12–106，`<script nonce>` 在 L205–847。clip 编辑器 + 只读分享视图共用它 |
| `internal/clip/page.go` | `landingHTML`（首页，L339 起，内联 `<style>` 在 L346–~370）、`PageCSP`、`landingCSP`、`renderPage`、`renderSharePage`、`faviconSVG` |
| 设计系统 | 全是 CSS 变量 + `prefers-color-scheme`（无需构建、无外部资源、无 webfont） |
| 无障碍基线 | 已有 `aria-label`、`role="status" aria-live="polite"`、`outline:2px solid` focus-visible、按钮 36px / 首页 44px 触控高度 |

仓库里**已经有能用的技能**，先读它们：
`.agents/skills/web-design-guidelines/SKILL.md`、`.agents/skills/accessibility-review/SKILL.md`。

## Verified / unverified

- [x] `scripts/check.sh` 全绿；`sqlc diff` 无漂移
- [x] 真库 `TEST_PG_DSN=… scripts/test-pg.sh -count=1` 全绿（21 条分享测试）
- [x] 真浏览器**打线上** 75/75 ×2；本地 75/75 ×3
- [x] runbook (a)–(f) 线上全绿；迁移 `0001`+`0002`+`0003` 已应用
- [x] 冷备 `/data/backups/clip-20261016-172707.dump`（`pg_restore -l` 可读）
- [ ] **假定、未测**：手机真实布局（只有一条 `@media (max-width:520px)`，没用真手机看过）
- [ ] **假定、未测**：键盘 Tab 顺序、屏幕阅读器朗读（只有静态 `aria-*`，没做过 a11y 审计）
- [ ] **假定、未测**：两位真人在两台真机器上互传（只有脚本在跑；库里 0 条数据）
- [ ] **假定、未测**：Safari / Firefox（只在 Chromium 1248 里验过）

## Loose ends

- [ ] **Postgres 备份 + `pg_restore` 恢复演练**（记忆库标了三遍的「最大运维缺口」；`/data/backups/` 只有三份手工 dump，从未恢复进库过）
- [ ] 生产 `clip.env` 没显式写 `CLIP_UNLOCK_PER_HOUR` / `CLIP_SHARE_CODE_TTL`（用默认值，模板已补）
- [ ] `scripts/verify-clip-browser.cjs` 里 `chromium-1248` 路径写死（可用 `CHROME=` 覆盖，但默认值是硬编码的）
- [ ] 仓库是 **public**，而记忆库记录了完整拓扑（无凭据）；想收就说一声
- [ ] 功能候选：分享链接 **QR 码**、一键复制、后缀被占时一键换名、**中文后缀**、**阅后即焚**（后两个要先写 decision）
- [ ] teleport 重写版的切换（另一个仓库：`/data/dev/teleport-gin`）

## Do not repeat

UI 美化专用。这些每一条都会**静默**失败（页面看起来对、行为不对，或测试红在一个不像原因的地方）：

1. **CSP 会挡掉内联 `style="…"` 属性。** `style-src 'nonce-…'` 里的 nonce **不能**授权 style 属性
   （CSP3：非不能授权 attribute；要 `'unsafe-inline'`/`'unsafe-hashes'` 才行）。所以
   **加 `style="width:300px"` 不生效，还会记一条 CSP violation**，而真浏览器脚本的
   console-noise 断言会因此变红。改样式一律写进那个 nonce 的 `<style>` 块里的选择器，
   或用 JS 设 `el.style.x`（CSSOM 不受 CSP 管）。见
   `.ai/pitfalls/cases/2026-10-12-csp-blocks-the-assets-the-page-links.md`。
2. **一个外部来源都没有**：无 CDN、无 webfont、无外链 CSS/JS。`default-src 'none'`、
   `img-src 'self' data:`、`connect-src 'self'`。想加字体只能内联或系统字体栈。
3. **别把 `[hidden]` 的规则弄掉。** 现有 CSS 有一条
   `button[hidden],label[hidden],div[hidden],section[hidden]{display:none}`
   —— 因为一旦给 `button` 之类加了 `display:inline-flex`，UA 的 `[hidden]` 默认样式就被
   盖掉，「保存/删除」会在**只读分享页上**重新出现（页面在撒谎，且
   `the share view offers no save button` 会红）。改样式时保留这条，或改用 class 控制显示。
4. **一个模板两种形态。** `page.html` 同时渲染 clip 编辑器与只读分享视图；分享侧
   `data-can-write="false"`，且 `page.html` 里的 `writable = !isShare;` 被
   `TestShareWireContract` 钉死（曾经解锁后把只读分享变成编辑器，见
   `.ai/pitfalls/cases/2026-10-16-share-page-unlocked-into-an-editor.md`）。美化不能把它变回可写。
5. **元素 id 与部分界面文案被脚本断言**。改文案/改 id 是允许的，但**必须在同一次改动里
   更新 `scripts/verify-clip-browser.cjs`**（它就是守卫，不是障碍）。被依赖的 id：
   `body content hint lockbox lockstatus pw save setpw del sharebox sharecodeout sharecodevalue sharelist sharestatus status`；
   被断言的中文串：`已自动保存`、`已设密码`、`解锁中`、`公开`、`密码`、`频繁`、`已创建`、
   `已撤销`、`失效`、`还没有`。另外不要改 `data-state="locked"` / `data-can-write` 这些属性名
   （契约测试与脚本都在用）。
6. **textarea 的 `white-space:pre` 是刻意的** —— 转移日志/命令时换行回流就是内容被改坏。
   别改成 `pre-wrap`（想换行就加横向滚动或提供预览，不要动它）。
7. **保留触控高度与 focus 可见性**（按钮 36px、首页 44px、`outline` focus-visible）。
   `.agents/skills/accessibility-review` 里有清单。
8. **配色别写死**：全部走 `:root` 变量 + `prefers-color-scheme:dark` 覆盖，否则暗色模式坏掉。
9. **响应字段名不能用错**：响应 snake_case、请求 camelCase，页面读 `data.x` / `d.x`；
   `TestClientReadsTheFieldNamesTheServerWrites` 钉住（写错是静默的）。
10. **验证脚本会吃掉写预算**：一次跑约 **10** 次写，而 `CLIP_WRITE_PER_HOUR=30` 是
    **按源地址、所有后缀一个桶**。一小时内跑三次以上就 429，且伪装成无关故障。脚本会以
    **退出码 3** 说明；限流器是进程内的，`systemctl restart clip` 即清空。
11. **本机绿 ≠ 线上绿。** 本会话本地 3 次全绿，打线上第一次就红（时序竞速，见
    `.ai/pitfalls/cases/2026-10-16-share-page-unlocked-into-an-editor.md` 的「同族」节）。
    **部署后必须拿同一份脚本再打一次线上**。另外别拿本机观察到的 HTTP 格式写断言
    （本地 HTTP/1.1 有 reason phrase，线上经 Caddy 是 HTTP/2，没有）。
12. **`go test` 会返回缓存的 PASS**：守路由/读文件这类检查一律 `-count=1`。
13. **只从干净工作区构建**，部署后用 `version` 核对（比的是「`version` 之后没有 `cmd/ internal/ scripts/` 改动」，不是「等于 HEAD」）。

## Next step

1. 先定视觉方向（配色/间距/字号/组件形态），读
   `.agents/skills/web-design-guidelines/SKILL.md` 与 `.agents/skills/accessibility-review/SKILL.md`。
2. 改 `internal/clip/page.html`（`<style nonce>` + 标记）与 `internal/clip/page.go` 的
   `landingHTML`（若首页在范围内）。**先只改样式，别动 id/行为**，把风险降到最低。
3. 本地起一个服务打真库跑真浏览器（这台机器上 `sqlc`/`go`/Chromium 都在）：

   ```bash
   cd /data/dev/clip
   scripts/check.sh
   PW=$(sudo grep -oP '(?<=postgres://relay:)[^@]+' /data/services/clip/clip.env)
   TEST_PG_DSN="postgres://relay:$PW@127.0.0.1:5432/relay_test?sslmode=disable" scripts/test-pg.sh -count=1
   go build -o /tmp/clip-ui ./cmd/clip
   DATABASE_URL="postgres://relay:$PW@127.0.0.1:5432/relay_test?sslmode=disable" \
     PUBLIC_BASE_URL=http://127.0.0.1:8799 LISTEN_ADDR=127.0.0.1:8799 ENVIRONMENT=production \
     setsid nohup /tmp/clip-ui > /tmp/clip-ui.log 2>&1 < /dev/null &
   node scripts/verify-clip-browser.cjs http://127.0.0.1:8799   # 期望 75/75
   ```

   （`CHROME=/path/to/chrome` 可覆盖浏览器路径。）
4. 截图想要证据就设 `VERIFY_OUT`（默认落在 `bin/e2e/localtest/`，`bin/` 已 gitignore）；
   跑完把关键截图给人看一眼再部署。
5. 提交 → `scripts/build.sh` → `git commit/push`（**先问人类**）→ 走
   `.ai/runbooks/deploy-clip.md` → **部署后再打一次线上真浏览器** → 写回
   `.ai/CURRENT_TASK.md`（UI 改动的写回至少要列：改了哪些 id/文案、脚本是否同步改过）。
