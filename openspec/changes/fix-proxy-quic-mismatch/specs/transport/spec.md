## ADDED Requirements

### Requirement: 代理链路禁止 QUIC 且失败必须可操作

客户端 SHALL 在配置了 `Proxy` 时永不尝试 QUIC：显式 `quic` + 代理必须**立即**
返回可操作错误（提示改用 tcp/auto），`auto` 模式的代理分支不得被历史 QUIC
失败偏好覆盖。GUI 在代理模式下 SHALL 只提供「自动 / TCP」，并自动纠正旧的
「代理 + QUIC」配置。TLS 被中间网络切断（`tls: EOF`）时 SHALL 提示用户切换
「直连」或「代理」，而不是原样展示底层错误。

落地证据：`internal/transport/tunnel.go`（Dial 早退与 prefer 互斥）、
`internal/transport/proxy_mode_test.go`、`cmd/gui/view.go`（transportChoices /
transportForNetwork）、`cmd/gui/format.go`（humanError）、`cmd/gui/network_mode_test.go`、
`cmd/gui/format_test.go`。

#### Scenario: 代理 + 显式 QUIC

- **WHEN** 网络出口为代理且传输方式被设为 QUIC
- **THEN** 拨号在 300ms 内失败，错误明确包含「代理模式不支持 QUIC」，不产生 UDP 探测

#### Scenario: 历史 QUIC 失败后切到代理

- **WHEN** 客户端曾因 QUIC 失败把 prefer 置为 TCP，随后启用代理并以 auto 拨号
- **THEN** 只尝试 TCP（经代理 CONNECT），错误中不出现 quic 尝试

#### Scenario: TUN 劫持导致 TLS 中断

- **WHEN** 拨号返回 `tcp tls: EOF`
- **THEN** GUI 失败行提示「把网络切到直连或代理后重新启动」，并说明疑似代理 TUN 劫持
