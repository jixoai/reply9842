## Why

`DEPLOY.md` 把 systemd 列为三种部署方式之一，给出的 unit 全文只存在于 Markdown 代码块里，
仓库没有任何可被脚本引用的 unit 资产：

```
ls deploy/          → 空目录（git 中零文件）
git ls-files | rg 'service|deploy'  → 只有 scripts/deploy.sh
```

后果：
- `scripts/deploy.sh` 部署的是**裸二进制**（`/opt/reply9842/reply9842-server` + install +
  restart），它能工作**只因为**目标机上已经手工建好了 unit。新机器上这个脚本跑不了。
- unit 里的参数（`-spool-dir`、`-cert`、`-key`、`-upstream`）与代码的默认值没有单一
  来源，改默认值时要手工同步 Markdown。
- 历史上这里出过真 bug：unit 的续行 `\n` 写成字面量导致 `-spool-dir` 等参数被吞
  （a7f017a 记录的第 6 个 bug）。文本形式的 unit 没有任何校验能拦住这类错误。

## What Changes

- 新增 `deploy/reply9842-server.service`（真实文件，非代码块），内容与 DEPLOY.md 一致：
  `Type=simple`、`Restart=always`、`RestartSec=5`、`User=reply9842`、
  `WorkingDirectory=/opt/reply9842`、显式全部参数。
- 新增 `deploy/reply9842-server.env.example`（若决定走 `EnvironmentFile`），或在 unit 里
  直接用 `ExecStart` 参数（二选一，不要两套并存）。
- `scripts/deploy.sh` 增加：若目标机 `/etc/systemd/system/reply9842-server.service` 不存在，
  从本地 `deploy/` 推送并 `daemon-reload`；若存在但与本地不一致，打印 diff 并**要求
  显式 `--force-unit`** 才覆盖（避免脚本悄悄改掉运维手工调过的参数）。
- DEPLOY.md 的 unit 段落改为「见 `deploy/reply9842-server.service`」+ 说明如何按环境改。

## Capabilities

### New Capabilities

无。

### Modified Capabilities

- `deployment`: 追加「systemd unit 必须作为可被脚本引用的仓库资产存在」需求。

## Impact

- 新增 `deploy/` 下的两个文件（该目录已存在但为空）。
- 改动 `scripts/deploy.sh`、`DEPLOY.md`。
- 安全性：脚本推 unit 需要 sudo；覆盖既有 unit 需要显式确认，不自动覆盖。
