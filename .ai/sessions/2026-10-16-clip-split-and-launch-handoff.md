# clip 拆分与上线 — handoff

**Date:** 2026-10-16 · **Branch/Commit:** `main @ f5f3d03` · **Duration:** 一个长会话（含 DNS 排障与拆分）

## Where we ended

**clip 已完整上线**：`https://clip.hcyj.xyz` 走完整公网路径（TLS + hcyj Caddy + frp 隧道 +
本机 systemd 服务 + 本机 Postgres），Let's Encrypt 证书已签发，公网真浏览器 27/27、
大陆观测点（hcyj / koma）全绿。这个仓库已从「曾同时装两个服务的仓库」变成**纯 Go 的 clip 仓库**
（Node 工具链整条移除）。teleport 的重写迁到它自己的仓库（本地 `/data/dev/teleport-gin`），
**本次未开始写代码**，只有纲领与 P0/P1 的前置工作已在那边的 `.ai/` 里。

## What changed

| File / area | Change | Why |
| --- | --- | --- |
| 整仓 | 拆成两个仓库，本仓只剩 clip；`backend/` 提到仓库根，module → `github.com/zeroicey/clip` | 用户裁决：两个服务受众/风险面/发布节奏都不同 |
| `web/`、`schema.sql`、Vite/Tailwind/自托管字体、`package.json` | **删除** | clip 的页面是 `//go:embed` 的单文件 SSR HTML + nonce 内联脚本，不需要构建步骤 |
| `internal/clientip/` | 从 teleport 的 `httpx` 抽出 CopyIP 逻辑（含 4 个回归测试） | 两个独立仓库之间不引依赖；并**去掉** `TrustCloudflare` 开关（路径里永远没有 CDN） |
| `internal/clip/` | 新服务：Gin + pgx/sqlc + Postgres；后缀寻址、owner token、长轮询、图片嗅探 | 见 `.ai/decisions/2026-10-15-clip-service-design.md` |
| `internal/clip/api.go` | 未匹配的 `/api/*` 改为走 JSON 封套 | 线上验证抓出：曾是 `text/plain`，与所有正常响应形状不一致 |
| `scripts/verify-clip-browser.cjs` | 改用 Node 内建的 `WebSocket` 与 `fetch`，去掉第三方依赖 | 拆分删掉了 Node 工具链，原先 require 的 pnpm 路径失效；且 `node:http` 发不了 HTTPS |
| `scripts/` | `check.sh`（gofmt+vet+sqlc+test）取代 `pnpm run check` | 这个仓库不再有 Node |
| `.ai/` | 剪掉 teleport 域的记忆条目（随新仓库带走），新增 2 份决策、1 份 runbook、7 条 pitfall | 记忆库契约 |

## Verified / unverified

- [x] `scripts/check.sh` 全绿；对真库 `relay_test` 的 47 个 clip 测试全绿
- [x] **打线上**真浏览器 27/27（输入→保存→另一浏览器只读可见→不刷新自动同步→粘贴图上传→自清理）
- [x] 证书：Let's Encrypt，TLSv1.3，`Verify return code: 0`；HSTS `includeSubDomains; preload` 已下发
- [x] 大陆观测点 `hcyj`（ttfb 0.35s）、`koma`（0.19s）可达且证书校验通过
- [x] 既有服务未受影响：`api.hcyj.xyz/` 200、`api.hcyj.xyz/yeciorez/teleport/*` 200（**现已按用户指示停掉**，见下）
- [x] 生产库已清空验证残留（`clips=0 bytes=0`）
- [ ] **未验证**：`bjbuwe`（121.9.113.26）观测点 —— ssh 不可达，改用 hcyj/koma
- [ ] **未验证**：Postgres 的**恢复**流程（只验证过冷备的 `pg_dump` 命令执行，没做过 `pg_restore`）
- [ ] 未做：配额满时的行为（只在单元测试里验过，没在生产把库填满）

## Loose ends

- [ ] **本仓库在 GitHub 上仍叫 `zeroicey/teleport`**（remote 也是），需改名 `clip`；
      `zeroicey/clip` 名字可用，`gh` 已认证（`zeroicey`，有 `repo` scope）
- [ ] **本地目录仍叫 `/data/dev/teleport`** —— 改名要在**本会话结束之后**做（会话绑定 cwd）
- [ ] teleport 的新仓库在 GitHub 上还不存在（本地 `/data/dev/teleport-gin`，remote 未设置）
- [ ] **Postgres 备份未排期** —— 现在承载两个服务的数据（clip 的 `relay` 库 + teleport 的 `teleport` 库）。
      目前只有部署前的手工 `pg_dump`。见 `.ai/runbooks/deploy-clip.md` 的「已知约束」
- [ ] clip 的 `clips` 表**没有索引用于 janitor 之外的查询**（只有 `expires_at` 偏索引）—— 目前够用
- [ ] 图片总量接近 1 GiB 时要改走对象存储（本机已有 MinIO，需独立凭据）

## Do not repeat

- **别再假设 `.hcyj.xyz` 泛解析指向 hcyj。** 它曾经指向 `81.148.233.134`（**英国电信 BT 的地址**），
  而 `ping` 通、延迟稳定得像网络设备代答 —— ping 不能证明那是目标主机、也不能证明上面有网站。
  判断要用**权威 DNS 的回答 + 延迟量级 + 端口是否真在服务**三件事一起看。
- **别用 `http.get` 打 HTTPS 的验证脚本**（`Protocol "https:" not supported`）。
  这个仓库的验证脚本只用 Node 内建，`fetch` 两种协议都能发。
- **别在 `pkill -f` 里带上会匹配到自己命令行的字符串**（`pkill -f clip-smoke` 会把跑它的 shell 一起杀掉，
  现象是「命令无输出、退出码 143」）。用 `pkill -x`。
- **别把「两个标签页」当成「两台机器」**：同一浏览器的标签共享 localStorage，
  清掉 reader 的 storage 会连带清掉 writer 的 token。见
  `.ai/pitfalls/cases/two-tabs-are-not-two-machines.md`。
- **别靠 `go test` 缓存去守「读包外文件」的检查**：会返回缓存的 PASS，守卫静默不执行。
  用 shell 比较 + `-count=1`。见 `.ai/pitfalls/cases/guards-that-silently-did-not-guard.md`（teleport 仓库）。

## Next step

**这个仓库已经没有「下一步」了** —— 它是完成态，只等两件人的动作：

1. `gh repo rename clip`（或等人类在网页上改），然后
   `git remote set-url origin git@github.com:zeroicey/clip.git`。
2. 本地目录 `/data/dev/teleport` → `/data/dev/clip`。

**teleport 的重写在另一个仓库**：`/data/dev/teleport-gin`。新会话应从那里的
`.ai/CURRENT_TASK.md` 开始读（P0 ✅ / P1 ✅ / P2 未开工），
决策纲领在 `.ai/decisions/2026-10-16-rewrite-on-gin-and-postgres.md`（第一张表是 14 项冻结契约）。

**旧 teleport 服务已在本次会话停掉并禁用**（用户确认无人使用）：
- 停之前：版本 `dbca734`，二进制 26 MB，`teleport.db` 307200 字节 + 10 份备份 + WAL/SHM
- 停之后：`:8788` 已释放，公网 `/yeciorez/teleport/*` 返回 **502**（Caddy 块**刻意保留**，P6 指回来即可）
- 数据、二进制、`teleport.env`（含三个密钥）**全部原样留在 hcyj 的 `/data/services/teleport/`**，
  P5 的数据搬运与回滚都要用它们
- 想恢复：`ssh hcyj 'systemctl enable --now teleport'`
