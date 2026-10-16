# `docker exec caddy ... validate` 校验的是容器内的旧文件

**Severity:** 🔴 high · **First line of defence:** 改 Caddyfile 前必读 · **First hit:** 2026-10-09 · **Hits since:** 1

## Symptom

`docker exec caddy caddy validate --config /tmp/Caddyfile.new` 连续两次返回
`Valid configuration`，但新路由**根本没生效**：`caddy adapt` 显示配置里没有 teleport 的
路由。一次「校验通过」的改动实际上是空操作。

## Root cause

容器内 `/tmp` 与宿主 `/tmp` 是**两个目录**。宿主上写好的 `/tmp/Caddyfile.new`
对容器不可见，于是 `docker exec` 校验的是容器里一个 9 月遗留的**陈旧副本**。

更糟的是这个 Caddyfile 是**单文件 bind mount**
（`/root/hcyj/caddy/Caddyfile` → `/etc/caddy/Caddyfile`），所以
`/etc/caddy/Caddyfile.new` 这种"放到旁边再改名"的写法**也不可见**。

## Fix

改线上 Caddy 配置的可靠流程：

1. 备份 live 文件（`Caddyfile.pre-teleport-<ts>`）。
2. 用 `docker cp` 把新配置送进容器，**比对 md5** 确认送进去的与宿主上的一致。
3. `caddy adapt` 并用 `grep` 断言**新路由确实出现在输出里** ——
   只看 `validate` 的退出码不够。
4. 重启 caddy 容器后，用真实请求验证行为。

```bash
docker cp /root/hcyj/caddy/Caddyfile caddy:/tmp/Caddyfile.check
docker exec caddy md5sum /tmp/Caddyfile.check    # 必须与宿主 md5 相同
docker exec caddy caddy adapt --config /etc/caddy/Caddyfile | grep -c yeciorez/teleport
```

## Guard

- 校验的标准是**行为**（真实请求返回预期），不是 `validate` 的退出码。
- 任何 "Valid configuration" 之后必须再 `adapt | grep` 确认改动在里面。
- kb 里对应条目：`~/kb/pitfalls/2026-10-09-docker-exec-validates-container-tmp-not-host.md`。
