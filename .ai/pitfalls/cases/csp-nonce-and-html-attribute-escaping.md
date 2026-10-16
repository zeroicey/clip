# CSP nonce 里的 `+` 被 html/template 转义，测试 30% 概率失败

**症状**：`TestPageEscapesTheBody` 间歇性失败（约 5 次里 2 次）：

```
found 0 tags carrying the authorised nonce, want at least 2 (style + script)
```

页面本身在浏览器里**完全正常**（27/27 的真实浏览器验证全绿）。只有逐字节比较的断言会红。

## 根因

nonce 用 `base64.RawStdEncoding` 生成，字母表含 `+` 与 `/`。
而 Go 的 `html/template` 在**属性上下文**里会转义 `+`：

```go
var htmlReplacementTable = []string{
	'"':  "&#34;", '&': "&amp;", '\'': "&#39;",
	'+':  "&#43;",   // ← UTF-7 防御
	'<':  "&lt;",    '>': "&gt;",
}
```

所以响应体里是 `nonce="…&#43;…"`，而 CSP 响应头里是 `nonce-…+…`。
**浏览器解析属性时会把实体解码回 `+`，于是两者其实一致** —— 页面正常工作。
但任何直接比较响应字节的检查（包括我自己的测试）就会不一致。
22 个字符的 base64 里出现 `+` 的概率约 29%，正好对应观察到的抖动频率。

## 修复

换成 **`base64.RawURLEncoding`**（字母表 `A-Za-z0-9-_`）：全部是属性安全的字符，
html/template 不会改写其中任何一个。CSP 规范里 nonce 的 `base64-value` 明确允许 `-` 与 `_`，
所以这不是将就 —— 它本来就是合法选择。

## 守卫

`TestNonceSurvivesHTMLAttributeEscaping`：生成 500 个 nonce，断言**一个**都不含
`+ / = & < > " ' \``，并顺带断言字符多样性（防止有人"修"成退化字母表）。

## 教训

**一个 flaky 测试比一个失败测试更坏**：它教所有人「重跑一下」，于是真问题也一起被忽略。
这次的抖动完全来自测试的比较方式，不是产品行为 —— 但正因为它抖，我才有机会看清
「HTML 属性里看到的字节」与「CSP 头里的字节」是两个不同的东西。

同类：`.ai/pitfalls/cases/csp-blocks-the-assets-the-page-links.md`（也是 CSP 与页面之间的
"看起来一致、实际不是"）。
