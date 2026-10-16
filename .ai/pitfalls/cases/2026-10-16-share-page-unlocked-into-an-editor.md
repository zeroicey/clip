# 只读分享链接解锁后变成了编辑器：路由层只读 ≠ 页面只读

**Date:** 2026-10-16 · **Area:** clip · **Severity:** 中（不是泄露，是页面在撒谎）

## 症状

真浏览器验证（`scripts/verify-clip-browser.cjs`）里，访客浏览器打开 `/s/<token>`、输入正确
分享口令解锁之后：

```text
FAIL  the shared content is read-only in the visitor browser
FAIL  the share view offers no save button
FAIL  the share view offers no delete button
```

即：一个**设计上只读**的分享链接，解锁后给了完整编辑器 —— 可编辑的 textarea、可见的「保存」和「删除」。

同时这条是通过的：

```text
PASS  a share refuses PUT with 405
PASS  a share refuses POST with 405
PASS  a share refuses DELETE with 405
```

也就是**内容没有风险**（服务端根本没有写路由），但页面控件与它自己的保证自相矛盾。
在这个项目里那不是“小瑕疵”：界面在撒谎，用户会以为自己能改。

## 根因

页面（`internal/clip/page.html`）里解锁成功的那条路径写死了：

```js
writable = true;      // ← 无条件
```

对**主入口**是对的：clip 的 `canWrite` 在锁定态也是 `false`（主密码同时是写凭据），所以
“解锁 = 可写”是它的语义。但分享页不是同一回事：分享的 `canWrite` **永远**是 `false`
（服务端 `renderSharePage` 里 `Editable: false, CanWrite: false`），解锁只买到“读”。

于是：

| | 锁定时 `canWrite` | 解锁后应可写？ |
| --- | --- | --- |
| 主入口 `/<后缀>` | `false` | ✅ 是（主密码是写凭据） |
| 分享 `/s/<token>` | `false` | ❌ 否（永远只读） |

两者在 `canWrite` 上**长得一模一样**，所以用 `writable = true` 无法区分。正确的判定是
“这是不是分享”，而不是“是不是刚解锁”。

## 修法

```js
// 解锁只买到读；分享永远是只读。
writable = !isShare;
```

`isShare` 来自服务端下发的 `data-share`，是页面一开始就知道的事实，不依赖任何客户端推断。

## 守卫

- **真浏览器**（会失败、且已经验过）：`scripts/verify-clip-browser.cjs` 里那三条断言
  （textarea `readOnly`、`#save` 隐藏、`#del` 隐藏），外加三条 405 的 HTTP 断言。
  故意做成两半：HTTP 那半证明“写不进去”，DOM 那半证明“页面没在骗人”。
- **闸门内的守卫**：`TestShareWireContract` 断言 `page.html` 里存在 `writable = !isShare;`
  这一行。它很直白（就是钉住那行代码），但它能在**不跑浏览器**的 `scripts/check.sh` 里
  拦住同一个回归 —— 真浏览器脚本不在闸门里，一个只在浏览器里守的 bug 会重新溜进来。
- **反向验证**：把 `writable = !isShare;` 改回 `writable = true;`，上面三条浏览器断言立刻 FAIL。

## 教训

**“路由层只读”是安全保证，“页面只读”是诚实保证，两者都要，而且要用两套不同的检查。**
只测 HTTP 会漏掉后者（405 全绿），只测 DOM 会漏掉前者（隐藏按钮 ≠ 没有端点）。

## 同族：只在快链路上跑过的验证，会在慢链路上以另一个形态失败

部署后拿**同一份**验证脚本打线上，第一次就红了：

```text
FAIL  the share link has the /s/<token> shape
verification crashed: Cannot navigate to invalid URL
```

本地 3 次 3/3 全绿，线上必红。不是产品缺陷，是**脚本与页面的时序竞速**：

1. 页面 `createShare()` 先写“已创建”状态文本，再**异步** `loadShares()`；
2. 脚本看到状态文本就立即读 `#sharelist a` 的 `href`；
3. 线上 RTT ~100ms，那一刻列表还是旧的（甚至空）→ `href === ""`；
4. 接着 `Page.navigate("")` 抛“invalid URL”，掩盖了真正的原因。

本地之所以全绿，只是**本地够快、恰好赢了这场竞速**。

两处一起修：

- **页面**：`createShare()` / `revokeShare()` 改成 `await loadShares()` **再**报成功。
  这不只是为了让测试稳定：状态文本说“已创建”而列表还是旧的，是真实的 UX 缺陷 ——
  人马上点“复制链接”会拿到旧列表。
- **脚本**：等锚点真的出现（`waitFor('!!document.querySelector("#sharelist a")')`），
  并在拿不到 URL 时抛一个能读懂的错误，而不是让 Chrome 报 invalid URL。

**教训**：本机验证跑绿不等于线上会绿 —— 差异常常不是功能而是**时序**。
所以“部署后再用同一份脚本打一次线上”不是走过场，它抓到过本地三次连绿都看不到的缺陷。
另：验证脚本里的**等待条件要等真正的目标**（锚点出现），不要等一个只是“看起来相关”
的中间信号（状态文本）。
