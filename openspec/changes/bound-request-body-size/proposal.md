## Why

客户端和服务端都会把整个请求/响应体读进内存：

```
internal/relayclient/runner.go:503   body, err := io.ReadAll(r.Body)   // 客户端，无上限
cmd/server/main.go 的 serveOne       // 服务端解出的 body 同样驻留内存
```

当前唯一的体积护栏是帧级 `256 MB`（`internal/transport/tunnel.go:418` 附近的
`n > 256<<20` 拒绝），但那是**读取帧之后**的上界检查，且作用在 wire 字节上（压缩后）。
一个 200 MB 的请求体（identity 编码时等于 wire）会被完整读进内存再做 delta/L19 压缩，
按 `PickLevel` 的注释「30 MB 级请求的 delta 需要显著内存，41 MB 文档用 L19 需 34.5 s /
328 MB RSS」外推，200 MB 会把进程推到 OOM 边缘。

README 已知限制第 5 条自己承认这点：「单请求体全量缓冲。客户端与服务端都会把 body 读进
内存（为统计与压缩）」，但没有给出任何上限或背压。

生产上的现实风险：Agent 上下文天然会长（推理型模型单请求可达数 MB），一旦某次请求异常
膨胀（例如误把大文件塞进 prompt），就是客户端 OOM。

## What Changes

- 客户端新增 `-max-body-mb`（默认取一个明确值，建议 64），超过则**不报错而是降级**：
  跳过 delta 与压缩，直接 identity 转发（保持功能可用，只是不省带宽），并在 stats 里打
  `enc=identity` + `err=body_too_large_for_delta` 便于观测原因。
- 服务端新增同名参数，超过则对该请求返回明确错误帧（服务端不能选择「慢慢传」，它的
  上游请求也要构造完整 body）。
- 两侧都在读 body 前用 `http.MaxBytesReader` / 长度预检，避免先把内存吃掉再判断。
- 补测试：超限请求的降级行为、不超限请求不受影响。

## Capabilities

### New Capabilities

无。

### Modified Capabilities

- `uplink-delta`: 追加「超大请求体的显式上限与降级策略」需求。
- `security`: 追加「请求体必须有过大保护，不得无界读入内存」需求。
- `server` / `client-cli`: 追加新参数。

## Impact

- 代码：`internal/relayclient`（读 body 前的预检与降级）、`cmd/server/main.go`（预检与
  错误帧）、`cmd/cli` 与 `cmd/gui`（新参数/设置项）。
- 行为变化：超限请求从「可能 OOM」变为「可预测地不压缩但成功」。这是一个语义上的
  放宽（原先是隐式的、可能失败的路径）。
- 文档：README 已知限制第 5 条改写为「有显式上限，超限降级为 identity」。
