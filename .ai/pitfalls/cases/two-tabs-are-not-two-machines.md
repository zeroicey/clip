# 两个标签页不是两台机器（验证脚本的前提错误）

**症状**：真实浏览器验证里，写者页面创建成功、token 也存进了 localStorage，
但紧接着用 Node 脚本发起的 API 更新返回 **409 suffix_taken**，读到 `owner_token: null`。

## 根因

验证脚本的设计错了，不是产品错了：

1. 用同一个 Chrome 实例开了两个标签页，一个当"机器 A"，一个当"机器 B"。
2. 为了让"机器 B"看起来没有凭据，调了
   `Storage.clearDataForOrigin({ origin: BASE, storageTypes: 'local_storage' })`。
3. **同一个 origin 的 localStorage 是两个标签页共享的** —— 这一清，把写者的 token 也清了。
4. 脚本随后再 `localStorage.getItem(...)` 拿 token 去发 API 请求，拿到 `null`，
   于是不带凭据的请求走到了「创建」分支 → 409（这是**正确**的服务端行为）。

顺带说明：这也解释了为什么该断言"应该"失败却先通过了 —— 写者标签页的 JS 把 token 存在闭包变量里，
所以它的**界面**继续正常工作，只有从 localStorage 重新读取的那条路径是空的。

## 修复

在清任何存储**之前**，把 token 提到 Node 变量里：

```js
const OWNER_TOKEN = await writer.eval(`localStorage.getItem('relay.clip.${SFX}')`);
// …之后所有 API 调用都用 OWNER_TOKEN
```

并在脚本注释里写明这一点。要真正模拟两台机器，应该用 CDP 的
`Target.createBrowserContext` 开一个隔离 profile，而不是清共享存储。

## 守卫

- 验证脚本里 `OWNER_TOKEN` 的取值点带着注释（改回去就会再踩一次）。
- 断言 409 的**响应体**也被打印出来，所以「为什么被拒」在日志里是可见的。

## 教训

**"看起来像两台机器"和"是两台机器"不是一回事。** 验证脚本本身也是一个被测对象：
它可以在产品完全正确的情况下变红，也可以在产品已经坏掉的情况下变绿。
写验证代码时，要问「我到底在模仿哪个真实条件」。

## 后续（2026-10-16）：这个坑被设计消掉了，不是被绕过

当时的根因是**凭据存在 localStorage**（同一 origin 的所有标签页共享）。现在凭据是
**密码，存在 sessionStorage**（按标签页），而且无密码的 clip 根本不需要凭据。
所以「两个标签页」在密码这件事上已经天然等于两台机器。
教训仍然成立：**只要凭据按 origin 存、就永远会在多标签页/多用户场景下被共享。**
