## Why

`internal/store` 实现了完整的缓存观测（`Stats()` 返回 items / bytes / max_bytes / hits /
misses / evictions / evicted_bytes，见 `internal/store/store.go:118`），服务端也持有
`store` 实例（`cmd/server/main.go:140`），但**这份统计没有任何出口**：

```
rg -n 'store.Stats|Hits|Evictions' cmd/server/main.go   → 无命中
rg -n 'Store.Has'                                       → 无命中
```

后果：运维无法回答最基本的问题——「字典缓存命中率多少？」「为什么突然开始 DeltaMiss 频繁？」
「缓存是不是一直满着在淘汰？」。当前只能从客户端侧的 `base_hits` 间接推测。

同样未接线：`internal/idem` 的 `StreamRequested`（`internal/idem/key.go:98`）定义了却无
调用方，`TeeReader` 只是一句 `var _ = io.Discard` 占位。

## What Changes

- 服务端启动时打印缓存配置，并**周期**（或每 N 次请求 / 每次 exit）把 `store.Stats()`
  写进 `server-stats.json` 的一个新段落（不破坏现有字段）。
- `stats.Agg` 增加服务端专用字段：`store_items` `store_bytes` `store_hits`
  `store_misses` `store_evictions` `store_hit_ratio`。
- 新增 `relay-server status`（或 `reply9842-server -status`）：读 `server-stats.json`
  打印人眼友好摘要，与客户端 `relay-cli status` 对称。
- 清理死代码：`StreamRequested` 与 `TeeReader` 占位要么接线要么删除（当前是「写了但没用」，
  容易被误认为已实现的能力）。

## Capabilities

### New Capabilities

无。

### Modified Capabilities

- `server`: 追加「缓存统计必须有观测出口」需求。
- `observability`: 追加「服务端统计与客户端统计对称可读」需求；移除「已知边界」里
  「服务端没有 status 子命令」与「store 指标未接出口」两条。

## Impact

- 代码：`cmd/server/main.go`（采集与落盘）、`internal/stats/stats.go`（Agg 增字段）、
  `cmd/server` 或 `cmd/cli`（status 子命令）、`internal/idem/key.go`（清理死代码）。
- 兼容：`server-stats.json` 只增字段，旧读者不受影响。
- 无新增依赖。
