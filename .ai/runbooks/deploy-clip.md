# 部署 clip（线上已就绪）

**Applies to:** clip 服务（后端在 `hpcore`，公网入口在 `hcyj`）
· **Owner:** lead · **Last verified:** 2026-10-16（证书已签发、公网 27/27 真浏览器 + 大陆观测点全绿）；
同日第二会话：修复 HEAD 掉进 NoRoute 后重新部署（`13ad95d`），三条检查 + d) 契约 9/9 +
真浏览器 27/27 + hcyj/koma 观测点全绿

## 形态

```text
浏览器 → clip.hcyj.xyz → hcyj 的 Caddy(:443, docker)
                             │  reverse_proxy 172.17.0.1:8790
                          frps(:7000 控制口, :8790 转发口)
                             ▲  隧道由 hpcore 主动拨出（frpc, systemd --user）
                             │
        hpcore:127.0.0.1:8790  clip (systemd, User=relay)
                             │
        PostgreSQL 容器(127.0.0.1:5432)  database=relay, role=relay
```

**顺序不能反**：后端先起 → 隧道起 → Caddy 才有人可代理。
反之 Caddy 会 502，而且**看起来像 DNS/证书问题**。

## 现场事实

| 项 | 值 |
| --- | --- |
| 本机 | `hpcore`，Tailscale `100.64.0.1`，Arch，systemd |
| hcyj | `100.64.0.4` / 公网 `8.148.233.134`（阿里云广州）；`:443` 是 **docker 容器 `caddy`** |
| 服务账号 | `relay`（system 用户，无登录 shell） |
| 服务目录 | `/data/services/clip/`（`relay:relay` 0750）；二进制 + `clip.env`（**600 root:root**） |
| unit | `/etc/systemd/system/clip.service`（模板 `deploy/clip.service`） |
| 隧道 | `/data/services/frp/conf/frpc-relay-clip.toml` + **user** unit `frpc-relay-clip.service`（`Linger=yes`） |
| 数据库 | 本机 `postgres` 容器；role `relay` + db `relay`（非超级用户）；测试用 `relay_test` |
| 证书 | Let's Encrypt，Caddy 自动签发与续期（`/data/caddy/certificates/`） |

## 部署（升级）

```bash
cd <clip repo>
scripts/check.sh                                    # 闸门必须先绿
TEST_PG_DSN=... scripts/test-pg.sh                  # 改过存储语义时必须（对真库）
CMD=clip scripts/build.sh                           # → bin/clip-linux-amd64

# 升级前冷备（迁移不可逆）
PW=$(sudo grep -oPm1 '(?<=postgres://relay:)[^@]+' /data/services/clip/clip.env)
docker exec -e PGPASSWORD="$PW" postgres pg_dump -U relay -d relay -Fc \
  > /data/backups/clip-$(date +%Y%m%d-%H%M%S).dump

sudo install -m 0750 -o relay -g relay bin/clip-linux-amd64 /data/services/clip/clip
sudo systemctl restart clip && sleep 2 && systemctl is-active clip
```

迁移在启动时自动应用；`clip migrate` / `clip sweep` 可单独跑。

## 验证（每次部署都走）

```bash
# a) 本机直连（绕过所有网络层）
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8790/api/health

# b) 隧道那一跳 —— 用 curl，不要用 ping
ssh hcyj 'curl -s -o /dev/null -w "%{http_code}\n" http://172.17.0.1:8790/api/health'
ssh hcyj 'docker exec caddy wget -q -O /dev/null -T 10 http://172.17.0.1:8790/api/health && echo REACHABLE'

# c) 完整公网路径（HEAD —— 路由表必须有 HEAD 孪生，否则 404；
#    见 pitfalls/cases/2026-10-16-head-is-not-get-in-gin-routing.md）
curl -sI -o /dev/null -w '%{http_code}\n' https://clip.hcyj.xyz/api/health   # 必须 200
# 别写成 `curl -sI ... | head -3`：hpcore 上有 http_proxy，第一行是代理的
# "HTTP/1.1 200 Connection established"，会把真正的 404 挤到下面看起来像成功。

# d) 契约形状（改过路由或 API 时必须）
curl -s -o /dev/null -w '%{content_type}\n' https://clip.hcyj.xyz/api/nope   # 必须是 application/json
curl -s -o /dev/null -w '%{content_type}\n' https://clip.hcyj.xyz/a/b/c       # 页面路径是 text/plain
curl -s -o /dev/null -w '%{http_code}\n' https://clip.hcyj.xyz/neversuch      # 200（第一次访问即创建）
curl -sI -o /dev/null -w '%{http_code}\n' https://clip.hcyj.xyz/robots.txt    # 200（不是 400）
curl -s -X POST -o /dev/null -w '%{http_code}\n' https://clip.hcyj.xyz/ops1    # 405（公共路径只服务 GET/HEAD）

# e) 密码那套（改过密码/读路径时必须）—— 用一个真 clip 走一遍：
SFX=deploycheck$RANDOM
curl -s -X PUT -H 'Content-Type: application/json' \
  -d '{"body":"PRIVATE-BODY","password":"deploy-check-pw"}' https://clip.hcyj.xyz/api/clips/$SFX >/dev/null
curl -s -o /dev/null -w '%{http_code}\n' https://clip.hcyj.xyz/$SFX/raw                       # 必须非 200
curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Clip-Password: deploy-check-pw' https://clip.hcyj.xyz/$SFX/raw   # 必须 200
curl -s https://clip.hcyj.xyz/$SFX | grep -c 'data-state="locked"'                            # 必须 1
curl -s https://clip.hcyj.xyz/$SFX | grep -c 'PRIVATE-BODY'                                    # 必须 0（正文不进 HTML）
curl -s -X POST -H 'Content-Type: application/json' -d '{"password":"deploy-check-pw"}' \
  https://clip.hcyj.xyz/api/clips/$SFX/unlock >/dev/null                                       # 解锁应 200
curl -s -X DELETE -H 'X-Clip-Password: deploy-check-pw' https://clip.hcyj.xyz/api/clips/$SFX  # 清理（204）

# f) 真库测试
TEST_PG_DSN=... scripts/test-pg.sh

# g) 真实浏览器（改过页面/脚本时必须；只依赖 Node 内建，无第三方包）
node scripts/verify-clip-browser.cjs https://clip.hcyj.xyz

# h) 部署产物能追溯到提交（见「已知约束」里线上那版无法复现的事）
git -C <clip repo> status --porcelain                                          # 构建前必须为空
V=$(curl -s https://clip.hcyj.xyz/api/health | grep -o '"version":"[^"]*"' | cut -d'"' -f4)
git -C <clip repo> cat-file -e "$V"                                            # 这个提交必须在历史里（-dirty 会在这里挂）
git -C <clip repo> diff --stat "$V"..HEAD -- cmd/ internal/ scripts/           # 之后没有代码改动
# 注意比的是「$V 之后没有代码改动」，不是「$V == HEAD」：部署后的纯文档提交
# （.ai/ 之类）本来就会让 HEAD 往前走，做到等号就等于禁止事后写记忆库。
```

**大陆观测点**：`hcyj`（阿里云广州）、`koma`（腾讯云 124.221.144.97）。
`bjbuwe`（121.9.113.26）在 2026-10-16 已不可达，改用上面两个。境外观测点测不出大陆封锁。

## 单条操作

```bash
sudo systemctl restart clip
sudo journalctl -u clip -n 50 --no-pager          # JSON 日志
systemctl --user status frpc-relay-clip
sudo -u relay /data/services/clip/clip sweep      # 手动清一次过期（janitor 每 5 分钟自己跑）
```

## 关掉写入（不改代码、不丢数据）

```bash
sudo sed -i 's/^CLIP_WRITES_ENABLED=.*/CLIP_WRITES_ENABLED=false/' /data/services/clip/clip.env
sudo systemctl restart clip     # 读/raw/图片照旧，PUT/POST/DELETE 返回 503 writes_disabled
```

## 密码相关的旋钮

| 环境变量 | 默认 | 作用 |
| --- | --- | --- |
| `CLIP_UNLOCK_PER_HOUR` | 20 | 每个源地址、每个后缀允许的**失败**解锁次数（成功的不过数，见 `clipAccess`） |
| `CLIP_WRITE_PER_HOUR` | 30 | 每个源地址的**写**预算（创建/更新/设密码/传图/删除**共用一个桶**） |

### 自测会把写预算刷爆（踩过，别当成产品 bug）

`CLIP_WRITE_PER_HOUR=30` 是**按源地址、所有后缀共用一个桶**的，而
`scripts/verify-clip-browser.cjs` **一次跑约 7 次写**。所以同一个出口 IP 一小时内跑四次以上，
就开始出现 429，而它伪装成一堆无关的功能故障：某个 clip 一直没设上密码、清理时 404、
旁观者页面没切到锁。症状看起像产品坏了，实际是自己把自己限了。

- 脚本现在能识别这种情况：检测到 429 会打印说明并以 **退出码 3** 结束，而不是把它当产品失败计数。
- **限流器是进程内的**（刻意的 speed bump，不是持久状态），所以 `systemctl restart clip`
  就会清空。拿它当测试后的清理手段是安全的：真正的滥用backstop 是全局配额与 kill switch，
  它们不依赖这个计数器。

**忘密码没有后门**：没有重置接口、没有找回流程（这是裁决过的）。忘了就只能等 `/sweep` 过期，
或者换一个后缀。不要把“帮用户重置密码”当成一个缺失的功能——它是被明确拒绝的。

## 回滚

```bash
# 当前（至少）有一份旧二进制在这里：/data/backups/clip-binary-57dd925-dirty
#   （sha256 5893fd10df35164a9b121d9cc16ec81ad0f7e181f039bf2cf6d62def599102e0，即修 HEAD 之前）
sudo install -m 0750 -o relay -g relay /data/backups/<上一个二进制> /data/services/clip/clip
sudo systemctl restart clip
# 若本次带了迁移：
docker exec -i -e PGPASSWORD="$PW" postgres pg_restore -U relay -d relay --clean --if-exists < <dump>
```

**升级前先冷备**（上面的部署步骤里已经包含）。备份到底能不能用要验一下 —— 至少让它能解析：

```bash
# 不能 `pg_restore -l -`：CUSTOM 格式需要可寻址的文件，管道会报 cannot open "-"
sudo docker cp /data/backups/<dump> postgres:/tmp/verify.dump
sudo docker exec postgres pg_restore -l /tmp/verify.dump | head    # 有 TOC 列表 = 归档可读
sudo docker exec postgres rm -f /tmp/verify.dump
```

（`pg_restore` 真的恢复到库里的演练**仍未做过**，见 CURRENT_TASK 的运维缺口。）

## 证书相关（DNS 改过之后）

Caddy 用 TLS-ALPN-01 自动签发（`:80` 归 nginx，只有一个 443）。改了 DNS 之后：

```bash
docker restart caddy     # 立即重试，否则要等它的退避计时（实测：重启后 ~40 秒签出）
docker exec caddy ls /data/caddy/certificates/acme-v02.api.letsencrypt.org-directory/
```

**顺序是硬的**：`api.hcyj.xyz` 在下发 `HSTS: includeSubDomains; preload`，
所以子域名必须在**拿到证书之后**才能对外公布 —— 否则访客看到的是不可点过的硬失败。

## 已知约束

- **线上二进制的版本必须能在本仓库里找到。** 2026-10-16 那次事故：线上嵌的是
  `57dd9257a1d6+dirty`，本仓库历史里根本没这个提交（拆分重写过历史，而那版是脏工作区
  构建的），于是「线上跑的到底是哪份代码」只能靠事后反推（实测：嵌入页面逐字节一致、依赖一致、
  方法/状态码矩阵只差当时还没修的 HEAD 与 405）。现已换成 `13ad95d`（干净树）。
  所以：**只从干净工作区构建**，部署后用验证的 g) 步核对。
- **本机关机 = 服务不可用**，hcyj 上不会降级。这是「后端放本机」的直接代价。
- **Caddy 站点级 `log` 会明文记录 `X-Clip-Password`（和从前的 `X-Relay-Clip-Token`）**
  （Caddy 默认只脱敏 `Authorization`/`Cookie`）。要开日志必须同时过滤：

  ```caddy
  log {
      format filter {
          wrap json
          fields {
              request>headers>X-Clip-Password delete
              request>headers>X-Relay-Clip-Token delete
          }
      }
  }
  ```

  Go 侧只记 method/path/status/bytes/requestId，不记任何头。
- **Postgres 备份尚未排期**（目前只有部署前的手工 `pg_dump`）。这是最大的运维缺口。
- hcyj 上 teleport 的转发口是 `:8788`，与 clip 的 `:8790` 不同；Caddy 的
  `/yeciorez/teleport*` 块指 `172.17.0.1:8788`，是另一个服务的东西，不要动。
