# 给页面加自托管字体，却忘了 CSP 也得允许 —— 页面静默回退系统字体

**Severity:** 🟠 medium（线上已存在，静默降级） · **First hit:** 2026-10-12 · **Hits since:** 1

## 症状

上线 `723c76e` 后，用**真浏览器打生产**发现 `/ai` 页：

```
Refused to load the stylesheet 'https://api.hcyj.xyz/yeciorez/teleport/fonts/fonts.css'
because it violates the following Content Security Policy directive:
"default-src 'none'; style-src 'unsafe-inline'; ..."
```

页面 HTTP 200、内容完整可读、没有任何可见异常 —— **只是悄悄用回了系统字体**。

同一次上线的分享页 `/s/{token}` 是好的，因为它本来就带 `style-src 'self'` 和
`font-src 'self'`。**同一个改动，两个页面，一个对一个错。**

## 根因

`/ai` 的 CSP 是按"这个页面只有一个内联 `<style>`、不加载任何外部资源"设计的：

```go
// 上线时（错误）
const CSP = "default-src 'none'; " +
    "style-src 'unsafe-inline'; " +   // ← 没有 'self'
    "img-src 'self' data:; " + ...
```

而本轮给它加了自托管字体，需要**两条**指令同时放行：

| 缺哪条 | 后果 |
| --- | --- |
| `style-src` 缺 `'self'` | `<link rel=stylesheet>` 直接被拒，页面连 `@font-face` 都看不到 |
| `font-src` 缺 `'self'` | 即使样式表加载成功，`@font-face` 的字体抓取也被拦 —— 它**回退到 `default-src 'none'`** |

第二条更隐蔽：补了 `style-src 'self'` 之后，样式表能加载、`@font-face` 也解析了，
但字体请求仍然被拦。**只修第一条会得到一个"看起来修好了"的假象。**

> 一般化的教训：**给页面引入任何外部资源时，被改动的文件不止是模板，还有它被送达时携带的策略。**
> 这两处在代码里相隔两个包（`aidoc/html.go` 的模板 + 同文件的 CSP 常量），
> 而"页面引用了什么"和"策略允许什么"从来没有被任何东西对着看过。

## 为什么没被拦住 —— 三道闸门全部盲区

| 闸门 | 为什么测不到 |
| --- | --- |
| `go test ./...` | 只断言 `CSP` 字符串本身（`default-src 'none'` 在、`script-src` 不在），**从不问页面引用了什么** |
| 本地真 Chromium 7 组页面 | **本地 stub 服务器不发 CSP 头** —— 没有策略，自然没有拦截 |
| 真实二进制 `curl` 核对响应头 | 只核对了字体/图标链接是否**存在**，没有核对 CSP 是否**允许**它们 |

三道闸门都"通过"了，而线上是坏的。**唯一抓住它的是一条：真浏览器 + 真 CSP 头。**

这是本轮第二次踩到同一个形状的坑（第一次见
`.ai/pitfalls/cases/2026-10-12-relative-asset-url-under-spa-fallback.md`）：
**"资源被引用了"和"资源能加载"是两件事，而测试往往只覆盖前者。**

## Fix

**已修（`dbca734`）**：

```go
const CSP = "default-src 'none'; " +
    "style-src 'self' 'unsafe-inline'; " +   // 'self' 允许外部样式表
    "font-src 'self'; " +                    // 允许 @font-face 抓字体
    "img-src 'self' data:; " + ...
```

`script-src` 依然**完全没有** —— 这个页面按设计不含脚本，
不放宽它，注入 `<script>` 仍会被 `default-src 'none'` 拦住。

## Guard

1. **断言"策略"与"页面实际引用的资源"互校**，而不是分别断言两侧。
   `aidoc/html_test.go` 的 `TestCSPAllowsExactlyWhatThePageLoads`：
   先确认页面确实声明了 stylesheet / icon，再断言 CSP 里有
   `style-src 'self'` / `font-src 'self'` / `img-src 'self'`，
   最后断言 `script-src` **不存在**（放宽不许蔓延到脚本）。
   **已反证验过**：还原成上线那版 CSP，测试立刻红并同时报出两条缺失。

2. **分享页的同类断言一并补齐**（`views/sharepage_test.go`）。
   它本来是对的，但"对的"如果没人守着，下一次就会跟着漂移。

3. **验证 CSP 必须用真浏览器打真环境。** 本地 stub 必须发 CSP 头，
   否则整个本地验证套件对这类缺陷是盲的 —— 这是本轮最有价值的教训。

4. **给页面加外部资源时，清单里要有三样**：资源本身、它的 URL（前缀正确）、
   以及**送达该页面时的策略是否放行**。

## 一句话

> 页面引用了什么，和页面**被允许**加载什么，是两份独立的清单 ——
> 只改一份，另一份不会报错，只会让你的字体悄悄变回系统字体。
