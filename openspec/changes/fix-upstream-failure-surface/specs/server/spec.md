## ADDED Requirements

### Requirement: 上游失败必须给下游真实的 5xx

服务端在**响应头尚未产生**时判定失败（上游不可达、上游 120 秒未给头），SHALL 向
客户端发送一个完整响应：`FrameResponse` 头（状态 502/504，identity 编码）+ JSON
错误体 + `FrameDone`，而不是只发 `FrameError`。

理由：`FrameError` 是帧级信号，客户端 HTTP handler 此时一个字节都没写，`net/http`
会在 Handler 返回时自动补 `200` + 空 body。调用方看到的是「空返回」，真实原因
（例如上游端口配错）完全丢失。

错误体形状固定为 `{"error":{"type":"upstream_error","message":"<原因>"}}`，
`Content-Type: application/json; charset=utf-8`。

#### Scenario: 上游端口配错

- **WHEN** 上游 base URL 指向一个没人监听的端口
- **THEN** 下游收到 `502` 与含 `connection refused` 的 JSON 错误体，而不是 200 空 body

#### Scenario: 上游两分钟不给响应头

- **WHEN** 上游既没有返回头也没有结束，超过 120 秒
- **THEN** 下游收到 `504` 与说明超时的 JSON 错误体

### Requirement: 上游支持候选列表与拨号期故障转移

`-upstream` SHALL 接受逗号分隔的多个 base URL。请求 URL 按首选候选拼接；建立连接时
按顺序尝试各候选，第一个连通的生效。故障转移只发生在拨号阶段——请求字节一旦发出，
不得重试（POST 不可重放）。

配置非法（缺 scheme/host、空列表）必须启动即失败，不得留到第一个请求才在运行期
变成「上游失败」。

默认值 SHALL 为 `http://127.0.0.1:3425,http://127.0.0.1:3426`（magpie 蓝绿网关双槽）。

#### Scenario: 首选槽 down

- **WHEN** 候选为 `127.0.0.1:3425`（没人监听）与 `127.0.0.1:3426`（活跃）
- **THEN** 请求落到 3426，下游拿到正常响应，而不是 502

#### Scenario: 配置写错

- **WHEN** `-upstream` 只写了 `127.0.0.1:3425`（无 scheme）
- **THEN** 进程启动即报错退出

### Requirement: 启动恢复不得全量扫盘

`spool.New` SHALL 只读 sidecar（`.meta`）恢复幂等条目，**不得**在启动时读取
`.dat` 内容重建 SSE 事件索引。事件索引改为惰性：首次按 event id 续传时
（`EventOffset` / `EventsFrom`）用 `sync.Once` 重建一次。

理由：2GB 幂等区在启动时顺序读 2.1GB 并解压，实测端口 90 秒才起来；而绝大多数
恢复条目永远不会被续传。

#### Scenario: 大幂等区重启

- **WHEN** idem 区有 2GB 条目，进程重启
- **THEN** 监听端口在秒级内可用，恢复条目的 `events` 索引为空

#### Scenario: 重启后按 event id 续传

- **WHEN** 客户端带着重启前的 event id 发起续传
- **THEN** 服务端惰性重建索引并返回与重启前一致的偏移
