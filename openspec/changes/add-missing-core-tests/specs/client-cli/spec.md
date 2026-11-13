## ADDED Requirements

### Requirement: 子命令嗅探必须有测试

CLI 的「无子命令即 serve」是一条明确的向后兼容承诺（文档：「旧写法的脚本无需改动」），
SHALL 有测试覆盖：已知子命令各自分派、首参数以 `-` 开头时按 serve、未知子命令退出码 2。

#### Scenario: 旧写法仍可用

- **WHEN** 参数为 `-listen 127.0.0.1:18899`
- **THEN** 走 serve 分支（不报「未知子命令」）

#### Scenario: 未知子命令退出码为 2

- **WHEN** 首参数是不在 `serve|status|version|help` 内的裸词
- **THEN** 打印用法并退出码 2

#### Scenario: 每个已知子命令分派正确

- **WHEN** 分别以 `serve` / `status` / `version` / `help` 为首参数
- **THEN** 各自进入对应分支，不互相串
