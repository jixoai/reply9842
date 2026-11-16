## ADDED Requirements

### Requirement: 本地入口不得返回空的 200

本地 HTTP 入口在「一个响应头都没收到就结束」时 SHALL 给下游写 `502` 与失败原因，
不得让 `net/http` 补出空的 `200`。统计记录的状态码也必须是 502，而不是 200。

覆盖两条路径：
1. 服务端在头产生前发来 `FrameError`（上游不可达 / 超时）；
2. 幂等键连续两次未命中（`delta-miss` → 全量重发 → 再 miss）。

若响应头已经写给下游（`HeadDone`）或已正常完成（`Completed`），不得改写状态码。

#### Scenario: 服务端上游不可达

- **WHEN** 服务端回 `FrameError: server: Post "...": connection refused`
- **THEN** 下游收到 `502` 且 body 含该原因，`client-<date>.jsonl` 里 `status=502`

#### Scenario: 幂等键连续未命中

- **WHEN** 首发 `delta-miss`，全量重发仍 `delta-miss`
- **THEN** 下游收到 `502` 与「连续两次未命中幂等键」说明

#### Scenario: 正常流式响应不受影响

- **WHEN** 服务端先发了响应头再流式推 body
- **THEN** 状态码保持服务端给的值，收尾兜底逻辑不写任何字节
