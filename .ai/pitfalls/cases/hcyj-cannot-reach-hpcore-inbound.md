# hcyj 连不到 hpcore 的入向端口（所以隧道必须由本机拨出）

**症状**：计划是「后端跑在本机（hpcore），hcyj 只做转发」。于是在 hcyj 上测
`http://100.64.0.1:3000/`（本机上一个已知在跑的服务）、以及后来
`hcyj → 100.64.0.1:{5432,6379,1411,3000,3100,8080}` —— **全部 timeout**。

但同一时刻：

- `hpcore → hcyj`（100.64.0.4:22 / :7000）**通**；
- `tailscale ping 100.64.0.1` 从 hcyj 发出**有 pong**（`via DERP(hpnet) in 70ms`）；
- `tailscale status` 里那一行是 `active; relay "hpnet"` 且末尾写着 **`direct connection not established`**。

## 根因（实测结论，未进一步定位到 ACL 还是中继）

**disco 层通、数据层不通**，而且是**单向的入向不通**。最可能是自建 Headscale 的
方向性 ACL（Headscale 的策略是 `src → dst` 的，`tailscale ping` 不受 ACL 管，TCP 受），
其次是 `hpnet` 这个自建 DERP 的中继回路只在一个方向上真的搬运数据。

无论原因是哪一个，**结论对本项目是确定的**：不能依赖「由 hcyj 主动连本机」。

## 修复

把流量方向反过来：**由 hpcore 主动拨出**。本机早就有这条链路了 —— 给 Minecraft 用的
frpc（`/data/services/frp/bin/frpc`，systemd **user** unit，`Linger=yes`），
`serverAddr = "100.64.0.4"`（hcyj 的 Tailscale IP）、`:7000`、token + TLS。

于是新增两个 proxy（`frpc-relay-clip.toml` / `frpc-relay-teleport.toml`），
hcyj 侧的 frps 在本地开 `:8790` / `:8788`，Caddy 再反代到 `172.17.0.1:<port>`：

```text
浏览器 → hcyj Caddy:443 → 172.17.0.1:8790 → frps → (隧道) → hpcore:8790
```

`/etc/frps.toml` 里**没有 `allowPorts` 限制**，所以加 proxy 不需要动 frps、不会影响 Minecraft。

## 守卫 / 判据

- 任何「hcyj 主动连本机」的方案都要先过这一关：
  **在 hcyj 上 `curl`（或 `/dev/tcp`）本机的目标端口**，而不是 `ping` 或 `tailscale ping`。
  ping 通不代表 TCP 通 —— 这次就是被它骗过一次。
- 验证隧道要用**完整路径**：`hcyj → 172.17.0.1:8790`、以及
  `docker exec caddy wget ...`（Caddy 容器里那一跳才是真实的那一跳）。

## 顺带记下的两个事实

- hcyj 的 `:443` 是 **docker 容器 `caddy`**（不是 systemd 服务），`:80` 归 nginx；
  `docker logs caddy` 的进程日志被重定向到了容器内的 `/var/log/caddy/access.log`。
- 本机 8788 曾被我的预演实例占过；排查「端口被占」时记得 `ss -ltnp` 看进程名。
