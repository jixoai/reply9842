# Tasks: ship-systemd-unit-asset

## 1. 资产

- [ ] 1.1 新增 `deploy/reply9842-server.service`，内容取自 DEPLOY.md 现有代码块（保持
      参数完全一致，先做等价搬迁，不做「顺手优化」）
- [ ] 1.2 决定是否用 `EnvironmentFile`：若用，新增 `deploy/reply9842-server.env.example`
      并把 unit 里 `ExecStart` 的长参数行拆开；若不用，不加 env 文件（不要两套并存）
- [ ] 1.3 在 unit 里加一行注释标明「仓库内唯一事实来源是此文件，DEPLOY.md 只是摘录」
- [ ] 1.4 若 unit 引用了 `-spool-dir` 等目录，确保 `WorkingDirectory` 与
      `StateDirectory=`（或文档里的 `mkdir -p` 步骤）一致

## 2. 部署脚本

- [ ] 2.1 `scripts/deploy.sh`：目标机无 unit 时从 `deploy/` 推送 + `daemon-reload` +
      `enable`
- [ ] 2.2 目标机有 unit 但与本地不同：打印 diff，**不覆盖**，提示用 `--force-unit`
- [ ] 2.3 `--build-only` 与 `--force-unit` 的用法写进脚本头部注释与 `--help`

## 3. 验证

- [ ] 3.1 unit 语法校验：`systemd-analyze verify deploy/reply9842-server.service`
- [ ] 3.2 在一次性容器/临时机上实测：脚本能从零把服务跑起来（无手工前置）
- [ ] 3.3 DEPLOY.md 的 unit 段落指向真实文件，且内容与文件一致（去掉重复的全文代码块或
      说明「以下为摘录」）

## 验收标准

- `deploy/reply9842-server.service` 存在且能通过 `systemd-analyze verify`。
- 全新机器上 `./scripts/deploy.sh <host>` 能完成部署并让服务 active，无需手工建 unit。
- 目标机已有自定义 unit 时脚本不会静默覆盖。
