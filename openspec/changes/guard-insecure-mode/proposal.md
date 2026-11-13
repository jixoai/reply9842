## Why

`-insecure` 当前是一个没有任何护栏的开关：可以对任意远端地址开启跳过证书校验，CLI 只在
help 文本里写「自签观察期」，GUI 的 flag 说明写「自签证书或回环调试」，但：

- 没有任何运行时警告（不会在日志里提一句「本次连接不校验证书」）；
- 没有限制它只能用于回环/私网地址；
- 没有在 stats 里标记「本次是 insecure 连接」，事后无法审计哪些流量是明文可解的风险面。

```
rg -n 'Insecure' --glob '!*_test.go'
internal/transport/tunnel.go:45/163/237/293   ← 只透传给 tls.Config
internal/relayclient/runner.go:61/283         ← 只透传
cmd/gui/main.go:172/173/204/232               ← 只是 flag 与字段
```

DEPLOY.md 已经写明风险（「生产别这么干——中间人可以解开全部流量」），但代码侧没有任何
机制让这个风险**在运行时可见**。对代理 LLM 流量的工具来说，静默的 MITM 风险面比普通
服务更大（请求里含完整会话上下文）。

## What Changes

- 启用 `-insecure` 时**必须**在客户端启动日志与 GUI 状态区打印显著警告（不是 debug 级）。
- GUI 的状态行/徽标在 insecure 模式下显示一个持续可见的标记（例如「未校验证书」）。
- stats 的 `transport` 字段或新增 `insecure: true`，使事后能筛出这些连接。
- 可选（建议但可分开做）：对**公网**地址启用 insecure 时要求额外的确认参数
  （如 `-insecure-allow-remote`），回环/私网地址则只需 `-insecure`。这样「自签观察期」
  的常见场景不受影响，而「对公网跳过校验」需要显式二次确认。

## Capabilities

### New Capabilities

无。

### Modified Capabilities

- `security`: 追加「insecure 模式必须运行时可见且可审计」需求。
- `client-gui` / `observability`: 追加 insecure 的状态展示与统计标记。

## Impact

- 代码：`internal/relayclient/runner.go`（启动警告）、`internal/transport/tunnel.go`
  （地址分级判定，若做二次确认）、`internal/stats/stats.go`（新字段）、
  `cmd/gui/view.go`（状态标记）。
- 兼容：不改变 `-insecure` 的语义，只是让它可见。若加公网二次确认，则是对既有脚本的
  破坏性变更（需要 `-insecure-allow-remote`）——建议先只做「可见」，二次确认单独一轮。
- 无新增依赖。
