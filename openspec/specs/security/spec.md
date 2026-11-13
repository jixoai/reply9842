# security Specification

## Purpose

安全边界：TLS 与 SNI 校验、`-insecure` 的适用边界、静默损坏防护、幂等缓存的跨用户隔离、
上游转发时的头处理、以及当前**未**实现的服务端访问控制。

## Requirements

### Requirement: TLS 校验默认开启，insecure 仅限观察期

客户端 SHALL 默认做完整证书校验（含 SNI）；`-insecure`（`InsecureSkipVerify`）仅用于
自签证书观察期或回环调试，文档必须明确「生产别这么干——中间人可以解开全部流量」。
服务端不因 `Insecure` 字段改变自身行为（该字段仅记录语义）。

落地证据：`internal/transport/tunnel.go:163`（QUIC 侧 `InsecureSkipVerify: d.Insecure`）、
`:237`（TCP 侧）、`cmd/server/main.go` 的 `transport.Server{Insecure: ...}`
（注释「自签证书时仍要提供证书；此字段仅记录语义」）、
`DEPLOY.md`（「生产别这么干」原文）。

#### Scenario: 默认绝不跳过校验

- **WHEN** 用户不传 `-insecure`
- **THEN** 证书无效即握手失败，不会静默连通

#### Scenario: insecure 不改变服务端

- **WHEN** 服务端被以自签证书启动
- **THEN** 它仍然要求客户端提供证书链并在客户端侧校验，服务端自身不做放宽

### Requirement: IP 直连必须显式给 SNI

用 IP 连服务端时客户端 SHALL 显式提供 `-server-name`（证书域名），或在「内置默认组合」
下由 `Normalize` 自动补。留空按 IP 推导 SNI 必然 `x509: cannot validate certificate
for <ip> because it doesn't contain any IP SANs`，TCP 与 QUIC 都会失败。

落地证据：`internal/relayclient/runner.go:84`（补值条件）、
`internal/transport/tunnel.go:262`（`serverName()` 推导）、
`internal/relayclient/default_test.go`（两个回归护栏）。

#### Scenario: 缺失 SNI 是可诊断的失败

- **WHEN** 用户把 `-server` 设为某 IP 且不给 `-server-name`
- **THEN** 报错信息明确指出证书 SAN 不含 IP，而不是笼统的「连接失败」

### Requirement: 三道防静默损坏

系统 SHALL 强制三层校验（zstd CRC、dcz 头哈希、ResultHash），没有关闭开关。这是安全
而非性能特性：关掉后「语义正确但内容错乱」的 LLM 请求不会报错，只给出奇怪回答，几天后
才发现。

落地证据：见 `openspec/specs/uplink-delta/spec.md` 的「三道防静默损坏防线不可关闭」
（`internal/codec/dcz.go:87`、`cmd/server/main.go:296`）。

#### Scenario: 没有开关能关掉校验

- **WHEN** 用户寻找关闭 checksum 的参数
- **THEN** 不存在（`-insecure` 只影响证书校验，不影响内容校验）

### Requirement: 幂等缓存必须按调用者隔离

幂等键 SHALL 含 `callerKeyId`（Authorization / X-Api-Key 的短哈希前缀，无凭据为
`anon`），保证「同 key 内幂等、跨 key 隔离」。字典缓存（"store"）当前是**全局共享**的，
这构成 CRIME/BREACH 类攻击面，README 已把它列为已知限制：若将来共享字典，必须按租户
隔离。

落地证据：`internal/idem/key.go:10`（「隐私边界（安全约束，勿改）」）、
`internal/idem/key.go:74`（`CallerKeyID`）、
`README.md` 已知限制第 6 条（跨用户字典隔离与 CRIME/BREACH）。

#### Scenario: 无凭据请求不互相命中

- **WHEN** 两个都不带 Authorization 的调用者发出相同请求
- **THEN** 两者的 callerKeyId 都是 `anon`，会互相命中——这是当前已知缺口，
  见 changes/`add-server-auth-and-tenant-isolation`

#### Scenario: 带凭据的调用者互相隔离

- **WHEN** 两个不同 API key 的调用者发出字节相同的请求
- **THEN** 幂等键不同，绝不互相命中

### Requirement: 上游转发剥离 hop-by-hop 头

服务端转发上游时 SHALL 丢弃 hop-by-hop 头，避免把隧道内部的连接语义泄漏给上游、也避免
上游返回压缩内容与下行压缩叠加。

落地证据：`cmd/server/main.go:752`（`isHopByHop`）、
`cmd/server/main.go:569`（遍历时跳过）、
`cmd/server/main.go:579`（删除 `Accept-Encoding`）。

#### Scenario: 端到端语义不被破坏

- **WHEN** 客户端带了 `Connection: keep-alive`、`Transfer-Encoding: chunked`
- **THEN** 这两个头不出现在上游请求里

### Requirement: 上游默认只允许同机回环

文档 SHALL 明确 `-upstream` 必须是 `127.0.0.1` 或同机内网地址；填公网域名会让流量绕一圈
公网（同时把本应留在机内的明文上下文暴露在链路上更长时间）。

落地证据：`README.md` 快速开始的加粗警告、
`cmd/server/main.go:59`（`-upstream` flag 说明「同机回环最优」）。

#### Scenario: 误配置可被察觉

- **WHEN** `-upstream` 指向公网域名
- **THEN** 功能可用但延迟显著变差，文档已把这条列为配置约束

## 已知边界（安全缺口，逐条见 changes/）

1. **服务端无任何调用方鉴权**：隧道端口一旦对外开放，任何能连上的人都能借它访问上游
   网关（上游的 API key 若在服务端配置里，就是白送的凭证）。见 changes/
   `add-server-auth-and-tenant-isolation`。
2. **字典缓存全局共享**：不同调用者的 body 会进同一个 `store`，两者都可作为对方请求的
   字典。构成跨用户信息侧信道（CRIME/BREACH 类）。见 changes/
   `add-server-auth-and-tenant-isolation`。
3. **`-insecure` 没有「仅回环」护栏**：可以对任意远端地址开 `-insecure`，没有任何警告
   或限制。见 changes/`guard-insecure-mode`。
4. **帧大小上限单侧**：`readFrame` 拒绝超过 256 MB 的帧，但请求体本身没有独立的显式
   上限校验（依赖读内存），大 body 会直接吃满内存。见 changes/
   `bound-request-body-size`。
