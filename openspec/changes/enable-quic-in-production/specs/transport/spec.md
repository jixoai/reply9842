## ADDED Requirements

### Requirement: QUIC 的生产启用状态必须有明确结论并与文档一致

关于 QUIC 是否在生产启用，项目 SHALL 保持「代码默认值、测试、README/DEPLOY 文档」三者
一致，且该状态必须有实测依据支撑，不允许悬空。默认 `-transport` 的取值必须有断言护栏，
改动时必须同步更新文档。

依据：README 已知限制写「QUIC 生产未启用」，而 `DefaultTransport = "auto"` 会在每次连接
先试 QUIC；提交 0194704 记录过把默认改成 tcp 的决定，当前代码是 auto 却无任何说明。

#### Scenario: 三者一致

- **WHEN** 检查 `DefaultTransport` 常量、`internal/relayclient/default_test.go` 的断言、README/DEPLOY 的措辞
- **THEN** 三者描述同一个事实（启用 / 不启用），且文档给出实测依据

#### Scenario: 默认值改动被护栏拦下

- **WHEN** 有人改动默认 transport 但不改文档
- **THEN** 有测试或文档中的显式声明让人立刻看到不一致（至少一条断言钉住当前值）

### Requirement: 文档不得声明代码不支持的能力

README / DEPLOY SHALL NOT 描述实现中不存在的能力。凡涉及协议特性（如 0-RTT）的表述，
必须能指到对应的配置代码。

依据：`DEPLOY.md` 写「弱网下重连更快（0-RTT）」，而 `quic.Config` 未设置 `Allow0RTT`，
`rg -n 'Allow0RTT'` 在整个仓库无命中。

#### Scenario: 0-RTT 要么真实要么删掉

- **WHEN** 文档出现 0-RTT 字样
- **THEN** 代码里存在 `Allow0RTT: true` 的显式配置与相应 e2e；否则该字样被删除
