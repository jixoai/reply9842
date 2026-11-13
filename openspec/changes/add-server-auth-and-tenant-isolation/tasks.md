# Tasks: add-server-auth-and-tenant-isolation

## 1. 服务端准入

- [ ] 1.1 `cmd/server` 新增 `-auth-token-file`（每行一个 token，权限 0600 校验）与
      `-auth-token-hashes`；未配置时启动日志打印**显著警告**（复用 `log.Printf` 的显式
      文案，不能用 debug 级）
- [ ] 1.2 `internal/meta`：`RequestHead` / `ResumeReq` 帧尾追加 token 摘要字段（沿用
      `AcceptDownZstd` 的尾字节向后兼容模式；旧端不发即视为空 token）
- [ ] 1.3 `cmd/server` 在 `serveOne` 的解码之后、`spool.Acquire` 之前做准入判定；
      失败返回专用错误帧（新增 `FrameUnauthorized` 或在 `FrameError` 里带可识别前缀），
      并记 `status=401` 到 stats
- [ ] 1.4 客户端 `relayclient.Config` 新增 `AuthToken`；CLI 加 `-auth-token`（支持
      `$ENV` 间接引用），GUI 设置页加一行密码型输入
- [ ] 1.5 单测：无 token 被拒、错误 token 被拒、正确 token 通过、旧格式帧（无 token 字段）
      在服务端未配 token 时正常通过

## 2. 字典缓存分区

- [ ] 2.1 决定并记录 `-cache-mb` 的新语义（建议：全局限额 + 每分区软配额 = 全局 / 活跃
      分区数，动态调整），写进 `openspec/specs/uplink-delta/spec.md` 与 README
- [ ] 2.2 `internal/store` 加 `tenant` 维度：`Put(tenant, body)` / `Get(tenant, hash)`；
      淘汰在全局维度做，命中率统计按租户可观测
- [ ] 2.3 客户端在请求里携带 `callerKeyId`（已有 `idem.CallerKeyID`），服务端用它作为
      store 分区键；未鉴权模式退化为单分区 `anon`
- [ ] 2.4 单测：`TestStoreTenantIsolation`（A 的 body 不能给 B 当字典）、
      `TestStoreGlobalBudget`（多数租户共享全局上限）

## 3. 验证与文档

- [ ] 3.1 端到端：本地起服务端（配 token）+ 客户端（错 token → 401；对 token → 正常），
      产出可粘贴的运行日志
- [ ] 3.2 `README.md` 新增「安全」章节；`DEPLOY.md` 新增「安全部署清单」
- [ ] 3.3 确认向后兼容：不配 token 时 `go test ./...` 与 e2e 全部仍绿

## 验收标准

- 配置了 token 的服务端，拒绝所有不带正确 token 的连接，且客户端日志能看出原因。
- 两个不同 `callerKeyId` 的调用者，无法把对方的 body 当字典（可用 `store` 单测与线上
  `base_hits` 对比证实）。
- 不配 token 时行为与改动前完全一致。
