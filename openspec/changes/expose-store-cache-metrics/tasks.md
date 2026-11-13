# Tasks: expose-store-cache-metrics

## 1. 落盘

- [ ] 1.1 `internal/stats` 的 `Agg` 增加服务端字段：
      `store_items` / `store_bytes` / `store_max_bytes` / `store_hits` / `store_misses`
      / `store_evictions` / `store_hit_ratio`（都用 `json` 小写下划线，与既有风格一致）
- [ ] 1.2 `cmd/server` 在写 `server-stats.json` 时把 `s.store.Stats()` 灌进去；写入时机
      与现有 `col.writeStatsLocked` 对齐（避免自己另开一条写路径）
- [ ] 1.3 启动日志打印缓存配置（`-cache-mb`）与初始 `store` 状态

## 2. status 子命令

- [ ] 2.1 `reply9842-server -status -stats <path>`（或 `cmd/cli status -server`）读
      `server-stats.json` 打印：请求数 / 错误数 / 上下行 / 缓存命中率与占用
- [ ] 2.2 `-json` 原样输出，供脚本消费
- [ ] 2.3 文件不存在时给人话提示（对齐 `relay-cli status` 的行为，退出码 0）

## 3. 死代码清理

- [ ] 3.1 `internal/idem/key.go` 的 `StreamRequested`：决定接线（用于决定是否走 SSE 优化）
      或删除；二选一，不留占位
- [ ] 3.2 `internal/idem/key.go` 末尾的 `var _ = io.Discard`（`TeeReader` 占位）删除或
      真正实现

## 4. 验证

- [ ] 4.1 `internal/store` 原本无测试文件（`ls internal/store` 只有 `internal/store/store.go`），
      补 `internal/store/store_test.go`：Put/Get/Has/LRU 淘汰/Stats 计数
- [ ] 4.2 端到端：跑一轮请求后 `server-stats.json` 里 `store_hits` > 0 且
      `store_hit_ratio` 合理
- [ ] 4.3 `go test ./...` 全绿

## 验收标准

- 运维能只看 `server-stats.json`（或 status 输出）判断缓存是否健康：命中率、占用、
  淘汰次数三项齐备。
- `internal/store` 有单测覆盖。
- 仓库中不再有「定义了但无调用方」的公开函数（`StreamRequested` 已处理）。
