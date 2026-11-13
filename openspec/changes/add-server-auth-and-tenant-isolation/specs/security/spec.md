## ADDED Requirements

### Requirement: 服务端准入可配置且默认显式告知

服务端 SHALL 支持可选的隧道准入：`-auth-token-file`（每行一个 token）或
`-auth-token-hashes`（每行一个 token 的 SHA-256）。未配置准入时服务端**必须**在启动
日志里打印显著警告说明「本实例无准入控制」，不得静默开放。

准入判定 SHALL 发生在解码之后、`spool.Acquire` 之前——未鉴权请求不得占用 spool 预算，
也不得触发上游请求。

#### Scenario: 配置了 token 后拒绝无效连接

- **WHEN** 服务端配置了 token，客户端不带 token 或带错误 token
- **THEN** 服务端返回可识别的未授权错误帧（客户端日志能看出原因），不转发上游、
  不写 spool、stats 里记为 401

#### Scenario: 未配置 token 时行为不变

- **WHEN** 服务端未配置任何准入参数
- **THEN** 所有连接照常服务（与改动前一致），但启动日志明确警告「本实例无准入控制」

#### Scenario: 旧客户端在未配置 token 的服务端上照常工作

- **WHEN** 客户端是不带 token 字段的旧版本，服务端未配置 token
- **THEN** 请求正常完成（尾字段缺失视为空 token，不报解析错）

### Requirement: 字典缓存按调用者隔离

服务端字典缓存（`store`）SHALL 按 `callerKeyId` 分区：某个调用者上传的 body 不得成为
另一个调用者请求的压缩字典。全局字节上限（`-cache-mb`）语义必须明确写在文档里——
要么「每分区上限」，要么「全局限额 + 分区软配额」，不允许含糊。

依据：`README.md` 已知限制第 6 条明确「若将来共享字典，必须按租户隔离——混合公私数据的
字典存在 CRIME/BREACH 类攻击面（RFC 9842 §9.2）」。当前实现即共享字典。

#### Scenario: 跨调用者不能互当字典

- **WHEN** 调用者 A 上传过 body X，调用者 B 以 X 的哈希发起 delta 请求
- **THEN** 服务端返回 `FrameDeltaMiss`，绝不使用 A 的 body 解码 B 的请求

#### Scenario: 全局预算仍被遵守

- **WHEN** 多个调用者各自上传大量 body
- **THEN** 字典缓存总占用不超过 `-cache-mb` 声明的口径，淘汰在全局维度生效

#### Scenario: 未鉴权模式退化为单分区

- **WHEN** 服务端未配置准入且客户端不带凭据
- **THEN** 所有请求归入 `anon` 分区，行为等价于改动前的全局缓存
