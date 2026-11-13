# Tasks: add-server-image-ci

## 1. 工作流

- [ ] 1.1 新增 `.github/workflows/server-image.yml`：
      触发 `push: tags: ['v*']` + `workflow_dispatch`；`permissions: contents: read,
      packages: write`
- [ ] 1.2 构建步骤用 `docker/build-push-action@v6`，`context: .`、
      `file: Dockerfile.server`、`platforms: linux/amd64`、
      `build-args: VERSION=${{ github.ref_name }}`（去掉 `v` 前缀）
- [ ] 1.3 标签规则：`ghcr.io/jixoai/reply9842-server:latest` 与
      `:${ref_name#v}` 双标
- [ ] 1.4 `workflow_dispatch` 与「非 tag 的 push 到 main」只构建不推送（验证 Dockerfile
      仍可编译），避免每次改代码都污染 registry

## 2. 可观测与安全

- [ ] 2.1 构建后 `docker run --rm <img> --help` 断言二进制可执行（scratch 镜像无 shell，
      只能跑二进制本身）
- [ ] 2.2 在 README/DEPLOY 写明镜像可见性（public / private）与拉取前置命令
- [ ] 2.3 验证 `docker inspect` 的 `Health.Status` 在本地 compose 起来后为 healthy
      （探针模式 `--health-probe`）

## 3. 文档对齐

- [ ] 3.1 `DEPLOY.md` 的 Docker 章节补「镜像由哪个工作流产出、有哪些 tag」
- [ ] 3.2 `docker-compose.server.yml` 加 `pull_policy: always`（或说明为何不加）

## 验收标准

- push 一个 `v*` tag 后，`docker pull ghcr.io/jixoai/reply9842-server:<version>` 成功。
- `docker compose -f docker-compose.server.yml up -d` 在干净机器上能起（除证书与上游配置外
  无额外手工步骤）。
- `docker inspect` 的 Health 为 healthy。
