## ADDED Requirements

### Requirement: 请求体不得无界读入内存

系统 SHALL 对请求体有显式上限，两侧行为不同但都必须可预测：客户端超限时降级为 identity
转发（保持可用），服务端超限时返回明确错误帧（不转发上游、不占 spool/store、stats 记 413）。

该上限属于应用层策略，与传输层的帧大小上界（`readFrame` 的 256 MB）是两回事：后者是
防解析错乱的兜底，前者是防内存耗尽的策略。

#### Scenario: 服务端超限不产生副作用

- **WHEN** 服务端收到超过 `-max-body-mb` 的请求
- **THEN** 返回明确错误帧，不触发上游请求、不写 store、不新增 spool 条目，stats 记 413

#### Scenario: 客户端超限不失败

- **WHEN** 客户端收到超过上限的请求体
- **THEN** 以 identity 转发并成功返回，不因压缩而 OOM
