# 限流把全网塌缩成一个桶：`{remote}` 是 `host:port`，而 `ClientIP` 会跳过 `host:port`

**Severity:** 🔴（fail-closed，不是绕过，但一个攻击者即可锁死全站申请）
**First hit:** 2026-10-10 · **Hits since:** 0（代码 + 部署双侧已修）

## 症状

上线「agent 自助申请」后做部署验收，从三个**完全不同的**客户端 IP 各打一次公开申请端点：

```
阿里云广州 8.148.233.134    → 429（第一次就 429）
广东电信  121.9.113.26      → 429
腾讯云   124.221.144.97     → 429（第一次就 429）
```

三个不同的 IP 共用同一个 `KEY_APPLY_PER_HOUR=5` 配额。生产库里的审计字段：

```
requester_ip = '172.17.0.5'   ← Caddy 容器的 docker 内网地址，每个请求都一样
```

也就是说：**一个攻击者打 5 次，就能让全世界所有 agent 一小时内无法申请密钥。**
而且 `requester_ip` 这个审计字段完全失去了意义。

## 根因

后端监听 `172.17.0.1:8788`（docker0 网关），Caddy 在 `bridge` 网络的容器里
（`172.17.0.5`）。Caddyfile 写的是：

```caddyfile
header_up X-Forwarded-For {remote}
```

**`{remote}` 在 Caddy 里是 `host:port`**，例如 `121.9.113.26:54321`。

而 `ClientIP` 的设计原则是「不猜」——注释原文：

> Entries that are not single IP literals (bare junk, **host:port**, CIDRs) are skipped
> rather than guessed at, so a non-IP string can never become a bucket.

于是 XFF 里那条唯一的真实地址被**跳过**，找不到可用项，函数按设计**回落到对端**
（"every unparseable case collapses into one shared bucket rather than a fresh one"）。
对端是 Caddy 容器 → 所有客户端同一个桶。

一次性诊断探针的实测（已删除，未留残留）：

```
XFF="121.9.113.26"           → ClientIP="121.9.113.26"   ✅
XFF="121.9.113.26:54321"     → ClientIP="172.17.0.5"     ❌ ← Caddy {remote} 的真实形式
XFF="[2001:db8::1]:54321"    → ClientIP="172.17.0.5"     ❌
XFF="2001:db8::1"            → ClientIP="2001:db8::1"    ✅
```

## 同一个缺陷的第二个方向：可绕过 + 审计投毒（本拓扑下不可达，但代码在两个方向上都是错的）

auth-dev 做变异测试时（把 `parseIP` 还原成修复前版本）暴露了另一半：

```
ClientIP(xff="6.6.6.6, 121.9.113.26:54321") = "6.6.6.6"   ← 修复前，攻击者伪造的最左值
```

机制：右到左遍历时，Caddy 追加的**真实**条目因带端口被当垃圾**跳过**，遍历于是**继续向左
走进了攻击者完全控制的地带**并采信了它。「跳过不可解析项」这个本意是"不猜"的策略，
一旦丢掉了真实条目，就**静默地把攻击者的条目提升成了答案**。后果：

- **链上只有真实条目**（无伪造）→ 被跳过 → 回落对端 → 全网共享一桶（= 观测到的现象）。
- **链上有伪造条目**（代理**追加**而非覆盖）→ 攻击者**自己挑桶**，每伪造一个值一个新桶，
  配额完全失效；`requester_ip` 落库的也是攻击者选的值 —— **审计字段被投毒**。

**准确界定本部署的可达性**：本部署的 Caddyfile 用 `header_up`，那是**覆盖**（不是追加），
所以链上恰好只有一条真实条目，**绕过那条腿在当时的拓扑下不可达** —— 当时线上真实发生的
只有 fail-closed 方向（全网一个桶）。但**代码在两个方向上都是错的**：一旦换个会追加的代理、
或在 Caddy 前面再挂一层，绕过立刻就成立。这也是为什么不能只改 Caddyfile 了事 ——
**部署修复掩盖了代码缺陷，而代码缺陷会在下一次拓扑变更时重新出现。**

## 为什么审计和单测都没抓到

**它们用裸 IP 模拟 Caddy。**

审计报告里限流那项写的是「可信对端 + `"伪造值, 真实IP"`（模拟 Caddy）→ 共享真实 IP 桶 ✅」——
测试通过了，因为它喂进去的是 `121.9.113.26`。而**真实 Caddyfile 产生的是
`121.9.113.26:54321`**。测试模拟一个外部组件时用了**那个组件不会产生的格式**，
于是覆盖了一个不存在的世界。

这与上一轮刚记下的元教训是同一类：**关于外部组件行为的断言，必须回到那个组件的真实输出**，
而不是回到你对它的印象。上一轮是"写**理由**前先看配置文件"，这一轮是"写**测试**前先看真实输出格式"。

## 第二个坑：`sed -i` 会换 inode，把 docker 的单文件 bind mount 弄断

修 Caddyfile 时我先用 `sed -i` 改宿主文件，然后 `docker exec caddy grep` 一看 —— **还是旧内容**。
因为 `sed -i` 的实现是「写临时文件 + rename」，**inode 变了**，而
`/root/hcyj/caddy/Caddyfile → /etc/caddy/Caddyfile` 是**单文件** bind mount：
容器一直盯着**旧 inode**，宿主上的新文件它根本看不见。

而且这个失败是**静默的**：`caddy validate` 读的是容器内的旧文件，报 "Valid configuration"，
`caddy reload` 也"成功"——一切看起来都做了，配置却一个字没变。

**修法**：对 bind mount 的单个文件必须**原地**写入（保 inode）：

```bash
python3 -c "f='$F';s=open(f).read();open(f,'w').write(s.replace(a,b))"   # 截断重写，同 inode
# 不要用 sed -i
```

若已经用 `sed -i` 弄断了，**重启容器**才能让挂载重新指向当前 inode
（`docker restart caddy`，约 1–2 秒中断）。

> 推论：这个坑对**任何** bind mount 的配置文件都成立（nginx.conf、prometheus.yml…），
> 不只是 Caddy。`sed -i` 是安全的默认习惯，但在容器化的宿主上不是。

## Guard（双侧）

**部署侧**（已做）：Caddyfile 改用 `{remote_host}`（裸 IP）。仓库里 `README.md`、
`.ai/runbooks/caddy-routing-on-shared-host.md`、`caddy-handle-path-eats-prefix.md` 的
片段一并改为 `{remote_host}` —— 否则后人照抄又踩。

**代码侧**（让结论不依赖部署巧合）：`ClientIP` 接受 `host:port` 与 `[v6]:port` 形式。
`1.2.3.4:5678` 是**无歧义**的，剥端口不是"猜测"；"不猜"原则继续对真正的垃圾
（`unknown`、`_hidden`、CIDR、空项）生效。测试**直接使用从 Caddy 抄来的真实格式**，
并在注释里写明这些字符串的来源，防止后人"简化"成裸 IP 而重新丢掉覆盖。

**验证**：修复后同一组观测点各自 201，`requester_ip` 分别为
`8.148.233.134` / `121.9.113.26` / `124.221.144.97`。

## 还有一条：把 bug 断言成正确行为的测试

原测试表里有一行 `{"host:port entry skipped", ..., "9.9.9.9"}` —— 它**把 bug 本身
断言成了期望行为**。这类测试比没有测试更糟：它会在有人试图修这个 bug 时**阻止修复**
（改了实现就"破坏测试"）。修 bug 时顺手删掉它，是这次修复的一部分。

## 一句话

> **限流桶键算错时，失败方向通常是"过度合并"而不是"过度分散"** ——
> 合并是静默的、看起来正常的（429 是个"合理"的响应），而分散才会立刻显眼。
> 所以：凡是拿来做安全边界的输入，都要有一条**用真实上游格式**的端到端断言，
> 而不是只用单元测试里手搓的理想格式。
