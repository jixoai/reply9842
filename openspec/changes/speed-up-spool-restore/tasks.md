# Tasks: speed-up-spool-restore

> 状态：计划（待开工）。编号即执行顺序，P1 单独就能消除 9.5s 重启停顿。

## P1 启动不扫描

- [ ] 1.1 `spool.New` 去掉 `restoreIdem` 调用，只保留 `MkdirAll`
- [ ] 1.2 新增 `loadIdemByKey(key)`：拼 `idem/<key[:16]>.dat` + `.meta`，水合成
      `Entry{state:Complete, inIdem:true}`（复用现有 sidecar 解析与
      「written=0 时退回文件大小」的兼容逻辑）
- [ ] 1.3 `Acquire` 未命中内存表时先冷查，再决定是否建 tmp 条目
- [ ] 1.4 `resetTmp` 保留（活跃区跨进程必为孤儿），但它只 readdir tmp/，不碰 idem/

## P2 后台补水

- [ ] 2.1 新增 `Manager.startHydrate(ctx)`：listener 就绪后启动，分批扫描 idem/
- [ ] 2.2 批间让路（200 条/批、10ms 间隔，可用 `-spool-hydrate-rate` 调）
- [ ] 2.3 扫描到的条目补进热表；无 sidecar 的孤儿删除；统计字节
- [ ] 2.4 补水结束后跑一次预算收敛（`evictLocked`）；此前按软上限
- [ ] 2.5 `Close` 可取消补水（不阻塞退出）

## P3 持久化索引

- [ ] 3.1 新增 `idem/index.ndjson` 追加写：`put`（k/f/w/z/n/st/t）与 `del`（f）
- [ ] 3.2 写点接入：`promote`、`SetHead`、`Finish`、`Evict`/LRU 淘汰
- [ ] 3.3 后台启动时顺序读索引填热表（替代 2.x 的随机读路径；索引缺失/损坏则退回扫描）
- [ ] 3.4 对账：索引记录 vs 目录列表 vs 文件实际大小；不符即丢弃该条
- [ ] 3.5 压实：追加条数 > 2×live 或文件 > 4MB → 后台 `tmp+rename`
- [ ] 3.6 末尾半行/坏行容错（截断到最后一个换行）

## P4 fd 纪律

- [ ] 4.1 冷条目不常驻 fd：`ensureFile()` 按需打开
- [ ] 4.2 全局句柄上限 64（LRU），关闭最久未用；`Close` 全关
- [ ] 4.3 移除 `restoreIdem` 里「`OpenFile` 失败即静默跳过」的路径

## P5 验证

- [ ] 5.1 合成 50k 条目基准：`New()` < 50ms、`.dat` 读取 0 字节
- [ ] 5.2 冷查单测：命中/半条 sidecar/无文件三种情形
- [ ] 5.3 索引重放 == 全量扫描（结果集与 written/wire/status 一致）
- [ ] 5.4 补水不阻塞请求（并发压测下 p99 不劣化）
- [ ] 5.5 预算收敛与孤儿清理
- [ ] 5.6 `go test -race ./...` + E2E 续传用例
- [ ] 5.7 生产灰度：v0.3.10 → 重启计时 < 1s、fd < 200、续传 200

## P6 文档

- [ ] 6.1 DEPLOY/README 补 `-spool-index` / `-spool-hydrate-rate` 开关与回滚方式
- [ ] 6.2 spec delta：`server` 的「启动恢复」要求改写为「启动 O(1) + 后台补水 + 索引缓存」
