# observability Specification

## Purpose

双向可观测：逐请求 JSONL 是唯一事实来源，汇总快照只是它的一个视图；进程重启必须从
磁盘重建聚合而不是从零开始。GUI 与 CLI/服务端共用同一套 `stats.Agg` 结构。

## Requirements

### Requirement: 磁盘即事实来源，重启重建聚合

统计 SHALL 以逐请求 JSONL（append-only）为事实来源；启动时先流式扫盘重建聚合，再打开
文件准备追加（顺序不可倒，否则同一请求被算两次）。内存里只保留图表要的 30 个桶与分位数
要的最近 500 条——两者都有界，与历史长度无关。

依据（kzf 原话，2026-11-13）：「每次重启统计全丢，说明你用了内存；同时也不要浪费内存，
利用磁盘来解决这个问题」。

落地证据：`internal/stats/stats.go:8`（包注释里的原话与设计结论）、
`internal/stats/stats.go:171`（`Open`：`rebuildLocked()` 在 `openLogLocked()` 之前）、
`internal/stats/stats.go:459`（`rebuildLocked`）、
`internal/stats/stats.go:488`（`scanFileLocked`）、
`internal/stats/rebuild_test.go:32`（`TestRebuildFromDisk`，核心需求即「进程重启后统计
必须还在」）。

#### Scenario: 重启后数据不丢

- **WHEN** 进程处理过若干请求后被重启
- **THEN** 新进程启动即从当日 JSONL 重建出全部聚合，`requests` / `up_raw` 等字段与
  重启前连续

#### Scenario: 不会重复计数

- **WHEN** 重建期间有新请求到达
- **THEN** 重建在锁内完成、之后才打开日志追加，同一请求不被算两次

### Requirement: 分桶图表 30 桶自适应粒度

时间序列 SHALL 固定 30 个桶，桶粒度 = 显示区间 / 30（至少 1 秒），在 2 秒到 48 分钟间
自动适配。按时间桶而不是「按请求」画图——请求到达不均匀，按请求画会让横轴撒谎。

落地证据：`internal/stats/stats.go:28`（`SeriesBuckets = 30` 与「380px 里每根约 9.6px」
的来历）、`internal/stats/stats.go:393`（`BucketSecs`）、
`internal/stats/stats.go:406`（`bucketLocked`）、
`internal/stats/rebuild_test.go:150`（`TestBucketSecsPerWindow`，六档逐行核对）。

#### Scenario: 六档显示区间各自粒度正确

- **WHEN** 显示区间取 1m / 5m / 30m / 1h / 6h / 24h 中任一档
- **THEN** 桶粒度与该档对应（回归测试逐行核对），图表柱宽一致

#### Scenario: 按时间而非按请求

- **WHEN** 空转几分钟后突然一串请求
- **THEN** 图表显示为突发（空桶 + 高柱），而不是被画成匀速

### Requirement: 分位数只用最近 500 条

p50/p90 SHALL 基于最近 500 条样本计算（内存约 60 KB）。分位数本来就是「最近的手感」，
不需要全历史。

落地证据：`internal/stats/stats.go:43`（`recentMax = 500`）、
`internal/stats/stats.go:351`（`recomputeLocked`）、
`internal/stats/stats.go:760`（`q`，分位数计算）。

#### Scenario: 长历史不拖慢

- **WHEN** 已积累数十万条历史请求
- **THEN** 分位数计算仍只取决于最近 500 条，内存与耗时恒定

### Requirement: 保留时长与显示区间各自可调且互相约束

两个时间跨度 SHALL 都可配置且跨重启生效（存 `~/.reply9842/settings.json`），并且必须
满足「显示区间 ≤ 保留时长」；越界或手改出来的非档位值一律吸附到最近的合法档位。
用户偏好绝不写进 `client-stats.json`（那是观测结果的快照，会被反复覆盖写）。

落地证据：`internal/relayclient/settings.go:1`（为什么单独一份文件的理由）、
`internal/relayclient/settings.go:25`（`RetainOptions` 六档）、
`:38`（`WindowOptions` 六档）、
`internal/relayclient/settings.go:77`（`Normalize`：吸附 + 显示区间不超过保留时长）、
`internal/relayclient/settings_test.go`（`RetainOptions`/`WindowOptions` 各 6 档断言 +
六档顺序断言）。

#### Scenario: 手改出 37 分钟

- **WHEN** 用户手工把 `retain_sec` 改成 2220（37 分钟）
- **THEN** 吸附到最近的合法档位（30m），界面不会显示一个不存在的选项

#### Scenario: 显示区间被保留时长压住

- **WHEN** 显示区间为 24h 而保留时长只有 1h
- **THEN** 显示区间被压到 1h，不出现「看 24 小时但只留 1 小时」的自相矛盾组合

#### Scenario: 偏好文件损坏不影响启动

- **WHEN** `settings.json` 是坏 JSON
- **THEN** 返回默认值（保留 24h / 显示 30m），GUI 照常启动

### Requirement: 逐请求记录字段足够回溯

每条记录 SHALL 含：`t` `method` `path` `status` `enc` `base_hit` `up_raw` `up_wire`
`down_raw` `down_wire` `ms_total` `ms_encode` `ms_wire` `ms_decode` `ms_upstream`
`up_bps` `down_bps` `transport` `err`。可直接被 `jq` 消费。

落地证据：`internal/stats/stats.go:46`（`Record` 结构体，字段与 JSON tag 一一对应）、
`internal/stats/stats.go:589`（`appendLocked`）、
`internal/stats/stats.go:619`（`dailyPath`，按日滚动命名）。

#### Scenario: 现场复盘

- **WHEN** 用户执行
  `tail -f ~/.reply9842/client-$(date +%F).jsonl | jq -c '{path,status,enc,up_raw,up_wire,ms_total}'`
- **THEN** 每条请求一行，字段齐全可读

### Requirement: 日志压实防止无限增长

单个日志文件 SHALL 超过 `compactBytes`（32 MB）时压实（删掉窗口外的老记录）。实测单行
约 300 字节、一天 1000+ 请求 ≈ 300 KB，32 MB 相当于 100 天的当日流量，只有异常刷屏才会
触发。

落地证据：`internal/stats/stats.go:39`（`compactBytes = 32 << 20` 与实测依据）、
`internal/stats/stats.go:656`（`compactLocked`）、
`internal/stats/stats.go:677`（`rewriteKeeping`）、
`internal/stats/stats.go:557`（`pruneLocked`）。

#### Scenario: 异常刷屏不撑爆磁盘

- **WHEN** 某日日志超过 32 MB
- **THEN** 压实到窗口外的记录被删除，文件回落到合理体积

### Requirement: TTFB 单独计量

记录 SHALL 单独保存上游首字节时间（`ms_upstream` / `p50_ttfb_ms`），且流式路径必须真的
测到（历史上恒为 0）。0 专指「没测到」（如 401、请求在写响应前就失败）。

落地证据：`internal/stats/stats.go:105`（`P50TTFB/P90TTFB` 字段）、
`internal/stats/stats.go:348`（「TTFB 为 0 表示没测到」的注释）、
`internal/clientstream/pump.go:38`（`Session.TTFB`）、
`internal/relayclient/runner.go`（`rec.MsUpstrm = stream.TTFB.Milliseconds()`）。

#### Scenario: 流式请求也有 TTFB

- **WHEN** 一次 SSE 请求成功
- **THEN** `ms_upstream` 是首个 body 字节抵达下游的耗时，不是 0

## 已知边界

- **服务端没有 `status` 子命令**：`relay-cli status` 只读客户端统计文件；服务端的
  `server-stats.json` 只能手工看。见 changes/`add-server-status-subcommand`。
- **GUI 无法导出内存日志**：`logRing` 收集了 200 行运行日志，但 `snapshot()` 无调用方，
  界面上没有任何入口能看到它。见 changes/`add-gui-log-export`。
