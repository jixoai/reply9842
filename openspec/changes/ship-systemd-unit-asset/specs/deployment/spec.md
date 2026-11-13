## ADDED Requirements

### Requirement: systemd unit 必须是可被脚本引用的仓库资产

systemd unit SHALL 作为真实文件存在于仓库（`deploy/reply9842-server.service`），
能被部署脚本直接推送，而不是只以 Markdown 代码块形式存在于文档。文档可以摘录，但必须
指回该文件并标明顺序。

unit 必须能通过 `systemd-analyze verify`；任何参数（`-spool-dir` / `-cert` / `-key` /
`-upstream`）都以该文件为准。

#### Scenario: 全新机器一键部署

- **WHEN** 在没有任何前置配置的新机器上运行 `./scripts/deploy.sh <host>`
- **THEN** unit 被推送、`daemon-reload`、服务启动并 active，无需手工建 unit

#### Scenario: 不静默覆盖运维定制

- **WHEN** 目标机已有 unit 且与仓库版本不同
- **THEN** 脚本打印 diff 且不覆盖，需显式 `--force-unit` 才覆盖

#### Scenario: 语法可验证

- **WHEN** 执行 `systemd-analyze verify deploy/reply9842-server.service`
- **THEN** 无报错（特别是不会把续行反斜杠写成字面量 `\n`）
