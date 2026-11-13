## ADDED Requirements

### Requirement: 服务端镜像必须由 CI 构建并推送

服务端容器镜像 SHALL 由 CI 在打 tag 时构建并推送到 `ghcr.io/jixoai/reply9842-server`，
同时打 `latest` 与版本号两个标签。`docker-compose.server.yml` 引用的镜像标签必须真实
存在——不允许引用一个从未被构建的镜像。

镜像构建必须传入 `VERSION` 构建参数，使二进制里的 `main.version` 是真实版本号。

#### Scenario: compose 能拉到镜像

- **WHEN** 在干净机器上执行 `docker compose -f docker-compose.server.yml up -d`
- **THEN** 镜像拉取成功并启动（除证书与上游配置外无额外手工步骤）

#### Scenario: 版本可查

- **WHEN** 在容器内执行 `/reply9842-server --help`（或看启动日志）
- **THEN** 打印的版本号等于发布 tag（不是 `dev`）

#### Scenario: 镜像内的二进制能跑

- **WHEN** CI 构建完成后执行 `docker run --rm <image> --help`
- **THEN** 退出码 0（scratch 镜像无 shell，这是唯一能验证二进制可执行的检查）
