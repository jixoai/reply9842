## ADDED Requirements

### Requirement: 服务端与客户端统计对称可读

服务端 SHALL 提供与客户端 `relay-cli status` 对称的读取方式：读
`server-stats.json`（路径可用 `-stats` 指定）打印人眼友好摘要，`-json` 原样输出供脚本
消费。文件不存在时打印人话提示且退出码为 0（不是错误）。

#### Scenario: 运维在服务器上直接看状态

- **WHEN** 在服务端执行带 `-status` 的调用
- **THEN** 打印请求数 / 错误数 / 上下行 / 缓存命中率与占用，无需手工 `jq`

#### Scenario: 脚本消费

- **WHEN** 加 `-json`
- **THEN** 输出合法 JSON，含服务端全部聚合字段
