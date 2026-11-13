# deployment Specification

## Purpose

部署与运维：服务端三种部署路径（Docker / systemd / npm）、容器镜像约束、
二进制安全传输、官网 Pages 发布。目标读者是「把服务端放到一台能直连上游的机器上」的
运维者。

## Requirements

### Requirement: 服务端镜像为 scratch 单二进制

`Dockerfile.server` SHALL 三阶段构建最终落到 `scratch`（约 12 MB）：纯 Go 静态二进制、
非 root（65532:65532）、内置时区数据、依赖层单独缓存（`COPY go.mod go.sum` + 预编译
占位再 COPY 源码，源码变更不重编依赖）。

落地证据：`Dockerfile.server`（三阶段结构、`FROM scratch`、`USER 65532:65532`、
`COPY --from=build /usr/share/zoneinfo`）。

#### Scenario: 镜像体积

- **WHEN** 构建并查看镜像大小
- **THEN** 约 12 MB 量级，不含 shell / 包管理器 / 构建工具

#### Scenario: 改源码不重编依赖

- **WHEN** 只改动 `cmd/` 或 `internal/` 下的 Go 源码后重新构建
- **THEN** `go mod download` 层命中缓存，不重新拉取依赖

### Requirement: compose 同时放通 TCP 与 UDP

`docker-compose.server.yml` SHALL 同时映射隧道端口的 TCP 与 UDP（QUIC 走 UDP，TCP/TLS
走 TCP），健康检查端口只绑回环（`127.0.0.1:${HEALTH_PORT}`），spool 用 named volume
持久化（重启后幂等区可复用）。

落地证据：`docker-compose.server.yml`（`${TUNNEL_PORT:-8443}:8443/tcp` 与 `/udp`、
`127.0.0.1:${HEALTH_PORT:-8080}:8080`、`reply9842-spool` volume）。

#### Scenario: 只放通 TCP 也能工作

- **WHEN** 宿主机防火墙只放通 TCP 8443
- **THEN** TCP/TLS 路径正常服务，客户端 auto 模式降级成功

#### Scenario: 健康检查不对外

- **WHEN** 从公网访问健康检查端口
- **THEN** 不可达（只绑 127.0.0.1）

#### Scenario: 重启后幂等区还在

- **WHEN** 容器被重启
- **THEN** spool volume 保留，`idem/` 下的已完成响应仍可被同 key 请求复用

### Requirement: 上游同机的两种接法

文档 SHALL 给出两种上游接法：上游也是容器时放进同一 compose 网络（
`UPSTREAM=http://magpie:3431`）；上游是宿主机服务时用
`extra_hosts: ["host.docker.internal:host-gateway"]` 并指向
`http://host.docker.internal:3431`。

落地证据：`DEPLOY.md` 的「上游在同机怎么办」章节、
`docker-compose.server.yml` 的 `UPSTREAM` 环境变量与注释。

#### Scenario: 容器内 127.0.0.1 不是宿主机

- **WHEN** 直接用默认 `UPSTREAM=http://127.0.0.1:3431` 而上游在宿主机
- **THEN** 连不通；文档给出 `host.docker.internal` 的解法

### Requirement: 二进制部署必须校验 sha256

`scripts/deploy.sh` SHALL：不设短超时（跨境链路慢就等）、传输后**必须**比对 sha256
（不一致立即中止）、部署后用 `systemctl is-active` + 日志无 SEGV 二次验证。

依据：2026-11-13 事故——`timeout 50` 掐断 `scp` 留下半截 ELF，`file` 报 normal，
部署后 SEGV，误导排查方向。

落地证据：`scripts/deploy.sh:1`（事故原因与三条铁律的完整说明）、
`:53`（scp 不带 timeout）、`:60`（远端 sha256 比对，不一致 exit 1）、
`:88`（验证 `systemctl is-active` 与 SEGV 计数）。

#### Scenario: 半截文件被拦下

- **WHEN** 传输后本地与远端 sha256 不一致
- **THEN** 脚本 exit 1 并打印两侧哈希，绝不执行 install/restart

#### Scenario: 服务起来后仍要做二次验证

- **WHEN** `systemctl is-active` 返回 active
- **THEN** 仍要再查 2 秒内的日志有没有 SEGV（SEGV 重启循环时状态可能是 activating）

### Requirement: systemd 部署给出完整 unit

`DEPLOY.md` SHALL 给出可直接复制的 unit 文件：`Restart=always`、`RestartSec=5`、
显式 `-spool-dir`、`-cert`/`-key`、`-upstream`。注意 unit 里的续行反斜杠必须是真的
换行（历史 bug：`\n` 写成字面量导致 `-spool-dir` 等参数被吞）。

落地证据：`DEPLOY.md` 的「二进制 + systemd」章节、
`git log` 中 a7f017a 的「修复的 6 个真 bug」第 6 条（systemd unit 的 `\n` 写成字面量）。

#### Scenario: 参数不被吞

- **WHEN** 按文档粘贴 unit 并 `systemctl show -p ExecStart`
- **THEN** `-spool-dir` / `-tmp-mb` 等参数完整出现在 ExecStart 里

### Requirement: 反向代理配置示例

文档 SHALL 给出 nginx/openresty 反代示例：`proxy_http_version 1.1`、
`proxy_buffering off`（SSE 流式必须）、`proxy_read_timeout`/`proxy_send_timeout` 600s，
并明确**不要**在反代层再压缩（隧道内已 zstd 压过，重复压缩浪费 CPU）。

落地证据：`DEPLOY.md` 的「反向代理注意」章节。

#### Scenario: SSE 不被反代缓冲

- **WHEN** 前面挂了 nginx 且未关 `proxy_buffering`
- **THEN** 客户端看到的是一段一段跳（甚至憋到结束），文档要求显式 `off`

### Requirement: 官网站点用 GitHub Pages 发布

`site/` SHALL 在 `site/**` 或 `.github/workflows/pages.yml` 变更时自动部署到 GitHub Pages，
也可 `workflow_dispatch` 手动触发。构建前校验 `site/index.html` 与 `site/CNAME`
存在。同组并发只允许一个部署，新 push 取消排队的旧部署但不打断运行中的部署。

落地证据：`.github/workflows/pages.yml`（`paths` 过滤、Verify site entry 步骤、
`concurrency` 配置）。

#### Scenario: 改 Go 代码不触发站点重建

- **WHEN** 只改动 `internal/` 下的 Go 文件并 push
- **THEN** Pages workflow 不触发

## 已知边界（需求与现状不符，逐条见 changes/）

1. **`deploy/` 目录是空的且未入库**：仓库根有空的 `deploy/` 目录，git 中无任何文件；
   而 `docker-compose.server.yml` 引用的镜像 `ghcr.io/jixoai/reply9842-server:latest`
   **没有任何 CI 构建并推送**——`.github/workflows/` 只有 `.github/workflows/release.yml` 与 `.github/workflows/pages.yml`，
   两者都不构建 Docker 镜像。见 changes/`add-server-image-ci`。
2. **systemd unit 没有入库资产**：只在 `DEPLOY.md` 里以代码块形式存在，无法被自动化部署
   脚本直接复用。见 changes/`ship-systemd-unit-asset`。
3. **`scripts/deploy.sh` 默认目标是 `gaubee-cloud`（北京机）**，而 README 与实际生产
   （2026-11-13 前）指向香港；默认值未随基础设施变更更新。见 changes/
   `update-deploy-default-host`。
4. **DEPLOY.md 声称 QUIC 弱网重连更快（0-RTT）**，但代码里没有配置 `Allow0RTT`，
   实际提供不了 0-RTT。见 changes/`enable-quic-in-production`。
