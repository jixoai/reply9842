# client-cli Specification

## Purpose

命令行客户端 `relay-cli`：在 `127.0.0.1` 起一个本地 HTTP 入口，把请求经增量压缩隧道
送到 relay-server，再把响应流式写回。只有一份转发逻辑（`internal/relayclient`），CLI 与
GUI 共用，避免「GUI 能用、CLI 不能」这类裂缝。

## Requirements

### Requirement: 三子命令且无子命令即 serve

CLI SHALL 支持 `serve` / `status` / `version` / `help` 四个子命令，且「第一个参数不是
已知子命令」时按 `serve` 处理——旧写法 `relay-cli -listen ...` 必须继续可用。
未知子命令退出码为 2。

落地证据：`cmd/cli/main.go:31`（`main` 的子命令嗅探）、
`cmd/cli/main.go:99`（`runServe`）、`cmd/cli/main.go:128`（`runStatus`）、
`cmd/cli/main.go:62`（`usage`）。

#### Scenario: 老脚本无需改动

- **WHEN** 用户执行 `relay-cli -listen 127.0.0.1:18899 -server host:28443`
- **THEN** 按 serve 处理并正常启动

#### Scenario: 未知子命令报错退出

- **WHEN** 用户执行 `relay-cli foo`
- **THEN** 打印「未知子命令」与用法，退出码 2

### Requirement: serve 参数与默认值集中在 relayclient

serve 的 flag SHALL 全部映射到 `relayclient.Config`，默认值来自 `relayclient` 包内的
常量（CLI flag 默认值与 GUI 输入框初值同源，避免「文档写的默认」与「代码跑的默认」漂移）。

参数集合：`-listen`(127.0.0.1:18899) `-server`(47.82.76.102:28443) `-server-name`
`-local-addr` `-transport`(auto) `-insecure` `-min-delta`(16384) `-history-mb`(64)
`-capture-dir` `-capture-n`(30) `-max-resume`(3) `-log` `-stats`。

落地证据：`cmd/cli/main.go:79`（`newServeFlags`）、
`internal/relayclient/runner.go:41`（默认值常量块）、
`internal/relayclient/runner.go:75`（`Config`）、
`internal/relayclient/runner.go:74`（`Normalize`）。

#### Scenario: 默认值可直接连内置服务端

- **WHEN** 不带任何参数启动
- **THEN** `-server` 与 `-server-name` 组成可用的默认组合（由
  `TestDefaultsAreConnectable` 钉住）

#### Scenario: GUI 与 CLI 默认值不漂移

- **WHEN** 任一默认值被改动
- **THEN** CLI flag 与 GUI 初值同时变化（二者都读同一个常量）

### Requirement: status 从落盘快照跨进程读取

`status` SHALL 读 `client-stats.json`（可用 `-stats` 指定）并打印人眼友好摘要，
`-json` 原样输出 JSON 供脚本消费。不得尝试连进程内 stats——跨进程唯一通道就是文件
（GUI 才是进程内快照）。

落地证据：`cmd/cli/main.go:128`（`runStatus` 及「为什么不连进程内 stats」的注释）、
`internal/stats/stats.go:746`（`ReadAgg`）、
`internal/relayclient/runner.go:134`（`DefaultStatsPath`）。

#### Scenario: 还没有数据时给人话提示

- **WHEN** 统计文件不存在
- **THEN** 打印「还没有统计数据」并说明先跑 serve，退出码 0（不是错误）

#### Scenario: 脚本消费 JSON

- **WHEN** `relay-cli status -json | jq .up_ratio`
- **THEN** 输出合法 JSON，含 `up_ratio` 字段

### Requirement: 运行时数据目录固定

客户端运行时数据 SHALL 落在 `~/.reply9842/`：逐请求日志 `client-<date>.jsonl`（按日
滚动）与汇总快照 `client-stats.json`。改名后的目录名是破坏性变更的一部分（旧
`~/.rfc9842-reply` 不兼容、不迁移）。

落地证据：`internal/relayclient/runner.go:120`（`DefaultDir`）、
`internal/relayclient/runner.go:129`（`DefaultLogPath`，含日期滚动）、
`internal/relayclient/runner.go:134`（`DefaultStatsPath`）。

#### Scenario: 跨日自动换文件

- **WHEN** 进程跨过午夜继续运行
- **THEN** 新请求写入新日期的 jsonl，旧文件保留

### Requirement: 采样落盘用于离线分析

CLI SHALL 支持 `-capture-dir` 把请求体原样落盘（按序号命名 `req-%04d.bin`），
`-capture-n` 限制条数，避免无限增长。

落地证据：`internal/relayclient/runner.go:487`（`handler.capture`）、
`internal/relayclient/runner.go:522`（`ServeHTTP` 里的采样调用点）。

#### Scenario: 只采前 N 条

- **WHEN** `-capture-dir` 已设置且已采满 `-capture-n` 条
- **THEN** 后续请求不再落盘，目录不继续膨胀

### Requirement: 优雅停止

serve SHALL 在收到 `SIGINT`/`SIGTERM` 后优雅停止：以 8 秒超时的 ctx 调用
`Runner.Stop`，把统计落盘后退出。

落地证据：`cmd/cli/main.go:99`（`runServe` 的信号处理与 8s 超时）、
`internal/relayclient/runner.go:334`（`Runner.Stop`）。

#### Scenario: Ctrl-C 不丢统计

- **WHEN** 用户按 Ctrl-C
- **THEN** 统计快照被写入磁盘，`relay-cli status` 仍能读到本次观测

## 已知边界

- 端口被占用时直接报 `bind: address already in use`（`Runner.Start` 先 bind 再返回，
  因此返回 nil 意味着端口真实可用）。INSTALL.md 的故障排查给出了 `lsof` 排查步骤。
