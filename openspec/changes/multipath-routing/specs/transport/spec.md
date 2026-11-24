## ADDED Requirements

### Requirement: 线路是一等配置对象，支持 direct / proxy / relay 三类混搭

客户端 SHALL 以「线路列表」而非单一出站配置表达网络出口。每条线路 MUST 至少包含
`id`（用户可读名）、`kind`（`direct`|`proxy`|`relay`）、`server`（目标 relay-server 地址）。
`kind=proxy` 时 MUST 提供合法的 `proxy` URL（`http`/`https`/`socks5`）；
`kind=direct` MAY 提供 `localAddr`（绑定出站源地址以绕开 TUN）；
`kind=relay` 的语义与 `direct` 相同，仅 `server` 指向不同主机。

列表长度 MUST 支持 1..N（N≥3 必须有测试覆盖），顺序由用户决定并持久化。

落地证据（现状）：`internal/relayclient/settings.go` 的 `NetworkMode` 只允许三选一、
`ProxyURL` 是单串；`internal/relayclient/runner.go:344` 在 `Start()` 里构造**单个**
`transport.Dialer`；`internal/transport/pool.go` 只有单池。本 Requirement 要求把这些
单值形态替换为列表形态。

#### Scenario: 三类线路混搭共存

- **WHEN** 用户配置了 ``{kind:direct}`` + ``{kind:proxy, proxy:socks5://...}`` + ``{kind:relay, server:other-host:28443}``
- **THEN** 三条线路都被客户端接受并各自建立可用连接，任一线路的配置错误只影响它自己

#### Scenario: 代理线路不得声明 QUIC

- **WHEN** 某条 `kind=proxy` 线路的 `transport` 为 `quic`
- **THEN** 该线路在建连前即报出可读错误（沿用 `transport.Dialer.Dial` 现有的
  「代理模式不支持 QUIC」路径），不得白等一次握手超时

#### Scenario: 单条线路等价于旧行为

- **WHEN** 线路列表只有一条且 kind=direct、server 为默认值
- **THEN** 建连参数与今天的 `Config{Server, LocalAddr, Transport}` 完全等价

### Requirement: 择路以「预计完成时间」为评分，不以 RTT 为评分

每条线路 SHALL 维护一个**吞吐模型**，用真实请求的 `(wire_bytes, 传输耗时)` 样本
拟合「固定开销 + 字节/带宽」两参数模型。选路时 MUST 用**本次请求的预期字节数**
计算每条线路的预计完成时间，并选择最小者。

禁止以 RTT / TTFB / 握手耗时作为主评分：owner 2026-11-24 明确「延迟低但带宽窄的
线路，最终传输完成时间可能反而更长」。RTT 类指标 MAY 参与固定开销 `a` 的估计，
但 MUST NOT 单独成为排序依据。

落地证据（现状）：`internal/stats/stats.go:60` 已有 `MsWire`（隧道往返）与
`UpWire`/`DownWire`（隧道实际字节），本变更复用这些既有字段作为样本来源，
新增 `Route` 维度。

#### Scenario: 低延迟窄带宽输给高延迟宽带宽

- **WHEN** 线路 A 的 RTT 为 50 ms 但实测带宽 200 KB/s，线路 B 的 RTT 为 200 ms 但带宽 4 MB/s，本次请求 wire 体积 2 MB
- **THEN** 预计完成时间 A≈10.2 s、B≈0.7 s，必须选 B

#### Scenario: 小请求时低延迟线路胜出

- **WHEN** 同样两条线路，但本次请求 wire 体积只有 16 KB
- **THEN** A≈0.13 s、B≈0.20 s，必须选 A

#### Scenario: 样本不足时不臆断

- **WHEN** 某条线路还没有足够的实测样本（新加入或刚恢复）
- **THEN** 该线路按「未知」处理并赋予探索优先权（主动放行一部分请求去采集样本），
  不得因为缺样本而永久冷落

### Requirement: 线路劣化时自动降权，但永不彻底排除

线路评分 SHALL 由两部分构成：
（a）吞吐模型——在最近 N 条样本上做两参数最小二乘拟合（滚动窗口，非 EWMA）；
（b）失败惩罚——`est × (1 + k × 有效连续失败次数)`，其中「有效失败次数」
按固定半衰期做**时间衰减**（不是原始计数）。
失败惩罚 MUST 有上限，且 MUST 随时间衰减——否则惩罚封顶的线路永远拿不到
「成功一次」的机会，效果等同于硬熔断。
任何时刻 MUST 保证至少一条线路被选为候选——所有线路都劣化时，
选择「当前评分最不差」的一条继续尝试。

禁止硬熔断（把失败线路从候选集合彻底移除直至外部健康检查恢复）：
owner 2026-11-24 裁决「三条线同时瞬断 → 全踢光 → 无路可用」是不可接受的风险。

落地证据（现状）：今天没有任何降权逻辑；单池在连接失败时只做「丢弃并重连」
（`internal/relayclient/runner.go:599`），没有跨线路视角。

#### Scenario: 单条线路持续失败不导致断流

- **WHEN** 线路 A 100% 失败、线路 B 正常
- **THEN** 流量持续由 B 承载，A 保留在候选集合中并可能被探测性尝试

#### Scenario: 全部线路同时劣化仍能发起请求

- **WHEN** 所有线路近期都失败
- **THEN** 仍会选出一条（评分最不差者）发起请求；请求失败后按既有重试逻辑处理，
  不得出现「无线路可选」这一状态

#### Scenario: 恢复后自动回升

- **WHEN** 线路 A 从持续失败中恢复并成功若干次
- **THEN** 其权重在有限次成功内回到与其他线路可比的水平，不需要重启客户端

### Requirement: delta 基线跨线路共享或显式降级，不得静默损坏

协议事实（已核实）：`internal/store` 是全局 `hash→body` KV，`internal/idem.Key`
的作用域是 caller key 而非连接，**delta 基线是 server 侧状态**。

因此：
- 指向**同一 server** 的多条线路，切换 MUST 对 delta 完全无损。
- 指向**不同 server** 的线路之间切换，MUST 允许发生 `FrameDeltaMiss` 并由客户端
  按既有路径重发全量；MUST NOT 出现数据损坏或请求丢失。
- 客户端 MUST 把「线路所属 server」暴露给用户（GUI 标注），让「跨 server 切换
  会偶发一次全量」这一代价可见。

落地证据：`internal/store/store.go`（`Put`/`Get` 以 `codec.Hash` 为键，无 tenant 维度）；
`internal/idem/idem.go:31`（`Key(callerKeyID, req, body)`）；`internal/meta/frame.go:33`
（`FrameDeltaMiss` 的存在即为此类失配的优雅回退）。

#### Scenario: 同 server 多线路无 delta 损耗

- **WHEN** 三条线路都指向同一 server，请求在它们之间轮转
- **THEN** 编码分布与单线路时无显著差异（`dcz`/`dcz2` 占比不下降）

#### Scenario: 跨 server 切换是可恢复的

- **WHEN** 择路把请求从 server A 的线路切到 server B 的线路，且 B 没有该基线
- **THEN** 客户端收到 `FrameDeltaMiss` 后重发全量并成功；请求内容与服务器响应均正确

### Requirement: 线路质量必须可观测

每条线路 SHALL 暴露可观测的质量指标，至少包含：预计完成时间、实测有效带宽、
成功率、当前承载的请求量、是否在用。这些指标 MUST 同时出现在 per-request 日志
（`client-*.jsonl`）与汇总统计中，并以 `route` 字段区分。

禁止只在内存里维护评分而不落盘：用户排障时必须能从日志回溯「当时为什么选了这条」。

#### Scenario: 日志能回答「当时为什么选这条」

- **WHEN** 查看任一条请求日志
- **THEN** 记录中包含该请求实际使用的 `route` 标识，以及该次传输的 wire 字节与耗时

#### Scenario: 面板显示实时质量

- **WHEN** 打开 GUI 设置页
- **THEN** 每条线路显示其预计耗时 / 带宽 / 成功率 / 使用状态，且随请求实时更新

### Requirement: 旧配置必须无损迁移

读取到不含线路列表的旧 `settings.json`（只有 `server` + `network_mode` + `proxy_url`）时，
客户端 SHALL 自动合成等价的一条线路，不得丢弃用户已有设置。
迁移 MUST 幂等：多次读写不产生漂移。

落地证据：`internal/relayclient/settings.go` 的 `NetworkModeOr()` 已有「空值按旧
`LocalAddr` 迁移」的先例，本 Requirement 沿用同一思路扩展。

#### Scenario: 现有生产配置自动还原

- **WHEN** 用当前生产配置（`network_mode:direct`、`server:magpie.aiweb.xin`、`proxy_url` 空）启动
- **THEN** 得到一条 `kind=direct`、`server=magpie.aiweb.xin` 的线路，连通性与今天一致

#### Scenario: 配过代理的旧配置不丢代理

- **WHEN** 旧配置为 `network_mode:proxy` + 非空 `proxy_url`
- **THEN** 迁移出一条 `kind=proxy` 线路，代理 URL 原样保留
