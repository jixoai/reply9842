## ADDED Requirements

### Requirement: 字典缓存统计必须有观测出口

服务端 SHALL 把字典缓存（`store`）的统计落盘到 `server-stats.json`：
条目数、占用字节、上限、命中数、未命中数、淘汰数、命中率。统计字段只增不改，
旧读者不受影响。

#### Scenario: 运维能判断缓存健康

- **WHEN** 查看 `server-stats.json`（或服务端 status 输出）
- **THEN** 能看到 `store_hits` / `store_misses` / `store_hit_ratio` /
      `store_evictions`，据此判断缓存是否长期满载淘汰

#### Scenario: 缓存变更有迹可循

- **WHEN** 线上突然出现大量 `DeltaMiss`
- **THEN** `store_evictions` 的上升趋势能证实「缓存被淘汰」这一假设

### Requirement: 无出口的观测代码必须处理

项目 SHALL NOT 保留「已实现但无任何调用方」的公开函数或占位符（当前实例：
`internal/idem` 的 `StreamRequested` 与 `TeeReader` 占位）。要么接线，要么删除。

#### Scenario: 不再有假能力

- **WHEN** 阅读 `internal/idem` 的导出函数列表
- **THEN** 每个函数都有实际调用方（可用 `rg` 验证）
