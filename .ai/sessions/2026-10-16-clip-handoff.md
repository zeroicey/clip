# clip —— 交接（新会话从这里开始）

**Date:** 2026-10-16 · **Repo:** `clip`（全新历史，1 个提交 `931452b`）· **State:** ✅ 已上线

---

## 一、人类要执行的命令

```bash
# 1. 改文件夹名（必须在旧会话结束后做，会话绑定 cwd）
mv /data/dev/teleport /data/dev/clip

# 2. 在这个文件夹里启动 pi（本仓库之后的维护都在这里）
cd /data/dev/clip && pi
```

GitHub 那边已经由 AI 用 `gh` 建好并推送（`zeroicey/clip`），remote 已设置。

---

## 二、给下一个会话（AI）的话

**你的验证职责只有一个：保证这台电脑（hpcore）能连上 hcyj 的服务器。** 别的都不要管 ——
不要动 hcyj 的 Caddy、不要碰本机的 Postgres 容器配置、不要管另一个仓库（teleport）的事。

「能连上 hcyj」的**确切含义**（跑这三条就算完成，不需要更多）：

```bash
# a) 本机 → hcyj 的公网入口（走完整路径：TLS + Caddy + frp 隧道 + 本机服务）
curl -sI https://clip.hcyj.xyz/api/health | head -3

# b) 证书有效（TLS verify = 0）
curl -s -o /dev/null -w 'verify=%{ssl_verify_result}  http=%{http_code}\n' https://clip.hcyj.xyz/

# c) 隧道那一跳真的通（用 curl，**不要用 ping**）
ssh hcyj 'curl -s -o /dev/null -w "%{http_code}\n" http://172.17.0.1:8790/api/health'
```

三条都 200 / `verify=0` = 链路健康。任何一条失败，**先读** `.ai/runbooks/deploy-clip.md`
的「验证」与「已知约束」，再动手。

---

## 三、当前状态（读一遍就够）

| 项 | 值 |
| --- | --- |
| 公网入口 | `https://clip.hcyj.xyz`（第一个路径段就是后缀：`/ops1`） |
| 服务 | 本机 systemd `clip.service`，`127.0.0.1:8790`，User=`relay` |
| 转发 | `frpc-relay-clip.service`（**systemd --user**，`Linger=yes`）→ hcyj frps `:8790` |
| 反代 | hcyj 的 docker 容器 `caddy`，`reverse_proxy 172.17.0.1:8790` |
| 数据库 | 本机 `postgres` 容器，`relay` 角色 + `relay` 库；测试用 `relay_test` |
| 证书 | Let's Encrypt，Caddy 自动续期 |
| 闸门 | `scripts/check.sh`（gofmt + vet + sqlc 漂移 + test） |
| 真库测试 | `TEST_PG_DSN=... scripts/test-pg.sh`（DSN 库名必须含 `test`，否则拒绝运行） |
| 真浏览器 | `scripts/verify-clip-browser.cjs [url]`（只用 Node 内建 + Chrome；`CHROME=` 可覆盖浏览器路径） |

**验证记录**：公网真浏览器 **27/27**、HTTP 冒烟 42/42、大陆观测点 hcyj / koma 全绿、
证书 `Verify return code: 0`。既有服务（`api.hcyj.xyz`）未受影响。

---

## 四、仓库历史是刻意的「全新」

这个项目曾经和 teleport 挤在同一个仓库里。2026-10-16 拆开时用户裁决：
**clip 重新开一个新项目、不要 teleport 的历史**；teleport 那边则接着它自己的旧历史继续。

- 本仓库：**1 个提交**，全新历史。
- 旧共用历史（含拆分前的 35 个提交）留在仓库外做备份：
  `/data/dev/.teleport-shared-history-20261016.bundle`
  （需要时 `git clone <bundle> /tmp/old` 查看；不要放进仓库。）
- teleport 的提交与历史在 **teleport 仓库**里继续。
- 拆分理由与两个仓库的关系：`.ai/decisions/2026-10-16-two-repositories.md`。

---

## 五、不要重复的坑（详情见 `.ai/pitfalls/cases/`）

- **别再假设 `.hcyj.xyz` 泛解析指向 hcyj**：它曾经指向 `81.148.233.134`（**英国电信 BT 的地址**），
  而 `ping` 通、延迟稳定得像网络设备代答。判断要用**权威 DNS 回答 + 延迟量级 + 端口是否真在服务**。
- **别用 ping 证明隧道通**：hcyj → hpcore 的**入向 TCP 是死的**（derp 中继、`direct connection not established`），
  隧道必须由本机拨出。验证要用 `curl`。
- **`scripts/verify-clip-browser.cjs` 的「两个标签页」不是「两台机器」**：同一浏览器共享 localStorage，
  清 reader 的 storage 会连带清掉 writer 的 token。
- **`pkill -f <字符串>` 会匹配到自己**（现象：命令无输出、退出码 143）。用 `pkill -x`。
- **别靠 `go test` 缓存守「读包外文件」的检查**：会返回缓存的 PASS。用 shell 比较 + `-count=1`。

---

## 六、还没做的（都记在 `.ai/CURRENT_TASK.md`）

- **Postgres 备份未排期**（现在只承载 clip 的 `relay` 库；teleport 的库还没搬数据）。
  `pg_restore` **从未演练过** —— 这是最大的运维缺口。
- 二维码/一键分享、「后缀被占」时的一键换名、移动端适配。
- `bjbuwe`（121.9.113.26）观测点 ssh 不可达，大陆观测点只剩 hcyj 与 koma。
