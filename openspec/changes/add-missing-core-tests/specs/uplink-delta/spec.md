## ADDED Requirements

### Requirement: 字典缓存的淘汰与记账必须有测试护栏

`internal/store` SHALL 有单元测试覆盖：内容寻址存取、body 复制语义、同内容单份、字节预算
淘汰、淘汰后必然 miss、`Stats()` 计数正确。理由：该包是整个压缩收益的中枢，而兄弟包
`spool` 曾因记账漂移出过真 bug（a7f07a 第 3 个 bug）后补了护栏，store 却没有。

#### Scenario: 淘汰后客户端必得 DeltaMiss

- **WHEN** 某条目因预算被淘汰后以它的哈希查询
- **THEN** `Get` 返回 false，服务端据此回 `FrameDeltaMiss`（该行为有测试钉住）

#### Scenario: 不持有调用方缓冲

- **WHEN** Put 之后调用方修改原 body 切片
- **THEN** 缓存内容不变（Put 内部已复制）

#### Scenario: 计数与操作一一对应

- **WHEN** 执行 N 次命中、M 次未命中、K 次淘汰
- **THEN** `Stats()` 的 hits/misses/evictions 精确等于 N/M/K
