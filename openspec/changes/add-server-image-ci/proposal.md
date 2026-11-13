## Why

`docker-compose.server.yml` 引用 `ghcr.io/jixoai/reply9842-server:latest`，`Dockerfile.server`
也存在，但**没有任何工作流构建并推送这个镜像**——`.github/workflows/` 只有 `.github/workflows/release.yml`
与 `.github/workflows/pages.yml`，两者都不含 docker 相关步骤：

```
rg -c 'docker' .github/workflows/release.yml   → 0
rg -n 'ghcr|buildx|docker/build' .github/workflows/*.yml → 无命中
rg -n 'Dockerfile.server' .github/workflows/*.yml → 无命中
```

后果：`docker compose -f docker-compose.server.yml up -d` 永远拉到一个不存在的镜像标签。
DEPLOY.md 把 Docker 列为「推荐」的第一种部署方式，这是**文档承诺与实现不符**。

同时 `.github/workflows/release.yml` 只产二进制资产，服务端运维者必须自己 `docker build`。

## What Changes

- 新增 `.github/workflows/server-image.yml`：在 tag `v*`（与 release 同触发）时
  `docker/build-push-action` 构建 `Dockerfile.server` 并推送 `ghcr.io/jixoai/reply9842-server`
  的 `latest` 与 `${{ github.ref_name }}` 两个标签；`push` 到 main 时只构建不推送
  （PR 级别的构建验证）。
- 镜像构建参数包含 `VERSION`（`-X main.version`），使 `reply9842-server --help` 与日志
  能报出真实版本。
- 文档对齐：`DEPLOY.md` 的 Docker 章节补「镜像来源与 tag 规则」；若镜像设为 private，
  必须写清 `docker login ghcr.io` 的前置（参考 gaubee.com 的同类经验）。

## Capabilities

### New Capabilities

无。

### Modified Capabilities

- `deployment`: 追加「服务端镜像必须由 CI 构建并推送，compose 引用的 tag 必须真实存在」需求。

## Impact

- 新增 `.github/workflows/server-image.yml`。
- 改动 `DEPLOY.md`（Docker 章节）、`docker-compose.server.yml`（可能加 `pull_policy`）。
- CI 额度：镜像构建是单平台 amd64（服务端目标机是 x86 云主机），一套三阶段构建约 1–2 分钟。
- 权限：需要 `packages: write`；GITHUB_TOKEN 推送的包默认 private，需在仓库设置里设为
  public 或让服务器 `docker login`。
