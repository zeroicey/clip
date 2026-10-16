# 创建成功了，但浏览器静默丢掉了写权限

**症状**：在页面上写内容、点保存，提示「已创建并保存」。**刷新或换个页面回来，就再也没有写权限了**
（保存按钮消失、textarea 只读）。服务端一切正常：`clips` 表里有那条记录，`owner_hash` 也对。

## 根因

跨语言字段名不一致，而且**两边都不会报错**：

- Go 结构体按项目约定用 snake_case 序列化：`owner_token`、`expires_at`。
- 页面 JS 读的是 camelCase：`r.json.data.ownerToken`。
- `undefined` 被 `store()` 写成字符串 `"undefined"`，长度不到 40，于是后续判断都当"没有 token"。

请求体是 camelCase、响应是 snake_case —— 这是项目里成文的约定（`expiresInHours` 进、
`expires_at` 出）。**这个不对称对人不友好，对编译器更是完全不可见**：TS 不检查 `fetch` 的响应形状，
Go 也不知道客户端读什么字段。于是错误表现为「功能部分可用」：内容存进去了，凭据丢了。

## 修复

页面读服务端真正发出的名字；并且在**契约被破坏时大声失败**：

```js
if (r.status === 201 && r.json && r.json.data && r.json.data.owner_token) { ... }
else if (r.status === 201) { say("创建成功了，但没有拿到写权限（这是 bug）", "err"); }
```

第二条分支是关键：一个 token-less 的 201 是**契约 bug**，不能当成成功。

## 守卫

`TestClientReadsTheFieldNamesTheServerWrites`（`internal/clip/contract_test.go`）同时钉住两半：

1. `json.Marshal(clipData{...})` 的字节里**必须有** `"owner_token"` 等 snake_case 键，
   且**不得有** `"ownerToken"` 这类 camelCase 拼写；
2. `page.html` 里必须**真的读**这些键（`data.owner_token` / `d.owner_token`），
   且不得残留 `data.ownerToken`。

只测一半没有意义：生产方自测只证明它产出了自己以为的东西。这是本项目反复记录的
「跨语言字符串契约必须有消费方视角的测试」那条。

同类教训：`.ai/pitfalls/cases/csp-blocks-the-assets-the-page-links.md`（页面通过检查但功能已死）。
