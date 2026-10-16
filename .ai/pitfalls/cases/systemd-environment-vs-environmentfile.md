# systemd 的 Environment= 盖不过 EnvironmentFile=（预演时生成了一个空库）

**症状**：想用一份"预演配置"（换端口、换数据库路径）启动服务，于是：

```bash
systemd-run --unit=tp-rehearse \
  --property=EnvironmentFile=/data/services/teleport/teleport.env \
  --property=Environment=LISTEN_ADDR=127.0.0.1:8799 \
  --property=Environment=DB_PATH=/tmp/tp-rehearse/teleport.db \
  /data/services/teleport/teleport
```

结果日志里是：

```
msg="database ready" path=/data/services/teleport/teleport.db
msg=listening addr=127.0.0.1:8788
```

**两条覆盖都没生效。** 服务用的是生产路径与生产端口，并且在生产路径上
**创建了一个空数据库**（迁移都跑完了，看起来完全健康）。

## 根因

`Environment=` 与 `EnvironmentFile=` 是**按出现顺序**生效的（后面的覆盖前面的），
而 `systemd-run` 生成 unit 时的顺序不由命令行参数的书写顺序决定。实际观察到的是
EnvironmentFile 后生效，于是把 `Environment=` 的值全部盖掉。

**更要紧的是后果**：一个空库出现在生产路径上，`/api/health` 会返回 200，
日志会写 "database ready"。这是最危险的一类静默状态 —— **看起来健康的空系统**。
如果这时切换流量，会得到「服务正常、但所有报告和密钥都不见了」。

## 修复

预演要**用与生产相同的机制**（EnvironmentFile），而不是绕过它：写一个一次性 env 文件
+ 一次性 unit，把值改在里面。这样既验证了 env 文件的解析（包括含 `$` 的口令哈希），
也不会在生产路径上留下任何东西。

预演结束后立刻清理，并**显式确认生产路径上没有残留的库**：

```bash
sudo rm -f /data/services/teleport/teleport.db{,-wal,-shm}
sudo test -e /data/services/teleport/teleport.db && echo STILL THERE || echo removed
```

## 守卫

- 真实切换的顺序被固定为：**停旧服务 → 冷拷库 → 核对行数 → 启动**，
  核对行数这一步就是「不是空库」的直接证据。
- 预演产生的空库当次删除，并有显式的「已删除」确认输出。

## 教训

**任何"预演/影子实例"都可能写到真实路径。** 覆盖配置要么被验证过真的生效
（看日志里它自己报告的路径/端口，而不是相信命令行），要么就不要在生产路径上跑。
