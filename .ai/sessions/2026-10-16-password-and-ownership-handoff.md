# 密码 / 归属重构 — 交接

**Date:** 2026-10-16 · **Branch/Commit:** `main @ eb3657f` · **Duration:** 一个长会话（先修 HEAD 上线，再做 webnote 对标设计）

## Where we ended

本会话先按上一份 handoff 跑三条连通性检查，抓出并**修好、部署、验证**了一个线上缺陷
（HEAD 掉进 NoRoute → 部署检查自己报 404）。线上现为 `13ad95d`，可追溯。

随后用户把定位定为 **webnote.cc 的开源版本**，并逐条裁决了密码/归属/分享三套设计
（三份 decision 已 ✅ ACCEPTED，见下）。**代码一行没写**：下一步是实现，顺序用户已选
「① 密码（含 MIT LICENSE + 自动保存）→ ② 分享」。

## What changed

| File / area | Change | Why |
| --- | --- | --- |
| `internal/clip/api.go` | 每条 GET 路由补 HEAD 孪生；公共路径只服务 GET/HEAD，其余 405 + `Allow` | 部署检查 `curl -I` 返回 404；监控/CDN 用的正是 HEAD |
| `internal/clip/api_test.go` | `+TestEveryGetRouteAlsoAnswersHead`（无需数据库，跑在 `check.sh`） | `check.sh` 跳过所有 DB 测试，路由回归需要一层不依赖库的守卫 |
| `internal/clip/contract_test.go` | `+TestHeadMirrorsGetOnEveryRoute`、`+TestPublicPathsOnlyAnswerReadMethods` | 用真 `httptest.NewServer`：body 丢弃是 net/http 在传输层做的，`ResponseRecorder` 会把它藏起来 |
| `.ai/pitfalls/cases/2026-10-16-head-is-not-get-in-gin-routing.md` | 新增 | 同族第 2 个坑，已与 `gin-noroute-presets-404.md` 交叉引用 |
| `.ai/runbooks/deploy-clip.md` | 检查 c) 改成可执行形式 + 契约/来源校验 + 回滚与冷备验法 | 原检查写成 `curl -I \| head -3` 会把失败藏在代理行下面，且从没人执行过 |
| `.ai/decisions/2026-10-16-clip-access-password.md` | ✅ ACCEPTED | argon2id 哈希、**挡 4 条读路径含 `/raw`**、unlock + `X-Clip-Password`、忘密码不可恢复 |
| `.ai/decisions/2026-10-16-clip-open-edit-and-ownership.md` | ✅ ACCEPTED | **无密码 = 谁都能改**，密码是唯一凭据，**owner_token 整套删掉** |
| `.ai/decisions/2026-10-16-clip-share-links.md` | ✅ ACCEPTED | `/s/<token>` 只读；分享密码 **与** 6 位取件码；独立于主密码；可撤销 + TTL |
| `.ai/CURRENT_TASK.md` | 定位段 + webnote 对齐表 + 待实现清单 | 定位变了，记忆库必须同步 |

## Verified / unverified

- [x] `scripts/check.sh` 全绿；`scripts/test-pg.sh`（真库 `relay_test`）全绿
- [x] **部署后线上**：三条检查全绿、契约 9/9、**真浏览器 27/27**、hcyj/koma 观测点全绿
- [x] 线上 `version` = `13ad95d`，`git cat-file -e` + 「之后无代码改动」双重核对通过
- [x] 反例已验证：stash 掉修复，两个新守卫按线上现象失败
- [x] 冷备可解析（`pg_restore -l`，15 个 TOC 项）；旧二进制存 `/data/backups/clip-binary-57dd925-dirty`
- [ ] **密码功能本身：未写一行代码**（设计已定，实现未开始）
- [ ] `pg_restore` **真正恢复到库里的演练仍未做过**（仍是最大运维缺口）

## Loose ends

- [ ] 实现步骤 ①：迁移 + sqlc + store（密码取代 token）→ API 四读路径闸门 + unlock → 页面（解锁壳 + 自动保存）→ MIT LICENSE → 测试与真库/真浏览器验证
- [ ] 实现步骤 ②：`/s/<token>` 分享（排在密码之后，要复用密码那步建好的闸门与口令传递）
- [ ] 文档债务：AGENTS.md 的「写要 owner token」与「后缀是公屏不是密码」两条硬约定要随代码一起改
- [ ] 存量浏览器里的 `owner_token` 会失效（用户已接受；发布说明要写一句）
- [ ] bjbuwe（121.9.113.26）观测点已不可达，只剩 hcyj / koma

## Do not repeat

- **别用 `curl -sI … | head -3` 判定线上**：hpcore 有 `http_proxy`，第一行是代理的
  `HTTP/1.1 200 Connection established`，会把真正的 404 挤到下面看起来像成功。用 `-w '%{http_code}'`。
- **别以为 GET 通了 HEAD 就通**：Gin 方法匹配是精确的，没注册 HEAD 就落到 `NoRoute`，
  而那里的分发器把第一段当后缀 → 静态路径变 400、`/api/*` 变 404。
- **`pg_restore -l -` 读不了 CUSTOM 归档**（需要可寻址文件），会报 `cannot open input file "-"`。
  先 `docker cp` 进容器再 `pg_restore -l`。
- **部署后别断言 `version == HEAD`**：纯文档提交会让 HEAD 往前走而二进制不动。正确断言是
  「`$V` 在历史里」+「`$V..HEAD` 之间没有 `cmd/ internal/ scripts/` 的改动」。
- **`go test` 会返回缓存的 PASS**：守路由/读文件这类检查一律 `-count=1`。
- **别信记忆库里过期的「下一步」**：本会话发现两条早就做完了（remote 已是 `zeroicey/clip`、
  目录已是 `/data/dev/clip`）。先核事实再动手。

### 下一会话实现时会撞上的两件事（已经想过，别当成意外）

1. **argon2 校验没法写进 SQL**。本仓库的硬模式是「所有权判定写在 `WHERE` 里，一次语句完成，
   没有 read-then-write 竞态」，但 argon2 需要明文 + 存储的 salt，只能在 Go 里算。
   保持原子性的写法是：Go 里校验通过后，**把「刚刚校验过的那个哈希」当条件传进 `WHERE`** ——
   期间密码若被改过，存储的哈希已不同 → 0 行 → 404。不要退化成 read-then-write。
2. **「不泄漏后缀是否存在」这条性质对带密码的 clip 会让位**：页面必须显示「输入密码」才能用，
   这本身就暴露了「这个后缀存在且受保护」（webnote 同样如此）。据此选择**对用户有用**的响应
   （`/raw` 与 API 返回 401 + 一句提示「这个 clip 需要密码」），而不是假装 404。
   这是个刻意的取舍，要写进 decision 的后果里。

## Next step

从 **步骤 ①-a** 开始：`internal/clip/migrations/0002_*.sql` —— 加 `password_hash TEXT`（NULL = 无密码，
与存量数据天然兼容）、删 `owner_hash`；然后改 `internal/clip/queries/clips.sql` 并 `sqlc generate`
（闸门里的 `sqlc diff` 会盯漂移）。接着按 `internal/clip/store.go` → `api.go` → `page.html` 的顺序推进，
每步保持 `scripts/check.sh` 能跑；安全模型改完后**必须**跑 `scripts/test-pg.sh` 与真浏览器。
