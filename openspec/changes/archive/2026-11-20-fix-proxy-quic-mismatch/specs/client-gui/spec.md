## ADDED Requirements

### Requirement: 代理模式下传输选项必须收敛

GUI 在网络出口为「代理」时 SHALL 只提供「自动 / TCP」两种传输方式；若当前
配置或旧设置文件里存在「代理 + QUIC」，SHALL 在启动/切换时自动改为 `auto`
并给出提示。

#### Scenario: 切到代理时已有 QUIC

- **WHEN** 用户把网络从直连/系统切换为代理，且传输方式原为 QUIC
- **THEN** 传输方式变为「自动」，并出现「代理模式不支持 QUIC」的提示
