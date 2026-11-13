# downlink-compression Specification

## Purpose

下行响应体的流式压缩（上行 dcz 的对偶）：上游 chunk 进来就压，绝不攒完整个响应，
保证 SSE 的实时性。生产实测 396 条响应 13.4 MB → 1.39 MB（10.4%，9.6 倍）。

## Requirements

### Requirement: 流式压缩保持 SSE 实时性

响应 SHALL 边收边压成一条连续 zstd 流：同一连接内多次 flush 必须能被后续 flush
接续（不能被当成新流）。禁止先攒完整响应再 `EncodeAll`——那会把流变批，SSE 变成
「憋到结束才吐」。

落地证据：`internal/downlink/downlink.go:62`（`New`）、
`internal/downlink/downlink.go:79`（`Write`）、
`internal/downlink/downlink.go:97`（`Flush`）、
`internal/downlink/downlink_test.go:181`（跨 flush 接续的回归测试）。

#### Scenario: 上游慢速吐事件时下游也慢速收到

- **WHEN** 上游每 38 ms 吐一个 SSE 事件（约 3.7 KB/s）
- **THEN** 客户端在 200 ms 量级内收到已压缩的增量，不会等到整条响应结束

#### Scenario: 多段 flush 是同一 zstd 流

- **WHEN** 一次响应过程中触发多次 flush
- **THEN** 客户端用**单个** decoder 顺序读完即可还原全部明文，不需要在段边界重建 decoder

### Requirement: 字节阈值 + 时间阈值双触发 flush

flush SHALL 由「累计明文字节 ≥ `DefaultFlushBytes`（4 KB）」或「距上次 flush 超过
`DefaultFlushIdle`（200 ms）」任一满足触发。逐事件 flush 必须被禁止：实测会把压缩率
从 11.9% 恶化到 63.9%。

落地证据：`internal/downlink/downlink.go:35`（`DefaultFlushBytes = 4 << 10`）、
`internal/downlink/downlink.go:36`（`DefaultFlushIdle = 200ms`）、
`internal/spool/spool.go:462`（读侧等待）与 `cmd/server/main.go:479`/`:494`
（推侧等待与超时拍板）——三处都以 `downlink.DefaultFlushIdle` 为上限。

#### Scenario: 数据稀疏时靠时间阈值兜住延迟

- **WHEN** 上游速率低于 4 KB / 200 ms
- **THEN** 每 200 ms 至少吐一次已压缩数据，客户端观感是连续流动而非一段一段跳

#### Scenario: 数据密集时靠字节阈值兜住开销

- **WHEN** 上游突发吐入 150 KB
- **THEN** 按 4 KB 粒度多次 flush，不会因为「等时间窗」把内存堆起来

### Requirement: 小响应不压缩

累计明文小于 `MinUseful`（512 字节）时 SHALL 保持 identity，绝不压缩。实测 401 错误
体 172–180 B 压缩后连帧头变成 328–333 B（1.8–1.9 倍膨胀）。

落地证据：`internal/downlink/downlink.go:44`（`MinUseful = 512`）、
`internal/spool/spool.go:314`（`settleCompressionLocked` 以 `MinUseful` 为门槛）、
`internal/spool/spool.go:354`（`ForceDecide` 超时拍板同样过门槛）。

#### Scenario: 小错误体走明文

- **WHEN** 上游返回 180 字节的错误体
- **THEN** 隧道上不出现 zstd 帧，客户端按 identity 直读，线上体积不膨胀

#### Scenario: 跨过门槛后中途改判

- **WHEN** 响应前 300 字节时还没到门槛，随后继续增长超过 512 字节
- **THEN** 之前暂存在 pending 的明文与后续数据一起进入压缩流，客户端只看到一条流

### Requirement: 压缩级别用 SpeedFastest

下行 SHALL 用 zstd `SpeedFastest`，不用 `SpeedDefault`：实测同批样本 10.16% vs
10.00%（只差 0.16 个百分点），而每 encoder 堆占用 0.64 MB vs 1.76 MB、CPU 24 ms vs
38 ms。下行是延迟敏感 + 高并发路径，省内存优先。

落地证据：`internal/downlink/downlink.go:64`
（`zstd.WithEncoderLevel(zstd.SpeedFastest)`）、`:20`（实测对比写在包注释里）。

#### Scenario: 并发连接的内存占用可控

- **WHEN** 数十条响应并发压缩
- **THEN** 每条 encoder 的堆占用约 0.64 MB（而非 1.76 MB），总量可预测

### Requirement: 能力协商保证新旧端互通

下行压缩 SHALL 由客户端在 `RequestHead` / `ResumeReq` 帧**尾部**追加 1 字节
`AcceptDownZstd` 协商；旧客户端不发这一位，服务端退回 identity。

放在帧尾是刻意设计：旧解码器按 `bodyLen` 截 body、只做上界检查，尾部多余字节被忽略，
因此「新客户端 + 旧服务端」也不会解析错乱。

落地证据：`internal/meta/frame.go:113`（`RequestHead.AcceptDownZstd`）、
`internal/meta/frame.go:389`（`ResumeReq.AcceptDownZstd`）、
`internal/meta/frame.go:183` 与 `:210`（编码两端尾部各写 1 字节）、
`internal/relayclient/runner.go:590`（客户端恒置 true）、
`cmd/server/main.go:602`（服务端按此开关决定是否启用压缩）、
`cmd/server/main.go:680`（续传时若条目已压缩但客户端不支持，必须退回）。

#### Scenario: 旧客户端连新服务端

- **WHEN** 客户端不发 `AcceptDownZstd`
- **THEN** 服务端全程以 identity 推响应，客户端不会看到二进制垃圾

#### Scenario: 新客户端连旧服务端

- **WHEN** 服务端不认识帧尾那 1 字节
- **THEN** 它按 `bodyLen` 截取 body、忽略尾部多余字节，请求照常成功

### Requirement: spool 存压缩流但对外保持明文偏移语义

spool 的 `.dat` SHALL 存储压缩后的 wire 字节，但 `Written()` / `ReadFrom()` /
`EventOffset()` 对外**一律保持明文偏移语义**。续传时从中间明文偏移读，先解压丢弃前缀
（实测最大 695 KB 响应 0.46 ms，中位 15 KB 约 0.01 ms，续传是罕见路径可忽略）。

落地证据：`internal/spool/spool.go:143`（`ReadFrom` 明文单位）、
`internal/spool/spool.go:399`（`WireBytes` 才是磁盘真实占用）、
`internal/spool/spool.go:296`（`EnableCompression`）、
`internal/spool/compressed_test.go`（磁盘节省 / 整读 / 跳读 / 任意偏移续传等 11 项）。

#### Scenario: 磁盘预算按压缩后计算

- **WHEN** 幂等区预算为 2048 MB
- **THEN** 记账用的是压缩后字节（`WireBytes`），同样预算能放下更多响应

#### Scenario: 从中间偏移续传内容正确

- **WHEN** 客户端带 `ResumeFrom=<非零明文偏移>` 重连
- **THEN** 服务端解压流并丢弃前缀，从该明文字节处继续推，下游拼接无重复无遗漏

### Requirement: 每条连接的压缩流相互独立

服务端 SHALL 为每条连接从 `ResumeFrom` 处新起一条 zstd 流，跨连接绝不复用 decoder。
复用 decoder 等于把两条无关的流拼接，是唯一的正确性陷阱。

落地证据：`cmd/server/main.go:359`（`streamToClient` 的注释明确「每条连接一个独立的
zstd 流」）、`internal/clientstream/pump.go` 中 `rawSink` 的生命周期约束
（注释：解压器生命周期严格等于一次连接）。

#### Scenario: 连续两次续传互不污染

- **WHEN** 同一请求经历两次断线续传
- **THEN** 客户端每次都用新的 decoder 从该次连接的起点解码，三次收到的内容拼起来
  与上游原文逐字节相同
