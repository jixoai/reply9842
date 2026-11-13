## ADDED Requirements

### Requirement: 部署脚本不得有隐式生产目标

部署脚本 SHALL NOT 在未显式指定目标时静默落到某台机器。要么强制传 host（首选），要么
打印目标并留出可取消的确认窗口。脚本头部必须写明 ssh 别名与机器用途的对应关系。

#### Scenario: 忘记传参不会误部署

- **WHEN** 执行 `./scripts/deploy.sh` 不带 host
- **THEN** 脚本报用法错误退出（或打印目标并等待可取消的确认），不直接开始 scp

#### Scenario: 目标可见

- **WHEN** 脚本开始传输前
- **THEN** 屏幕上已明确显示「将部署到 <host>」，操作者能当场发现不对

#### Scenario: 只编译时不要求 host

- **WHEN** 执行 `./scripts/deploy.sh --build-only`
- **THEN** 完成交叉编译并打印 sha256 后退出 0，不涉及任何远端
