## ADDED Requirements

### Requirement: insecure 模式必须运行时可见且可审计

启用 `-insecure`（跳过证书校验）时，客户端 SHALL 在启动输出打印显著警告，GUI SHALL 显示
持续可见的状态标记，并且逐请求记录里 SHALL 带可识别的 insecure 标记。任何一次
insecure 连接都必须事后可审计。

#### Scenario: 启动即警告

- **WHEN** 客户端以 `-insecure` 启动
- **THEN** 启动日志出现一行明确警告（含「中间人可解开全部流量」语义），不是 debug 级

#### Scenario: 界面持续可见

- **WHEN** GUI 处于 insecure 模式
- **THEN** 界面上有一个持续显示的标记（非 5 秒提示），说明当前未校验证书

#### Scenario: 事后可筛出

- **WHEN** 用 `jq 'select(.insecure)'` 过滤当日 JSONL
- **THEN** 能筛出全部 insecure 连接
