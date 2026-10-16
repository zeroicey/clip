# `set -a; . ./env` 会吃掉口令哈希里的 `$`，导致登录永失败

**Severity:** 🔴 high（鉴权静默失效） · **First hit:** 2026-10-09 · **Hits since:** 1

## Symptom

本地 e2e 登录始终 401 / 500，但 `teleport.env` 里的
`ADMIN_PASSWORD_HASH=pbkdf2$600000$<salt>$<key>` 看起来完全正确。

## Root cause

用 `set -a; . ./env` 把 env 文件加载进 shell 时，**shell 会展开值里的 `$`**。
`pbkdf2$600000$abc$def` 被展开成 `pbkdf200000`（`$600000`、`$abc`、`$def` 被当成
未定义变量，展开为空）—— 哈希被静默截断，校验必然失败。

已实测：

```bash
# 坏的：shell 展开
set -a; . ./env; echo "$ADMIN_PASSWORD_HASH"     # → 'pbkdf200000'
# 好的：systemd 直接读文件，不做 shell 展开
systemd-run --property=EnvironmentFile=./env ... # → 'pbkdf2$600000$abc$def'
```

## Fix

- **生产**：用 systemd `EnvironmentFile=/data/services/teleport/teleport.env`
  —— systemd 按字面读值，不经过 shell。
- **本地**：env 文件里的值一律**单引号包裹**；或干脆用 Go 程序自带的
  `.env` 读取（`internal/config`，真实环境变量优先），不要 source 进 shell。

## Guard

- 任何含 `$` 的密钥（PBKDF2 哈希、base64url 串）都当作"shell 会破坏它"处理。
- 验收要**真实登录一次**（拿到 200 + `Set-Cookie`），不要只看 env 文件长得对。
- 这条也是 Gemini/Chroma 之外少数几个"配置看着对、行为完全错"的类别 ——
  属于必须写进知识库的那一类。
