# Tasks: fix-upstream-failure-surface

## 1. 服务端失败面

- [x] 1.1 `cmd/server/main.go` 新增 `failWithStatus`：头未产生时发
      `FrameResponse`(502/504) + JSON 错误体 + `FrameDone`
- [x] 1.2 `streamToClient` 的两条失败路径（上游未给头、120s 无头超时）改走
      `failWithStatus`
- [x] 1.3 错误体固定形状 `{"error":{"type":"upstream_error","message":...}}`

## 2. 上游候选列表

- [x] 2.1 新增 `cmd/server/upstream.go`：`parseUpstreams`（逗号分隔、启动即校验）
- [x] 2.2 `DialContext` 按序拨号，第一个连通的生效；忽略 Transport 传入的 addr
- [x] 2.3 默认值 `http://127.0.0.1:3425,http://127.0.0.1:3426`
- [x] 2.4 单测：解析（含非法输入）、dead→live 故障转移

## 3. 客户端空 200

- [x] 3.1 新增 `finalizeNoHead`：未发头且未完成时写 502 + 原因，并回填
      `stream.Status` / `stream.Err`（统计不再记成 200）
- [x] 3.2 单测：FrameError / 幂等键连续 miss / 无原因兜底 / 已发头不覆盖

## 4. spool 启动停顿

- [x] 4.1 `restoreIdem` 不再 eager `scanEventsLockedFile`
- [x] 4.2 `EventOffset` / `EventsFrom` 走 `ensureEventsIndex`（`sync.Once` 惰性重建）
- [x] 4.3 单测：恢复后索引为空、首次 EventOffset 补齐且偏移一致

## 5. 验证与发布

- [x] 5.1 `go build ./...` + `go test ./...` 全绿
- [x] 5.2 生产：单元文件改 `-upstream http://127.0.0.1:3425` 并重启，隧道端到端 200
- [x] 5.3 文档/规范同步 3425 与候选列表语义
- [x] 5.4 发布 v0.3.9；生产服务端换新二进制并改用候选列表参数
