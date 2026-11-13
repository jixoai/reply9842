## Why

README 与 DEPLOY.md 都把 QUIC 当作可用能力在讲（「QUIC 与 TCP/TLS 共用同一个端口」、
「两组都放通则 QUIC 可用，弱网下重连更快（0-RTT）」），但实现侧从未在生产启用：

- `README.md` 已知限制第 3 条自己写着：「**QUIC 生产未启用**。有回环端到端测试
  （`TestQUICRoundTrip`）通过，但生产链路仍走 TCP/TLS；启用需放通 UDP 端口并验证中间设备。」
- 提交 0194704 的正文明确记录了把默认值从 `auto` 改成 `tcp` 的**理由**：
  「默认 transport auto → tcp：QUIC 在该链路从未握手成功，auto 把失败暴露给用户」。
  而当前代码 `DefaultTransport = "auto"`（`internal/relayclient/runner.go:47`），
  说明该决定后来被回退，但**没有留下为什么回退、也没有补上验证结论**。
- DEPLOY.md 声称「0-RTT」，而代码没有任何 `Allow0RTT` 配置
  （`rg -n '0-RTT|Allow0RTT'` 只命中该文档本身）。QUIC 侧 `quic.Config` 只有
  HandshakeIdleTimeout / MaxIdleTimeout / KeepAlivePeriod。

也就是说：QUIC 代码完整、测试通过、文档在卖，但既没有生产验证，文档里的 0-RTT 能力
根本不存在。

## What Changes

- **二选一，必须给出结论**：
  (a) 在生产启用 QUIC：放通 UDP、验证中间设备、把验证结论写进 `deployment` spec 与
      DEPLOY.md；或
  (b) 明确不启用：把 README/DEPLOY 的措辞改成「QUIC 已实现但默认不启用」，并从
      「弱网 0-RTT」的卖点里删掉。
- 把「默认 transport 应该是 auto 还是 tcp」这个反复摇摆的决定**固化成一条有依据的
  Requirement**（当前代码是 auto，但没有任何测试或文档解释为什么推翻 0194704 的 tcp 决定）。
- 若启用：`quic.Config` 显式设置 `Allow0RTT` 并补 e2e；若不启用：删掉 0-RTT 措辞。
- 新增护栏测试：默认 transport 的值被断言（类似 `internal/relayclient/default_test.go` 已有的默认值护栏），
  防止再次无声摇摆。

## Capabilities

### New Capabilities

无。

### Modified Capabilities

- `transport`: 追加「QUIC 的生产启用状态必须有明确结论且与文档一致」需求；
  修正「应用层 auto 回退」条目里「记住成功模式」与默认值现状的关系。
- `deployment`: 追加「DEPLOY.md 的网络要求必须与实际配置一致」需求（含 0-RTT 措辞）。

## Impact

- 代码：`internal/relayclient/runner.go`（默认值，若改）、`internal/transport/tunnel.go`
  （`quic.Config`，若启用 0-RTT）、`internal/relayclient/default_test.go`（默认值护栏）。
- 文档：`README.md`（已知限制第 3 条）、`DEPLOY.md`（网络要求与 0-RTT 措辞）。
- 基础设施：若选 (a)，需要服务器放通 UDP 28443 并验证中间设备（跨境链路尤其要验）。
- 风险：若在跨境链路上启用 QUIC 失败，每次连接会多等一个 1 s 探测超时——这正是
  0194704 把默认值改成 tcp 的原因。改动前必须实测该链路的 QUIC 握手。
