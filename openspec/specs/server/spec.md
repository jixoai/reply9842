# server Specification

## Purpose

隧道服务端 `reply9842-server`：接受 QUIC/TCP 连接、解帧解压、校验 `ResultHash`、
转发上游、把响应流式落盘并推回客户端。它是隧道的远端终点，也是唯一持有字典缓存与
幂等区的一方。

## Requirements

### Requirement: 启动参数集合与默认值

服务端 SHALL 支持且仅支持以下参数（默认值以代码为准，README / DEPLOY.md 必须与之一致）：
`-listen`(0.0.0.0:28443) `-upstream`(http://127.0.0.1:3431)
`-uphost`(magpie.aiweb.xin) `-cert`/`-key`(必填) `-cache-mb`(256)
`-spool-dir`(/var/lib/reply9842/spool) `-tmp-mb`(512) `-idem-mb`(2048)
`-upstream-timeout`(10m) `-log` `-stats` `-health-addr`(空) `-health-probe`(false)。

缺 `-cert` / `-key` 必须启动即失败（`log.Fatalf`），不得降级为明文监听。

落地证据：`cmd/server/main.go:56`（`main` 里的 flag 定义）、
`cmd/server/main.go:105`（证书缺失时 Fatalf）、`cmd/server/main.go:108`（日志默认路径）。

#### Scenario: 缺证书直接拒绝启动

- **WHEN** 未提供 `-cert` 或 `-key`
- **THEN** 进程以非零码退出并打印「必须提供 -cert 与 -key」

#### Scenario: 服务端日志按日滚动

- **WHEN** 未指定 `-log`
- **THEN** 默认写 `~/.reply9842/server-<date>.jsonl`，统计写同目录
  `server-stats.json`

### Requirement: 上游必须是同机回环地址

上游 base URL SHALL 指向 `127.0.0.1` 或同机内网地址。若填公网域名，流量会绕一整圈公网
（实测某云主机公网 IP 不在网卡上，`172.17.61.35` 才是真实内网地址）。

落地证据：`README.md` 快速开始章节的加粗警告、
`cmd/server/main.go:59`（flag 说明「同机回环最优」）。

#### Scenario: 误填公网域名

- **WHEN** `-upstream` 填了公网域名
- **THEN** 功能仍可用但延迟显著上升（文档已警告；这是使用约束不是代码强制）

### Requirement: 上游请求改写与 hop-by-hop 头剥离

转发上游时 SHALL：用 `-uphost` 覆盖 `Host` 头（同时设 `req.Host`）、删除
`Accept-Encoding`（避免上游压缩后再由本层压缩）、丢弃 hop-by-hop 头
（connection / keep-alive / proxy-authenticate / proxy-authorization / te / trailer /
transfer-encoding / upgrade / content-length / host）。

落地证据：`cmd/server/main.go:557`（`startPump` 里的头处理）、
`cmd/server/main.go:752`（`isHopByHop`）、`cmd/server/main.go:569`（遍历
`meta.DecodeHeaders` 并跳过 hop-by-hop）。

#### Scenario: 上游不返回 gzip

- **WHEN** 客户端原始请求带 `Accept-Encoding: gzip`
- **THEN** 转发上游时该头被删除，上游返回明文，下行压缩由本层统一负责

#### Scenario: Host 被改写

- **WHEN** `-uphost magpie.aiweb.xin` 且客户端请求的 Host 是 127.0.0.1:18899
- **THEN** 上游收到 `Host: magpie.aiweb.xin`，网关正常路由

### Requirement: 健康检查端点与 fail-fast 端口占用

启用 `-health-addr` 时 SHALL 提供 `GET /healthz` 返回 `ok`（text/plain）。端口占用必须
**同步先 Listen 再起 goroutine**，冲突即 fatal——原实现「打日志继续跑」会让探活打到别人
端口并返回 200，编排层看到假绿。

`-health-probe` 为探针模式：GET 自己的 `/healthz`，成功 exit 0、失败 exit 1。
这是给 scratch 镜像的 HEALTHCHECK 用的（容器内无 shell、无 curl）。

落地证据：`cmd/server/main.go:167`（`healthAddr` 的 mux 与 `net.Listen` fail-fast 注释）、
`cmd/server/main.go:68`（`-health-probe` flag 与说明）、
`Dockerfile.server`（`HEALTHCHECK CMD ["/reply9842-server", "--health-probe", ...]`）。

#### Scenario: 端口被占时不装绿

- **WHEN** `-health-addr` 指定的端口已被别的进程占用
- **THEN** 进程 fatal 退出，不启动一个没有健康端点的服务

#### Scenario: scratch 容器能自探活

- **WHEN** 容器内执行 `/reply9842-server --health-probe -health-addr 127.0.0.1:8080`
- **THEN** 健康时 exit 0、不健康时 exit 1，无需 curl

### Requirement: 上游超时独立于客户端断开

上游请求超时 SHALL 由 `-upstream-timeout` 控制（默认 10m）且**不随客户端断开取消**，
派生自 `context.WithoutCancel`。这是断线续传能成立的前提。

落地证据：`cmd/server/main.go:558`（`WithTimeout(WithoutCancel(...), upstreamTimout)`）、
`cmd/server/main.go:131`（启动日志明确打印「不随客户端断开取消」）。

#### Scenario: 客户端走开后上游仍受控

- **WHEN** 客户端断开且上游迟迟不返回
- **THEN** 满 10 分钟后上游请求被取消，条目以错误结束并被驱逐（不占用预算）

### Requirement: 解码校验与 DeltaMiss 分支

服务端 SHALL 在转发前完成：解帧 → 按 `reqHead.BaseHash` 从 store 取字典 → 解 dcz/zstd →
校验 `ResultHash`。任何一步失败都必须返回错误帧（`FrameError`）而不是把可疑数据转发上游。

落地证据：`cmd/server/main.go:640`（`handleResume`）、
`cmd/server/main.go:228`（`serveOne` 的主解码流程）、
`cmd/server/main.go:296` 附近（`ResultHash` 校验与 422 记录）、
`cmd/server/main.go:733`（`s.store.Get(declared)`）。

#### Scenario: 哈希不匹配不转发上游

- **WHEN** 解码得到的 body 与 `ResultHash` 不符
- **THEN** 返回错误帧、记为 422、不上游、不写缓存

### Requirement: 服务端统计与逐请求日志

服务端 SHALL 与服务端采集器共用同一套 `stats` 结构：逐请求 JSONL（append）+
汇总快照（覆盖写），字段含 `transport`（quic/tcp）、`up_raw`/`up_wire`、
`down_raw`/`down_wire`、`enc`、`status`。

落地证据：`cmd/server/main.go:116`（`stats.New(c.logPath, c.statsPath)`）、
`internal/stats/stats.go:46`（`Record` 字段定义）、
`internal/stats/stats.go:171`（`Open`：先流式重建聚合再打开追加）。

#### Scenario: 服务端重启后统计不清零

- **WHEN** 服务端进程重启
- **THEN** `Open` 先从当日 JSONL 流式重建聚合，再继续追加，历史不被清零

## 已知边界

- 服务端**不做调用方鉴权**：没有 API key 白名单、没有租户概念，靠网络边界（只暴露
  TLS 端口 + 上游同机）隔离。见 changes/`add-server-auth-and-tenant-isolation`。
- 字典缓存与幂等区都是**进程内/单机**：没有多实例共享，也无法水平扩展（README 第
  3 条设计决策里「服务端完全无状态，可水平扩展」的说法与实现不符，见 changes/
  `fix-stateless-claim-vs-implementation`）。
- `internal/store` 有 `Stats()`（hits/misses/evictions）但**未接到任何观测出口**，见
  changes/`expose-store-cache-metrics`。
