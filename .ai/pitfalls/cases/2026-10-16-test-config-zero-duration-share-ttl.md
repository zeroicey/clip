# 测试里手搓 Config：零值 duration 不等于默认值（取件码分享生下来就过期）

**Date:** 2026-10-16 · **Area:** clip 测试 · **Severity:** 低（只是测试），但症状极具误导性

## 症状

分享功能写完后，真库测试里只有一条红：

```text
--- FAIL: TestPickupCodeIsReturnedOnceAndThenActsAsTheSecret
    share_test.go:409: the pickup code does not open its share: 404 (not found)
```

创建取件码分享（201，响应里也带 6 位码）之后，**拿着正确取件码**去读 `/s/<token>/raw`
得到 **404**（不是 401）。404 在这条路径上只可能来自一件事：**分享不存在/已过期**。

## 根因

`testStore`（`internal/clip/store_test.go`）是**手写**的 `Config` 字面量，它只列了它当时
关心的字段。`ShareCodeTTL` 是这次新加的，没有加进去 —— 于是它是 **零值 0**。

而取件码分享的过期时间是这样算的：

```go
expiresAt = time.Now().Add(s.cfg.ShareCodeTTL).UnixMilli()   // 加 0 ＝ 就是现在
```

于是分享**生下来就已经过期**，`GetLiveShare` 立刻返回 `ErrNotFound` → 404。

注意生产路径**没有**这个 bug：`config.Load()` 给了 `CLIP_SHARE_CODE_TTL` 默认值 30 分钟，
并且校验 `> 0`。问题只出在“手工构造 Config”的那条路上。

## 为什么症状具有误导性

- 404 在那条路径上**看起来像路由出错**（`/s/<token>/raw` 没匹配上），会让人去查
  `serveShare` 的分支和 token 校验，而不是查配置。
- 创建是 **201**，响应里码也在 —— 每一步看起来都对，只有“以后再来读”是坏的。
- 只有一个测试红：其它分享测试设的都是显式 TTL（`expiresInHours`），或者不用取件码。

## 修法

`testStore` 的 Config 补齐 `ShareCodeTTL: 30 * time.Minute`，并写清楚**为什么不能省**：

```go
// Mirrors config.Load's defaults. Leaving a duration field at its zero value
// here is not harmless: a code share's TTL is used as an absolute deadline, so
// a zero would mint shares that are born expired and every share test would
// fail with a 404 that looks like a routing bug.
ShareCodeTTL: 30 * time.Minute,
```

## 守卫

- `TestPickupCodeIsReturnedOnceAndThenActsAsTheSecret` 就是守卫：它包含“正确取件码必须
  打开分享”这条**正向**断言。没有它，一个恒过期的实现会全部绿着通过（因为所有失败断言都
  在期待失败）。
- 通用的规矩：**每一个“拒绝”断言都要配一条“正确凭据必须通过”的正向断言**，否则
  一个坏掉的实现只会到处返回拒绝，而测试全绿。

## 教训

- 手写的测试配置**不会**自动拿到 `Load()` 的默认值。加字段时，如果它是“绝对时间/时长”，
  零值几乎总是错的那个值 —— 要么补进 testStore，要么让代码对 `<= 0` 显式失败。
- 真库测试的价值就在这里：**这不是逻辑错误，而是“分享真的过期了”，只有对着真库跑才会
  以 404 的形式暴露**；纯 mock 会把 `expires_at = now` 当成一个普通参数收下。
