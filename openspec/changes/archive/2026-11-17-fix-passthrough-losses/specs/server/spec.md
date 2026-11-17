## ADDED Requirements

### Requirement: 转发必须保真到字节

服务端转发上游请求时 SHALL 保留：同名头的**全部值**（每值一行，不得只留首值）、
请求行里的 query string（含百分号编码原样）、以及上游响应的真实状态码。

#### Scenario: 多值头

- **WHEN** 请求携带两个同名头（如两行 `X-Magpie-Kept-Body`）
- **THEN** 上游收到两个值；响应方向同理，下游收到两个值

#### Scenario: 请求带 query

- **WHEN** 下游请求 `/v1/messages?beta=true&alt=sse&x=%2Fslash`
- **THEN** 上游收到的 RequestURI 与之一致；且不同 query 的同一路径不共享幂等键

#### Scenario: 上游 4xx 如实记录

- **WHEN** 上游返回 400
- **THEN** 服务端逐请求日志（server-<date>.jsonl）的 `status` 为 400，不得记成 200
