# Gin 不会把 HEAD 路由到 GET 路由上 —— HEAD 探测掉进 NoRoute 被当成后缀

**Severity:** 🔴 high（线上；让部署检查自己报故障）· **First hit:** 2026-10-16 · **Hits since:** 0

## 症状

`.ai/runbooks/deploy-clip.md` 的「验证」第 c) 步，也是交接文档要求新会话跑的三条里的第一条：

```console
$ curl -sI https://clip.hcyj.xyz/api/health | head -3
HTTP/1.1 200 Connection established

HTTP/2 404
```

`GET` 同一路径是 200。整张方法表（线上实测）：

| 路径 | GET | HEAD |
| --- | --- | --- |
| `/api/health` | 200 | **404** |
| `/robots.txt` | 200 | **400**（`text/html`，后缀不合法） |
| `/favicon.ico` | 204 | **400** |
| `/favicon.svg` | 200 | **400** |
| `/`、`/<后缀>` | 200 | 200（本来就对） |

HEAD 探测正是监控、CDN、负载均衡和 `wget --spider` 用的方法 —— 也就是说**服务对唯一
盯着它的那类客户端报告自己坏了**，而 `curl -I` 这种人工检查会直接得出「部署失败」的结论。

## 根因

**Gin 的方法匹配是精确的**：用 `r.GET(path, h)` 注册的路由**不回答 HEAD**。`r.HEAD` 是
独立的方法树，没注册就没有路由，于是请求落到 `r.NoRoute(s.handlePublicPath)`。

`handlePublicPath` 是按「公共命名空间」写的：它把**第一个路径段当成用户选的后缀**，
所以静态路径被送进 `ValidateSuffix`（`robots.txt`、`favicon.ico` 含 `.` → 400），
`/api/...` 走「未知接口路径」分支（→ 404 JSON）。两条分支都没错，错的是它们**收到了
本来不该来的请求**。

第二半是同一个 `NoRoute` 的性质：它对方法**完全不敏感**，所以 `POST /ops1` 也曾拿到
200 + 一整页 HTML —— 写形状的请求被一个文档应答。

## 修复

`internal/clip/api.go`：

1. `router()` 里加一个 `get(...)` 闭包，**每条 GET 路由同时注册 HEAD**：

   ```go
   get := func(relativePath string, handlers ...gin.HandlerFunc) {
       r.GET(relativePath, handlers...)
       r.HEAD(relativePath, handlers...)
   }
   ```

   handler 不需要为 HEAD 写特例：RFC 9110 §9.3.2 要求 HEAD 与 GET **同状态码、同响应头、
   无 body**，而 body 由 net/http 丢弃、响应框架与 GET 一致（小 body 量出 `Content-Length`，
   超过它的 2 KiB 缓冲则两边都是流式）—— 实测 `HEAD /api/health` → `200` + `Content-Length: 195`。

2. `handlePublicPath` 在 `/api` 分支**之后**加方法闸门：公共命名空间只服务 GET/HEAD，
   其他方法一律 405 + `Allow: GET, HEAD`。`/api` 分支刻意保持方法无关（未知路径对任何
   方法都不存在，404 + 封套才是诚实的答案）。

## 守卫

两层，因为一层不够：

- `TestEveryGetRouteAlsoAnswersHead`（`internal/clip/api_test.go`）—— **不需要数据库**，
  所以它跑在默认闸门 `scripts/check.sh` 里。它读 `router.Routes()`，断言每条 GET 路由都有
  HEAD 孪生。`NewServer` 建路由时不碰 store，所以可以传 `nil`。
- `TestHeadMirrorsGetOnEveryRoute` + `TestPublicPathsOnlyAnswerReadMethods`
  （`internal/clip/contract_test.go`）—— 端到端，用**真的 `httptest.NewServer`** 而不是
  `ResponseRecorder`：RFC 要求的「无 body」是 net/http 在传输层实现的，而
  `ResponseRecorder` 会把 body 记下来，正好把要断言的东西藏起来。

两个守卫都验证过**反向**：把 `api.go` 的改动 stash 掉，它们按线上现象失败（`HEAD /robots.txt
= 400`、`POST /ops1 = 200`、`GET /api/health 没有 HEAD 孪生`）。

## 为什么它活到了线上

**那条检查写在文档里，但从来没人执行过。** 部署时的验证记录是「27/27 真浏览器 + 42/42
HTTP 冒烟」，而真浏览器走的是 GET，冒烟脚本也没覆盖 HEAD —— 唯一会踩到它的那条命令只
存在于 runbook 里。教训：**写下来的检查不是检查**，要么它被某次执行覆盖，要么它等于不存在。
