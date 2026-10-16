# 部署 clip（线上已就绪）

**Applies to:** clip 服务（后端在 `hpcore`，公网入口在 `hcyj`）
· **Owner:** lead · **Last verified:** 2026-10-16（证书已签发、公网 27/27 真浏览器 + 大陆观测点全绿）

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

# c) 完整公网路径
curl -sI https://clip.hcyj.xyz/api/health | head -3

# d) 契约形状（改过路由或 API 时必须）
curl -s -o /dev/null -w '%{content_type}\n' https://clip.hcyj.xyz/api/nope   # 必须是 application/json
curl -s -o /dev/null -w '%{content_type}\n' https://clip.hcyj.xyz/a/b/c       # 页面路径是 text/plain
curl -s -o /dev/null -w '%{http_code}\n' https://clip.hcyj.xyz/neversuch      # 200（第一次访问即创建）

# e) 真库测试
TEST_PG_DSN=... scripts/test-pg.sh

# f) 真实浏览器（改过页面/脚本时必须；只依赖 Node 内建，无第三方包）
node scripts/verify-clip-browser.cjs https://clip.hcyj.xyz
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

## 回滚

```bash
sudo install -m 0750 -o relay -g relay <上一个二进制> /data/services/clip/clip
sudo systemctl restart clip
# 若本次带了迁移：
docker exec -i -e PGPASSWORD="$PW" postgres pg_restore -U relay -d relay --clean --if-exists < <dump>
```

## 证书相关（DNS 改过之后）

Caddy 用 TLS-ALPN-01 自动签发（`:80` 归 nginx，只有一个 443）。改了 DNS 之后：

```bash
docker restart caddy     # 立即重试，否则要等它的退避计时（实测：重启后 ~40 秒签出）
docker exec caddy ls /data/caddy/certificates/acme-v02.api.letsencrypt.org-directory/
```

**顺序是硬的**：`api.hcyj.xyz` 在下发 `HSTS: includeSubDomains; preload`，
所以子域名必须在**拿到证书之后**才能对外公布 —— 否则访客看到的是不可点过的硬失败。

## 已知约束

- **本机关机 = 服务不可用**，hcyj 上不会降级。这是「后端放本机」的直接代价。
- **Caddy 站点级 `log` 会明文记录 `X-Relay-Clip-Token`**（Caddy 默认只脱敏
  `Authorization`/`Cookie`）。要开日志必须同时过滤：

  ```caddy
  log {
      format filter {
          wrap json
          fields { request>headers>X-Relay-Clip-Token delete }
      }
  }
  ```

  Go 侧只记 method/path/status/bytes/requestId，不记任何头。
- **Postgres 备份尚未排期**（目前只有部署前的手工 `pg_dump`）。这是最大的运维缺口。
- hcyj 上 teleport 的转发口是 `:8788`，与 clip 的 `:8790` 不同；Caddy 的
  `/yeciorez/teleport*` 块指 `172.17.0.1:8788`，是另一个服务的东西，不要动。
