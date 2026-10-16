# 在共享主机上安全改 Caddy 路由

**Applies to:** hcyj 的 `caddy` docker 容器（占用 443） · **Owner:** lead · **Last verified:** 2026-10-10

## When to use this

给 `api.hcyj.xyz` 增删路径分流（例如本项目的 `/yeciorez/teleport*`），
或修改站点级响应头。

## Preconditions

- `ssh hcyj`（root）
- 知道 443 由 **docker 容器 `caddy`** 持有（**不是** systemd 服务）
- Caddyfile 是**单文件 bind mount**：`/root/hcyj/caddy/Caddyfile` → `/etc/caddy/Caddyfile`

## 现场事实（改之前先确认没变）

| 项 | 值 |
| --- | --- |
| 容器 | `caddy`，容器 IP `172.17.0.5`，docker0 网关 `172.17.0.1` |
| 端口 | `0.0.0.0:443->443`（含 443/udp） |
| 挂载 | `/root/hcyj/caddy/Caddyfile`→`/etc/caddy/Caddyfile`，`data`→`/data/caddy` |
| restart 策略 | 已从 `no` 改为 `unless-stopped`（否则重启机器后整个 `api.hcyj.xyz` 消失） |
| 兜底上游 | `172.17.0.1:3000`（另一个服务，**不要破坏**） |
| 其他必须不动的服务 | `hcyj_minio`(59000/59001)、`hcyj` MySQL(53306)、`friendly_perlman`(:3000)、`nginx:80`(launchadvisor)、`frps`(58173/7000) |

## Procedure

1. **备份**：`cp Caddyfile Caddyfile.pre-teleport-$(date +%Y%m%d-%H%M%S)`
2. **编辑**宿主上的 `/root/hcyj/caddy/Caddyfile`，加入长路径优先的 handler：

   ```caddyfile
   handle /yeciorez/teleport* {
       reverse_proxy 172.17.0.1:8788 {
           header_up Host {host}
           header_up X-Real-IP {remote}
           header_up X-Forwarded-For {remote_host}
           header_up X-Forwarded-Proto {scheme}
       }
   }
   ```

3. **站点级安全头必须排除该前缀**：

   ```caddyfile
   @notTeleport not path /yeciorez/teleport*
   header @notTeleport { ... }
   ```

   后端对分享页发的是更严格的策略（`default-src 'none'`、`X-Frame-Options: DENY`），
   两份同时下发会重复且互相冲突。

4. **校验 —— 这一步最容易骗人**：

   ```bash
   docker cp /root/hcyj/caddy/Caddyfile caddy:/tmp/Caddyfile.check
   docker exec caddy md5sum /tmp/Caddyfile.check          # 必须等于宿主 md5
   docker exec caddy caddy adapt --config /etc/caddy/Caddyfile | grep -c yeciorez/teleport
   ```

   ⚠️ **`docker exec caddy caddy validate --config /tmp/x` 校验的是容器内的文件。**
   宿主 `/tmp` 与容器 `/tmp` 是两个目录，它可能在校验一个陈旧副本并返回
   `Valid configuration`。见 `.ai/pitfalls/cases/docker-exec-validates-container-tmp.md`。

5. **重启并验证**：

   ```bash
   docker restart caddy
   curl -sI https://api.hcyj.xyz/yeciorez/teleport/api/health   # 新路由 200
   curl -sI https://api.hcyj.xyz/                              # 原有服务仍 200
   ```

## Verification

**两个方向都要验**：

- 新路径通（200，且响应确实来自 Go 后端 —— 看 `X-Frame-Options: DENY` 等后端特征头）。
- **原有服务没坏**（`api.hcyj.xyz/` 的兜底上游仍 200）。

Caddy 按路径**最长匹配**选 `handle`，所以新块优先于兜底块，理论上不影响原服务 ——
但"理论上"不算验证。

## Rollback

```bash
cp Caddyfile.pre-teleport-<ts> Caddyfile && docker restart caddy
curl -sI https://api.hcyj.xyz/   # 确认恢复
```

若容器起不来，`docker logs caddy` 看解析错误；Caddy 配置非法时容器会拒绝启动，
443 会整体不可用（影响所有共享服务）—— 所以校验步骤不能跳。

## 已知坑

- `/etc/caddy/Caddyfile.new` 这类"旁边改名"写法**不可见**（单文件 bind mount）。
- Caddyfile 里**没有站点级 `log` 指令** → 没有逐请求访问日志。
  全局 `log` 块只设置默认 logger，不等于开启访问日志（曾据此误判为"日志回归"）。
- **站点级 `log` 会明文记录 `X-Teleport-Claim`。** Caddy 默认只脱敏
  `Authorization` 与 `Cookie`（实测本地 Caddy：`{"headers":{"Authorization":["REDACTED"],
  "Cookie":["REDACTED"],"X-Teleport-Claim":["<明文>"]}}`）。交接链接的一次性凭证走的就是这个头，
  所以**一旦给 `handle /yeciorez/teleport*` 加上 `log`，所有 claim secret 就会落盘**。
  要开日志就必须同时过滤掉它：

  ```caddy
  log {
      format filter {
          wrap json
          fields {
              request>headers>X-Teleport-Claim delete
          }
      }
  }
  ```

  Go 侧的 `LogRequests` 只记 `method/path/status/bytes/requestId`，**不记任何请求头**，
  所以这条约束只关乎反向代理这一层。见
  `.ai/decisions/2026-10-11-one-step-key-handoff.md` 的 D12。
