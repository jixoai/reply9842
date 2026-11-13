# transport Specification

## Purpose

隧道传输层：QUIC 与 TCP+TLS 在同一端口双栈监听，客户端支持应用层 auto 回退，
并支持绑定出站源地址以绕过代理 TUN 劫持。帧格式自定界，两种传输共用同一套编码。

## Requirements

### Requirement: QUIC 与 TCP+TLS 同端口双栈

服务端 SHALL 在同一个 `-listen` 地址上同时启动 QUIC（UDP）与 TCP+TLS 监听，
把两类连接统一成 `Accept{Kind, Conn}` 送进同一条 accept 通道。ALPN 一律为
`reply9842/1`（新旧客户端不兼容是改名后的刻意结果）。

落地证据：`internal/transport/tunnel.go:307`（`Server.Listen`，同时起 QUIC
listener 与 `tls.Listen`）、`internal/transport/tunnel.go:39`（`ALPN`）、
`internal/transport/tunnel_test.go:25` 与 `:85`（`TestTCPRoundTrip` / `TestQUICRoundTrip`）。

#### Scenario: 只放通 TCP 也能工作

- **WHEN** 服务端 UDP 端口被防火墙拦掉
- **THEN** TCP/TLS 路径照常服务，客户端 auto 模式降级成功

#### Scenario: 一条 QUIC 连接承载多请求

- **WHEN** 客户端在一条 QUIC 连接上先后打开多个 stream
- **THEN** 每个 stream 独立处理一次请求，某条连接退出不影响其它连接

### Requirement: 应用层 auto 回退

客户端 SHALL 在 `-transport auto` 下先试 QUIC（探测超时 `QuicProbe`，默认 1 s），
失败降级 TCP+TLS，并**记住成功的那一种**用于后续连接优先级。回退必须实现在应用层：
HTTP/3 的自动回退是 HTTP 协议族能力（Alt-Svc），不是 QUIC 传输层自带。

落地证据：`internal/transport/tunnel.go:101`（`Dial`）、
`internal/transport/tunnel.go:159`（`dialQUIC`）、
`internal/transport/tunnel.go:233`（`dialTCP`）、
`internal/transport/tunnel.go:47`（`QuicProbe` 字段）、
`internal/transport/tunnel.go:61`（粘性记忆注释）。

#### Scenario: QUIC 握手失败后 TCP 接管

- **WHEN** 链路上 QUIC 从未握手成功（该情况在生产链路上实测过）
- **THEN** 同一次请求在探测超时后自动改走 TCP+TLS 并成功，用户不感知

#### Scenario: 记住成功模式

- **WHEN** 某次连接以 TCP 成功
- **THEN** 后续连接优先走 TCP，不再每次都等一个 QUIC 探测超时

### Requirement: 帧必须自定界

隧道帧 SHALL 为「4 字节大端长度 + payload」，读取必须用 `io.ReadFull`。禁止手写
「循环 Read，遇 err 立即返回」——`io.Reader` 允许同时返回数据与 `io.EOF`，QUIC 的
stream 在最后一帧正是 `(n>0, io.EOF)`，手写循环会丢掉这一帧（实测为「服务端 write
成功、客户端读到 EOF」的根因）。

落地证据：`internal/transport/tunnel.go:404-412`（`readFrame` 及 io.ReadFull 的理由）、
`internal/transport/tunnel_test.go:178`（`TestReadFrameEOFWithData`）、
`internal/transport/tunnel_test.go:211`（`TestWriteAllPartialWrites`）。

#### Scenario: 末帧带 EOF 也不丢

- **WHEN** 对端写完最后一帧立即 FIN
- **THEN** 该帧被完整读出，不会因为同时返回的 `io.EOF` 被丢弃

#### Scenario: 半包写入被补齐

- **WHEN** 底层连接一次只写入了部分字节
- **THEN** `writeAll` 继续写直到写完，帧边界不被打断

### Requirement: 支持绑定出站源地址

客户端 SHALL 支持 `-local-addr`（CLI / GUI 设置页「源地址」）绑定出站源地址：
TCP 走 `net.Dialer.LocalAddr`，QUIC 走 `net.ListenUDP` + `quic.Dial`（`DialAddr`
不允许指定源地址）。只给 IP 时自动补 `:0`；无效地址必须**明确报错**，不得静默回退
（静默回退会让用户以为绑定生效，实际仍在走 TUN）。

落地证据：`internal/transport/tunnel.go:275`（`localAddr` 归一化）、
`internal/transport/tunnel.go:233`（TCP 路径的 `LocalAddr`）、
`internal/transport/tunnel.go:159`（QUIC 路径的 `ListenUDP`）、
`internal/transport/localaddr_test.go:104`（`TestDialLocalAddr`）、
`:152`（bare IP）、`:173`（无效地址报错）、
`cmd/gui/view.go:665`（GUI「源地址」字段）。

#### Scenario: 代理 TUN 全局模式下仍能连通

- **WHEN** 本机代理把 `0.0.0.0/1` + `128.0.0.0/1` 全部导进 utun，隧道流量被劫持
- **THEN** 填物理网卡 IP 后连接立刻打通（生产实测：ALPN 协商成功、真实请求 200）

#### Scenario: 无效地址显式失败

- **WHEN** `-local-addr` 填写了不存在的地址
- **THEN** 启动即报错并说明，不静默降级为「让内核选」

### Requirement: TLS SNI 可显式指定，IP 直连必须给

客户端 SHALL 支持 `-server-name` 指定 TLS SNI。用 IP 直连时必须显式给出证书上的域名：
服务端证书 SAN 通常只有 DNS 没有 IP，留空会按 host（那个 IP）推导 SNI，必然
`x509: cannot validate certificate ... doesn't contain any IP SANs`。
内置默认组合（`-server` 默认 IP）由 `Config.Normalize` 自动补 `DefaultServerName`；
用户自定义 `-server` 时**不得**强塞内置 SNI。

落地证据：`internal/relayclient/runner.go:44`（`DefaultServerName`）、
`internal/relayclient/runner.go:84`（`Normalize` 的补值条件）、
`internal/transport/tunnel.go:262`（`serverName()` 的推导）、
`internal/relayclient/default_test.go`（两个回归护栏
`TestDefaultsAreConnectable` / `TestCustomServerKeepsEmptyServerName`）。

#### Scenario: 内置默认组合可直接连

- **WHEN** 用户不做任何配置直接启动客户端
- **THEN** SNI 已被补成证书域名，不会因 IP SAN 缺失而握手失败

#### Scenario: 自定义上游不被污染

- **WHEN** 用户把 `-server` 改成别家的 `example.com:28443` 且不给 SNI
- **THEN** 不自动填内置域名，交给 transport 按 host 推导

### Requirement: TLS 版本下界

实现 SHALL 对 TCP/TLS 与服务端监听使用 `MinVersion: tls.VersionTLS12`，QUIC 使用
`VersionTLS13`（QUIC 强制要求 TLS 1.3）。

落地证据：`internal/transport/tunnel.go:238`（TCP 客户端）、
`internal/transport/tunnel.go:315`（服务端）、
`internal/transport/tunnel.go:164`（QUIC 客户端）。

#### Scenario: 拒绝降级到 TLS 1.1

- **WHEN** 对端只支持 TLS 1.1 及更早
- **THEN** 握手失败，不降级

## 已知边界

- **QUIC 生产未启用**：有回环端到端测试通过，生产链路仍走 TCP/TLS，启用需放通 UDP
  端口并验证中间设备。这一条是当前事实，不是设计目标（见 changes/
  `enable-quic-in-production`）。
- `FrameAck`（类型 5）已定义为「保留」，当前无使用方。
