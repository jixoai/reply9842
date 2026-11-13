# Tasks: update-deploy-default-host

## 1. 裁决

- [ ] 1.1 确认 `gaubee-cloud` 与 `47.82.76.102`（生产）是否同一台。若不是，记录两者
      分别是什么用途
- [ ] 1.2 决定：保留默认并改成生产别名 / 移除默认强制显式传参（推荐后者）

## 2. 脚本改动

- [ ] 2.1 若移除默认：`HOST="${1:?用法: ./scripts/deploy.sh <ssh-host> [--build-only]}"`，
      并保留 `--build-only` 分支
- [ ] 2.2 打印「将部署到 <host>（<解析出的 IP 若可得>）」，非 `--yes` 时等 3 秒可 Ctrl-C
      取消
- [ ] 2.3 头部注释补「ssh 别名 ↔ 生产/灾备 对应关系」表
- [ ] 2.4 `--build-only` 与 `--yes` 写进用法说明

## 3. 验证

- [ ] 3.1 `./scripts/deploy.sh` 不带参数：按裁决要么报用法错误（推荐），要么打印目标并
      等待确认
- [ ] 3.2 `./scripts/deploy.sh --build-only`：只编译 + 打印 sha256，退出码 0
- [ ] 3.3 `./scripts/deploy.sh <真实别名> --yes`：完整走一遍（含 sha256 校验与
      systemctl 验证），确认无回归

## 验收标准

- 不会因为「忘了传参数」把二进制部署到非预期机器。
- 每次部署前，屏幕上明确显示目标 host。
- `scripts/deploy.sh` 头部能读到「哪个别名是哪台机器」的对应关系。
