## MODIFIED Requirements

### Requirement: 启动恢复不得全量扫盘

`spool.New` SHALL 在启动阶段对 idem 区**不做**任何逐条目工作：不 `ReadDir`、不
`Stat`、不 `OpenFile`、不读 `.meta`。启动成本必须与幂等条目数量无关（O(1)），
只允许 `MkdirAll` 与清空 tmp/ 活跃区。

冷条目按 key 惰性水合：`Acquire(key)` 未命中内存表时，按可推导的
`idem/<key[:16]>.dat` + 同名 `.meta` 直接读取（≤ 2 次文件打开）；命中则水合成
`Entry`（`state=Complete`、`inIdem=true`）；未命中则维持「新建 tmp 条目、由调用方
回 409 delta-miss」的既有语义。

索引与预算是后台工作：监听就绪之后由后台协程分批补水（填充热表、删除无 sidecar
的孤儿、按 mtime 收敛字节预算），可被 `Close` 取消。允许预算在补水完成前处于
软超状态。

索引缓存（`idem/index.ndjson`）只作为 hint：末尾半行忽略、与目录列表及文件实际
大小对账，不符即丢弃该条并退回冷查。索引错误只允许造成 409（多一次重传），
不得产出错误字节。

#### Scenario: 大幂等区重启

- **WHEN** idem 区有 2.2GB / 28k 条目，进程重启
- **THEN** 监听端口在 1 秒内可用，且启动阶段未打开任何 `.dat`

#### Scenario: 重启后按 key 续传

- **WHEN** 客户端带着重启前的 key 发起续传
- **THEN** 冷查在 ≤ 2 次文件打开内水合该条目，返回与重启前一致的偏移与响应头

#### Scenario: 重启后按 event id 续传

- **WHEN** 客户端带着重启前的 event id 发起续传
- **THEN** 服务端惰性重建索引并返回与重启前一致的偏移

#### Scenario: 冷查未命中

- **WHEN** 客户端用重启后已不存在（被淘汰）的 key 发起续传
- **THEN** 服务端回 409 delta-miss，客户端重发完整请求

#### Scenario: 索引损坏

- **WHEN** `idem/index.ndjson` 末尾是半行，或记录的 wire 与文件实际大小不符
- **THEN** 该条被忽略，不影响其余条目的水合
