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
