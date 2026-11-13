## Why

隧道端口一旦对外开放，任何能连上的人都能借它访问上游 AI 网关；若上游 API key 配在服务端
环境里，等于白送凭证。同时字典缓存（`store`）是**全局共享**的——不同调用者的请求体会进同
一个 `hash → body` KV，彼此都能作为对方请求的压缩字典，构成跨用户信息侧信道。

依据：
- `cmd/server/main.go` 全文无任何调用方鉴权（`rg -n 'Authorization|白名单|tenant' cmd/server/main.go`
  只命中转发上游时的头处理，没有准入检查）。
- `internal/store/store.go` 的 `Store` 没有租户维度，`Put/Get` 只按内容哈希。
- `README.md` 已知限制第 6 条自己写明：「若将来共享字典，必须按租户隔离——混合公私数据的
  字典存在 CRIME/BREACH 类攻击面（RFC 9842 §9.2）」。当前实现即「共享字典」，未隔离。
- `internal/idem/key.go:10` 的安全约束注释要求「同 key 内幂等、跨 key 隔离」，
  但该约束只覆盖幂等缓存，没有覆盖字典缓存。

## What Changes

- 新增服务端准入：可选 `-auth-token-file`（每行一个 token）或 `-auth-token-hashes`，
  客户端在隧道帧里携带 token（`RequestHead` / `ResumeReq` 尾部追加，与
  `AcceptDownZstd` 同族的向后兼容模式）。未配置时保持**显式开放**并在启动日志里大声警告。
- 字典缓存（`store`）改为按 `callerKeyId` 分区：不同调用者的 body 不互相作为字典。
  `-cache-mb` 语义变为「每分区上限」或「全局限额 + 分区配额」，取其一并在文档写清。
- 未鉴权请求返回明确的错误帧（不是 404 也不是静默降级），客户端日志可读。
- 文档补一节「安全部署清单」：只暴露 TLS 端口、上游必须同机、必须配 token。

**BREAKING**（仅当配置了 token 时）：旧客户端不带 token 会被拒绝。

## Capabilities

### New Capabilities

无（在既有 `security` / `server` 能力上追加需求）。

### Modified Capabilities

- `security`: 追加「服务端准入必须可配置且默认显式告知」与「字典缓存按调用者隔离」两条需求。
- `server`: 追加「未鉴权请求的拒绝语义」需求。

## Impact

- 代码：`cmd/server/main.go`（准入检查点）、`internal/meta/frame.go`（帧尾追加 token 字段，
  沿用 `AcceptDownZstd` 的向后兼容尾字节模式）、`internal/store/store.go`（分区）、
  `internal/relayclient/runner.go`（客户端携带 token）、`cmd/cli` / `cmd/gui`（新参数与设置项）。
- 配置：新增服务端参数；`docker-compose.server.yml` 需要能挂载 token 文件。
- 文档：`README.md`（安全章节）、`DEPLOY.md`（安全部署清单）。
- 兼容性：默认不配 token 时行为不变（但启动日志警告），因此可灰度上线。
