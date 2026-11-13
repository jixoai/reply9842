# resume-idempotency Specification

## Purpose

断线续传与幂等：客户端断开后上游继续收，重连时从断点续传或直接复用已完成的响应。
核心是 pump（上游→文件）与 client writer（文件→客户端）解耦，以及磁盘双区域 spool。

## Requirements

### Requirement: 上游请求脱离客户端生命周期

服务端 SHALL 用 `context.WithTimeout(context.WithoutCancel(context.Background()), upstreamTimeout)`
派生上游请求的 ctx——客户端断开不得取消上游请求。这是「请求能续上」的全部前提。

落地证据：`cmd/server/main.go:557`（`startPump`）、
`cmd/server/main.go:558`（`WithoutCancel` 派生点）、
`internal/idem/key.go:8`（包注释把该约束写成「正交意图」第 2 条）。

#### Scenario: 客户端断开后上游仍跑完

- **WHEN** 客户端在响应中途断开连接
- **THEN** 上游请求继续接收直到完成，响应落盘到幂等区，客户端重连即可取回

#### Scenario: 上游超时独立于客户端

- **WHEN** 客户端从发起后立即断开
- **THEN** 上游请求仍受 `-upstream-timeout`（默认 10m）约束，不会永久悬挂

### Requirement: 磁盘双区域 spool 与字节预算淘汰

spool SHALL 分两个区域：`tmp/`（活跃，客户端在线）与 `idem/`（断线继续收 + 已完成可复用），
各自独立字节预算（`-tmp-mb` 默认 512、`-idem-mb` 默认 2048），按字节预算淘汰最久未用。
淘汰以 LRU 为准，不按时间 TTL。

落地证据：`internal/spool/spool.go:1014`（`Acquire`）、
`internal/spool/spool.go:1054`（`Detach`）、
`internal/spool/spool.go:1223`（`evictLocked`）、
`internal/spool/spool.go:1062`（`promote`，tmp→idem 原子 rename）、
`cmd/server/main.go:125`（`spool.New` 传入两个预算）。

#### Scenario: 活跃条目永不被淘汰

- **WHEN** tmp 区接近预算上限，其中有正在写入的活跃条目
- **THEN** 淘汰只挑非活跃条目（回归测试 `TestActiveNeverEvicted`）

#### Scenario: 预算记账不漂移

- **WHEN** 大量 Append / Detach / 淘汰交错发生
- **THEN** 记账字节与实际磁盘占用一致，不出现负数或永久漂移
  （回归测试 `TestBudgetAccountingNoDrift`）

### Requirement: 完成即迁入幂等区，不只靠 Detach

上游结束时（`Finish`）SHALL 立刻触发 tmp→idem 迁移，不得只在`Detach`（客户端断开）
时迁移。原实现在只 Detach 迁移，导致客户端全程在线的正常请求永远滞留在 `tmp/`，
重启后丢失——这是 2026-11-13 线上实证的 bug。

落地证据：`internal/spool/spool.go:714`（`Finish`，注释明确「这是修复『完成响应滞留
tmp/』的核心」）、`internal/spool/spool.go:232` 附近的
`TestCompletePromotesToIdem`、`internal/spool/spool.go:328` 附近的
`TestDetachAfterCompleteKeepsComplete`。

#### Scenario: 正常请求完成后可被复用

- **WHEN** 一次请求正常完成（客户端全程在线，从未断开）
- **THEN** 其响应已在幂等区，同 IdemKey 的下一次请求直接命中（实测 257 µs 级）

#### Scenario: 已完成条目不被断开降级

- **WHEN** 条目已 Complete 之后才发生 Detach
- **THEN** 状态保持 Complete，不回退

### Requirement: 幂等键由请求指纹派生且含调用者身份

幂等键 SHALL 为 BLAKE3/SHA-256 派生值，材料为
`callerKeyId || method || path || sortedHeaderSubset || body`（长度前缀编码）。
`callerKeyId` 必须参与——否则不同用户发相同内容会互相命中缓存，造成跨用户响应泄露。
只取影响模型输出的请求头（content-type、anthropic-version、anthropic-beta、
openai-beta、accept），忽略 authorization（已由 callerKeyId 表达）、user-agent 与 trace 类噪音头。

落地证据：`internal/idem/key.go:31`（`Key`）、`internal/idem/key.go:74`
（`CallerKeyID`，取 Authorization 或 X-Api-Key 的短哈希前缀，无凭据时返回 `anon`）、
`internal/relayclient/runner.go:577`（客户端计算键）、
`cmd/server/main.go:278` 附近（服务端使用 reqHead 里的键）。

#### Scenario: 同内容同键

- **WHEN** 同一调用者以完全相同的方法、路径、头子集与 body 重发请求
- **THEN** 得到同一个 IdemKey，命中已完成的响应

#### Scenario: 跨调用者隔离

- **WHEN** 两个不同 API key 的调用者发出字节完全相同的请求
- **THEN** 幂等键不同，绝不互相命中对方缓存

### Requirement: 续传游标以「已写给下游的字节」为准

客户端 SHALL 记录 `session.Bytes`（已成功写给下游 body 的字节）与
`session.EndEventID`（最后 SSE event id），重连时作为 `ResumeFrom` / `EventID`。
不得以「已从隧道收到的字节」为准——缓冲区里没刷出去的部分断线后必须重放。

落地证据：`internal/clientstream/pump.go:36`（`Session.Bytes` 与注释
「已成功写给下游的 body 字节（重连时作为 ResumeFrom）」）、
`internal/clientstream/pump.go:337`（`BuildResumeFrame`）、
`internal/relayclient/runner.go:677`（续传重试循环）。

#### Scenario: 未刷出的缓冲被重放

- **WHEN** 隧道已收到 10 KB 但只有 8 KB 写给了下游，随后连接中断
- **THEN** 续传从 8 KB 处开始，下游拼接出的内容无重复无遗漏

### Requirement: 终止帧区分「上游完成」与「连接断裂」

服务端推完响应后 SHALL 发送 `FrameDone`。客户端只能靠连接关闭判断结束时，无法区分
「上游正常完成」与「网络抖动」——实测正常 200 响应会被记成 `recv: EOF` 错误，并触发
3 轮无意义续传（12.5 s 里大部分时间耗在这）。

落地证据：`internal/meta/frame.go:39`（`FrameDone` 的完整理由注释）、
`internal/meta/frame.go:46`（`FrameDone FrameType = 9`）、
`cmd/server/main.go:359` 附近（正常推完后发 FrameDone）、
`internal/clientstream/pump_test.go:120`（`TestDrainFrameDoneCompletesCleanly`）、
`internal/clientstream/pump_test.go:154`（`TestDrainConnCloseWithoutDoneIsResumable`）。

#### Scenario: 正常完成不算错误

- **WHEN** 服务端推完并发出 FrameDone
- **THEN** 客户端 `stream.Completed = true`、`session.Err = nil`，不触发续传

#### Scenario: 无终止帧仍可续传

- **WHEN** 连接在对端无 FrameDone 的情况下关闭
- **THEN** 客户端判定为可续传中断，用已消费偏移发起 Resume

### Requirement: 中止条件收敛为「可恢复才重试」

续传重试 SHALL 只发生在「可恢复的中断」：已完成、服务端判定 Fatal、下游已断开、
DeltaMiss 四种情况都不续传。重试次数由 `-max-resume` 控制，退避为递增的
`(n+1) × 300 ms`。

落地证据：`internal/relayclient/runner.go:677`（循环条件
`att < MaxResume && !stream.Completed && !stream.Fatal && stream.Err != nil &&
!stream.ResumeMiss && !isDownstreamGone(stream.Err)`）、
`internal/clientstream/pump.go:44`（`Fatal` 字段的语义注释）、
`internal/clientstream/pump_test.go:173`（`TestDrainFrameErrorIsFatal`）。

#### Scenario: 下游已走不再续传

- **WHEN** 下游 HTTP 客户端已断开
- **THEN** 不再重连隧道（数据没人接收），条目保留在服务端幂等区等待真正的重连

#### Scenario: 4xx 业务错误不重试

- **WHEN** 上游返回业务性失败（`Fatal` 置位）
- **THEN** 立即结束，不消耗重试配额——重放只会换来同样结果

### Requirement: 失败响应不得污染幂等缓存

上游不可达或非 2xx 的条目 SHALL 标记 `uncacheable` 并在推送完成后立即 Evict，不进入
复用池。原缺陷：香港 magpie-b 因 OOM 被杀后产生 `written=0` 的空响应被标记为已完成，
同 key 之后**永久**命中空响应。

落地证据：`internal/spool/spool.go:770`（`MarkUncacheable`）、
`internal/spool/spool.go:419` 附近的 `TestUncacheableEviction`、
`cmd/server/main.go:557` 附近 `startPump` 内的置位逻辑。

#### Scenario: 上游失败后同 key 能拿到正确结果

- **WHEN** 某 key 的首次请求因上游不可达失败
- **THEN** 该条目被立即驱逐，上游恢复后同 key 请求会重新发上游

## 非目标

- 幂等复用不减少「上游生成时间」，只省掉重复排队与重传
- `tmp/` 重启后不恢复是刻意设计：活跃请求绑在进程内 goroutine，进程死了 pump 也没了
