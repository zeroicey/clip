# Gin 的 NoRoute 会把状态码预置成 404

**症状**：`GET /<后缀>` 返回的 HTML 完全正确（编辑器、内容、CSP 都在），但**状态码是 404**。
浏览器里看起来一切正常 —— 因为它照样渲染页面。只有脚本、监控、部署检查会看到 404。

**现象细节**：`w.Body` 有 15000+ 字节的完整页面，`w.Code == 404`。

## 根因

`gin.Engine.ServeHTTP` 在路由树里找不到匹配时，会**先把响应状态写成 404**，然后才调用
`NoRoute` 注册的 handler：

```go
if engine.noRoute != nil {
    c.handlers = engine.allNoRoute
    serveError(c, http.StatusNotFound, default404Body)   // ← 状态在这里就写下了
}
```

`serveError` 里第一步是 `c.writermem.WriteHeader(code)`。之后 handler 再 `c.Writer.Write(...)`
只是写 body —— **状态码已经定型，改不回来了**（除非显式重置）。

我用 NoRoute 来分发公共路径（`/<suffix>`、`/<suffix>/raw[/n]`），因为第一个路径段是用户选的，
用 `/:suffix` 注册会和 `/api/...`、`/robots.txt` 这些静态兄弟节点一起把 Gin 的基数树
推进它处理得最差的那种配置。于是这个坑就踩上了。

## 修复

在 NoRoute handler 里产生**真实内容**的每一条路径上，把状态显式设回去：

```go
c.Status(http.StatusOK)          // 然后才写 body
renderPage(c.Writer, ...)
```

两处：落地页 `renderLanding` 之前、渲染剪贴板页之前。

## 守卫

`TestPublicPagesAreNot404`（`internal/clip/api_test.go`）对 `/` 与一个全新的后缀
断言 **200**，并额外断言 body 不是 Gin 的默认 404 文本。
只断言「body 里有内容」是不够的 —— 那正是让这个 bug 活下来的断言方式。

相关：`.ai/pitfalls/cases/2026-10-12-csp-blocks-the-assets-the-page-links.md` 也是同一类
「页面看起来对、但语义是错的」故障。

同一片区域的第二个坑：`.ai/pitfalls/cases/2026-10-16-head-is-not-get-in-gin-routing.md`
—— 同一个 NoRoute 分发器还会**不区分方法**地接住请求（Gin 不会把 HEAD 路由到 GET 上，
`POST /<后缀>` 也曾拿到 200 页面）。
