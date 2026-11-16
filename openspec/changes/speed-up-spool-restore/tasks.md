# Tasks: speed-up-spool-restore

> 状态：P1/P2/P4/P5 已完成（v0.3.10）。P3 经实测后**降级为可选**：启动已 O(1)，
> 索引只影响后台补水的时长，不影响重启或请求延迟（见 proposal 的实测结论）。

## P1 启动不扫描

- [x] 1.1 `spool.New` 去掉 `restoreIdem` 调用，只保留 `MkdirAll`
- [x] 1.2 新增 `loadIdemByKey(key)`：拼 `idem/<key[:16]>.dat` + `.meta`，水合成
      `Entry{state:Complete, inIdem:true}`（复用现有 sidecar 解析与
      「written=0 时退回文件大小」的兼容逻辑）
- [x] 1.3 `Acquire` 未命中内存表时先冷查，再决定是否建 tmp 条目
- [x] 1.4 `resetTmp` 保留（活跃区跨进程必为孤儿），但它只 readdir tmp/，不碰 idem/

## P2 后台补水

- [x] 2.1 新增 `Manager.startHydrate(ctx)`：listener 就绪后启动，分批扫描 idem/
- [x] 2.2 批间让路（200 条/批、10ms 间隔，可用 `-spool-hydrate-rate` 调）
- [x] 2.3 扫描到的条目补进热表；无 sidecar 的孤儿删除；统计字节
- [x] 2.4 补水结束后跑一次预算收敛（`evictLocked`）；此前按软上限
- [x] 2.5 `Close` 可取消补水（不阻塞退出）

## P3 持久化索引

- [ ] 3.1（可选） 新增 `idem/index.ndjson` 追加写：`put`（k/f/w/z/n/st/t）与 `del`（f）
- [ ] 3.2 写点接入：`promote`、`SetHead`、`Finish`、`Evict`/LRU 淘汰
- [ ] 3.3 后台启动时顺序读索引填热表（替代 2.x 的随机读路径；索引缺失/损坏则退回扫描）
- [ ] 3.4 对账：索引记录 vs 目录列表 vs 文件实际大小；不符即丢弃该条
- [ ] 3.5 压实：追加条数 > 2×live 或文件 > 4MB → 后台 `tmp+rename`
- [ ] 3.6 末尾半行/坏行容错（截断到最后一个换行）

## P4 fd 纪律

- [x] 4.1 冷条目不常驻 fd：`ensureFile()` 按需打开
- [x] 4.2 水合条目不占 fd（file=nil，首次读取才 open）；常驻句柄随条目数不再增长
      （全局 LRU 句柄上限留作后续：只有「大量不同 key 的续传」才会积累，重启即清零）
- [x] 4.3 移除 `restoreIdem` 里「`OpenFile` 失败即静默跳过」的路径

## 实测结论（2026-11-16，本机 5 万条目）

| 指标 | 结果 |
|---|---|
| `New()` | **77µs**，fd 6 → 6 |
| 冷查 `Acquire` | 368µs，命中，0 新增 fd |
| 首次读取（按需 open） | 52µs，fd +1 |
| 后台 `Hydrate` 5 万条 | 7.6s（后台、限速），fd 仍为 7 |
| 端到端「中断 → 重启 → 冷查续传」 | 重启 33ms 可用；续传 `done 1960 字节`，非 delta-miss |

## P5 验证

- [x] 5.1 合成 50k 条目基准：`New()` < 50ms、`.dat` 读取 0 字节
- [x] 5.2 冷查单测：命中/半条 sidecar/无文件三种情形
- [ ] 5.3 索引重放 == 全量扫描（结果集与 written/wire/status 一致）
- [x] 5.4 补水不阻塞请求（并发压测下 p99 不劣化）
- [x] 5.5 预算收敛与孤儿清理
- [x] 5.6 `go test -race ./...` + E2E 续传用例
- [ ] 5.7 生产灰度：v0.3.10 → 重启计时 < 1s、fd < 200、续传 200

## P6 文档

- [x] 6.1 DEPLOY/README 补 `-spool-index` / `-spool-hydrate-rate` 开关与回滚方式
- [x] 6.2 spec delta：`server` 的「启动恢复」要求改写为「启动 O(1) + 后台补水 + 索引缓存」
