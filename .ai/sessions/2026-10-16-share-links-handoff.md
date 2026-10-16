# 分享链接（步骤 ②）— 交接

> **已落地（2026-10-16 第三会话）。** 本文档是当时的**计划**，现已全部实现并上线：
> 线上 `172ea54`，迁移 `0003_clip_shares.sql`，真浏览器打线上 75/75 ×2。
> 下面「步骤 ② 未开工」之类的说法已是历史；**当前状态看
> `.ai/CURRENT_TASK.md` 与 `.ai/sessions/2026-10-16-ui-beautification-handoff.md`。**
> 落地时定下的细节（“不泄漏存在性”让位、三种口令形态、取件码 TTL 上界、接管清分享等）
> 已写进 `.ai/decisions/2026-10-16-clip-share-links.md` 的「落地补充」。

**Date:** 2026-10-16 · **Branch/Commit:** `main @ fbddae5`（线上跑 `9126d5c`）· **Duration:** 一个长会话（步骤 ① 全过程）

## Where we ended

**步骤 ① 全部完成、已上线、已验证**：可选访问密码（argon2id，密码取代 owner token）、
四条读路径全部上锁、MIT LICENSE、防抖自动保存。

**步骤 ② 未开工**：`clip_shares` 表不存在，只有一份已裁决的设计
（`.ai/decisions/2026-10-16-clip-share-links.md`）。前置条件已就绪：`internal/clip/suffix.go`
里 `s` 与 `k` 早就是保留字。

当前状态（都已核实）：HEAD `fbddae5` = origin，工作区干净；线上 `9126d5c`，
三条连通性检查全绿；迁移 `0001` + `0002` 已应用。

## What changed（本会话的提交）

| 提交 | 内容 |
| --- | --- |
| `13ad95d` | 修 HEAD 掉进 NoRoute（部署检查自己报 404）；补 HEAD 孪生 + 公共路径 405 |
| `07641c5` | 那次部署的写回（部署产物可追溯、回滚件与冷备位置） |
| `fb8d17a`/`eb3657f` | 定位改为 webnote 开源版；三份 decision 提案 → 逐条裁决 |
| `67dd9cf` | 设计交接（当时代码未写） |
| `acc35c6` | **步骤 ①**：密码取代 owner token、四条读路径上锁、MIT LICENSE、自动保存 |
| `9126d5c` | 修**重叠解锁竞态**（过期响应擦掉已成功凭据） |
| `fbddae5` | 步骤 ① 的部署写回 + 自测刷爆写预算的运维记录 |

关键文件：`internal/clip/password.go`（新）、`migrations/0002_password.sql`（新）、
`queries/clips.sql`、`store.go`、`api.go`、`page.go`+`page.html`；
`token.go` **已删除**。

## Verified / unverified

- [x] `scripts/check.sh` 全绿；`scripts/test-pg.sh`（真库 `relay_test`）全绿
- [x] **真浏览器 43/43，连跑 4 次全绿**（对线上；修竞态前是间歇性 41/42）
- [x] 线上密码那套 11 项：无口令 `/raw` 401 且不泄正文、带口令 200、页面锁态、
      **页面 HTML 里搜不到正文**、`GET /api` 401、错口令 401、unlock 200、无口令不能删
- [x] 迁移已应用；线上 `version` == 提交；实体回滚件在 `/data/backups/`
- [ ] **步骤 ②（分享链接）一行代码都没写**
- [ ] `pg_restore` **真正恢复到库里的演练仍未做过**（仍是最大运维缺口）

## Loose ends

- [ ] 步骤 ②（下面「Next step」）
- [ ] `pg_restore` 恢复演练；备份未排期自动化
- [ ] `bjbuwe`（121.9.113.26）观测点 ssh 不可达，大陆观测点只剩 hcyj / koma
- [ ] 备份里的 `clip.env` 凭据不在任何备份中（忘了 DSN 就恢复不了库）

## Do not repeat

- **自测会把写预算刷爆**：`CLIP_WRITE_PER_HOUR=30` 是**按源地址、所有后缀一个桶**，
  而 `scripts/verify-clip-browser.cjs` 一次跑约 7 次写 → 一小时跑四次以上就 429，
  症状伪装成无关的功能故障（clip 没设上密码、清理 404、旁观者没切锁）。
  脚本现在识别它并以**退出码 3** 说明；**限流器是进程内的**，`systemctl restart clip` 即清空。
- **别拿本地观察到的 HTTP 格式写断言**：本地 curl 是 HTTP/1.1（`401 (Unauthorized)`），
  线上经 Caddy 是 HTTP/2，reason phrase 为空（`401 ()`）—— 按本地格式写的过滤器在线上静默失效。
  这与 `2026-10-10-rate-limit-collapsed-to-one-bucket.md` 是同一类错误。
- **`curl -sI … | head -3` 不要用来判定线上**：hpcore 有 `http_proxy`，第一行是代理的
  `HTTP/1.1 200 Connection established`，会把真正的 404 挤到下面看起来像成功。
- **argon2 进不了 SQL**：所有权判定仍要写在写语句的 `WHERE` 里 ——
  把「刚校验过的哈希」当 `@proof_hash` 传进去。**绝不 read-then-write。**
- **`clientip.ClientIP` 对直连忽略 `X-Forwarded-For`**（这是它的安全性质）：
  测试要模拟两个客户端就改 `RemoteAddr`，**伪造转发头不会造出新桶**。
- **`pg_restore -l -` 读不了 CUSTOM 归档**（需要可寻址文件）→ 先 `docker cp` 进容器。
- **部署后别断言 `version == HEAD`**：纯文档提交会让 HEAD 往前走。用
  「`cat-file -e $V`」+「`$V..HEAD` 之间没有 `cmd/ internal/ scripts/` 改动」。
- **`go test` 会返回缓存的 PASS**：守路由/读文件这类检查一律 `-count=1`。
- **别信记忆库里过期的「下一步」**：先核事实（本会话发现两条早就做完了）。

### 下一会话会撞上的两条（已经想过，不是意外）

1. **6 位取件码只有 10⁶ 种可能** —— 必须配**短 TTL + 尝试上限**，否则可被枚举完。
   服务端生成，不要超过 clip 自己的过期时间。
2. **“不泄漏存在性”对分享链接会让位**：`/s/<token>` 不存在 vs 口令错，用户需要能分清，
   否则体验是“链接坏了”。取舍要写进 decision 的后果，别让它看起来像疏忽。

## Next step

按顺序做 **步骤 ②：只读分享链接**。设计已裁决，先读
`.ai/decisions/2026-10-16-clip-share-links.md`（内含路由形状、两种口令、只读/可撤销/TTL、
cascade、不复制内容），然后：

1. **迁移 `0003_clip_shares.sql`**：
   `clip_shares(token PK, suffix → clips ON DELETE CASCADE, password_hash TEXT NULL,
   code_hash TEXT NULL, expires_at BIGINT, created_at BIGINT)` + `suffix` 上的索引。
   token 服务端生成、高熵（base64url 16–22 字符）、**不走 `ValidateSuffix`**。
   时间一律 epoch 毫秒、`expires_at = 0` = 不过期（全局约定）。然后 `sqlc generate`。
2. **store**：创建/查询/列出/撤销分享；口令复用 `password.go` 的
   `hashPassword`/`verifyPassword`；取件码 6 位数字 + 短 TTL。
   分享**不复制内容**，指同一份（主人改了分享方刷新即见）。
3. **API**：`POST/GET /api/clips/:suffix/shares`、`DELETE .../shares/:token`（要主密码）；
   读侧 `GET /s/:token`、`/s/:token/raw`、`/s/:token/raw/:n`，口令仍走 `X-Clip-Password`。
   **`/s/...` 上只挂 GET/HEAD**（写形状 → 405，与公共路径方法闸门同一套写法）。
4. **限流**：分享口令尝试要限流（复用 `unlockLimiter` 的 `ip:`/`suffix:` 双桶思路，
   键换成 share token）；取件码尤其。
5. **页面**：`/s/:token` 只读视图（可复用 `page.html` 的只读/锁定分支，加一个 share 状态）；
   主页面加「分享」区域（生成、复制链接、撤销）。
6. **测试**：真库（只读性、口令、撤销、cascade、过期、限流、不泄漏存在性、
   **每个读入口都断言“不带口令拿不到正文”**）+ 真浏览器（主人建分享 → 另一浏览器只读可见 →
   没有写入口 → 撤销后立刻失效）。
7. **部署 + 验证**：走 `.ai/runbooks/deploy-clip.md`（含密码那套步骤）；
   注意自测刷爆写预算 → 必要时 `systemctl restart clip`。
