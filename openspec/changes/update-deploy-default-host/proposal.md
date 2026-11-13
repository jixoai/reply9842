## Why

`scripts/deploy.sh` 的默认部署目标是 `gaubee-cloud`：

```
scripts/deploy.sh:21   #   ./scripts/deploy.sh              # 默认目标 gaubee-cloud
scripts/deploy.sh:26   HOST="${1:-gaubee-cloud}"
```

而客户端默认服务端地址是 `47.82.76.102:28443`（`internal/relayclient/runner.go:43` 的
`DefaultServer`，README/INSTALL 也用它）。按 Owner 环境记录，`47.82.76.102` 是**香港**
那台（2026-11-13 起是生产源站），而 `gaubee-cloud` 是按项目惯例命名的北京机 ssh 别名。
两者不是同一台。

后果：直接跑 `./scripts/deploy.sh` 会部署到**非生产**的那台（或部署失败，若该 ssh 别名
不存在）。脚本注释里的「默认目标 gaubee-cloud」与代码里的默认服务端地址是两条互相矛盾
的事实来源，且都没有测试或校验。

另外脚本注释提到的事故记录是「手工 scp 到香港机」，说明实际在生产用的就是香港，但默认
落地值没跟上。

## What Changes

- 二选一，必须给结论：
  (a) 把默认目标改成生产机对应的 ssh 别名；或
  (b) **移除默认值**，强制显式传 host（最保守：部署目标不该有隐式默认）。`--build-only`
      保持可用。
- 无论选哪个，脚本启动时打印「本次将部署到 <host>」并**要求 3 秒确认**（或 `--yes` 跳过），
  避免手滑打到生产。
- 在脚本头部注释里写明「ssh 别名与生产 IP 的对应关系，以及为什么这样命名」——当前注释
  只说默认是 gaubee-cloud，没有任何依据。
- 补充：`scripts/deploy.sh` 目前只部署 linux/amd64 单平台二进制；若生产是多架构或有
  灾备机，文档要说明如何切换。

## Capabilities

### New Capabilities

无。

### Modified Capabilities

- `deployment`: 追加「部署脚本不得有隐式生产目标」需求。

## Impact

- 改动 `scripts/deploy.sh`（默认值、确认提示、头部注释）。
- 文档：DEPLOY.md 若提到该脚本，需同步。
- 风险低（脚本级），但避免了一次「打到错误的机器上」的事故类型。
