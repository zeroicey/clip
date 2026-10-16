# clip — 网络剪贴板

在两台机器之间搬文本或截图，只需要一个网址。

```bash
# 机器 A：打开 https://clip.hcyj.xyz/ops1 ，写下内容，点保存
# 机器 B：打开同一个网址，全选复制
curl https://clip.hcyj.xyz/ops1/raw          # 或者命令行直接拿
```

场景是 Linux 运维时最烦的那件事：两台机器之间有网络，但文件不好传、命令不好执行。
把要搬的东西放在这个网站上，在另一台机器的浏览器里打开、复制、粘贴。
**不需要账号，不需要装任何东西。**

它不为 AI 设计、没有 API key、没有 AI 接口 —— 这是给人用的。AI 报告发布是另一个项目
（teleport），在它自己的仓库里。

## 怎么用

1. 打开 `https://clip.hcyj.xyz/` ，随便打一个后缀（只能用 `a-z 0-9 - _`，最长 32 字符），
   点「打开」。或者在地址栏直接敲 `https://clip.hcyj.xyz/任意后缀`。
2. 写出你要搬过去的内容，点**保存**。**这个后缀就是你的了** —— 浏览器替你记住了写权限，
   以后回到这个网址还能继续改。
3. 在另一台机器上打开同一个网址：全选复制即可。图片会以缩略图显示，点开看原图。
4. 命令行：`curl https://clip.hcyj.xyz/ops1/raw` 拿到纯文本，图片是 `/raw/0`、`/raw/1`…
5. 两边都开着这个页面时会**自动同步**（几秒内），不需要刷新。

**这个网站是公开的**：知道后缀的人都能读。**别往里放密码、密钥或任何机密。**
后缀越短越好猜 —— 短后缀很方便，也正是它不安全的原因。

默认 **24 小时**后自动清空（创建时可以说要多久，说 `0` 就是永不过期）。
内容上限：文本 256 KiB，图片单张 5 MiB、最多 4 张。

## 它是怎么工作的

```text
浏览器 ──https──> clip.hcyj.xyz
                    │
     hcyj (阿里云广州)  Caddy :443   ← 终止 TLS、只做转发
                    │  reverse_proxy 172.17.0.1:8790
                    │
                  frps :7000  ◄── 隧道由本机主动拨出（Tailscale/EasyTier 到 hcyj）
                    │
     hpcore (佛山电信)  frpc ──> 127.0.0.1:8790  clip (systemd)
                    │
                  PostgreSQL (本机容器, database `relay`)
```

**为什么后端在本机、公网入口在 hcyj**：本机在电信 NAT 后面，入向不可达（而且 hcyj →
本机的入向 TCP 是死的），所以由本机主动拨出 frp 隧道到 hcyj，hcyj 负责证书与转发。
代价是本机的可用性决定了服务的可用性。

**为什么剪贴板独占一个子域名**：第一个路径段就是后缀本身
（`https://clip.hcyj.xyz/ops1`）。前缀会把这个卖点吃掉 —— 短到能从一个屏幕念到另一个屏幕，
是这个功能存在的理由。

## 安全模型（读之前先知道）

| 面 | 处理 |
| --- | --- |
| 读 | **永远匿名**。这是设计目标，不是缺陷 |
| 写 | 创建时返回一次 owner token（256 位），只存 `sha256`；改/删必须带 `X-Relay-Clip-Token` |
| 猜后缀 | 能读，**不能改**。恶意改名/换内容被 token 挡住 |
| 后缀即密钥？ | **不是**。后缀是公屏，可枚举。别放机密 |
| 内容渲染 | 一律纯文本（`textContent`），绝不 `innerHTML`；per-response nonce CSP，无 `unsafe-inline` |
| 图片 | 类型从**字节**嗅探，只允许 PNG/JPEG/GIF/WebP（SVG 是可执行文档，明确拒绝） |
| 滥用 | 按 IP 写限流（默认 30/小时）+ 全局配额（2 万条 / 1 GiB，满了**关闭写入**）+ 全局 kill switch |
| 爬虫 | `X-Robots-Tag: noindex` + `robots.txt` 全站 Disallow（被索引的后缀等于被公开） |
| 日志 | 服务端只记 method/path/status/bytes/requestId，**不记任何请求头**（token 走 header） |

## 开发与部署

```bash
scripts/check.sh        # 闸门
scripts/build.sh        # → bin/clip-linux-amd64（静态单文件）
```

部署：`.ai/runbooks/deploy-clip.md`。架构与决策：`.ai/ARCHITECTURE.md`、`.ai/decisions/`。
踩过的坑：`.ai/pitfalls/cases/` —— 那里面每一条都对应代码里的一道守卫。
