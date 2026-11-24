## ADDED Requirements

### Requirement: 线路是一等配置对象，支持 direct / proxy / relay 三类混搭

客户端 SHALL 以「线路列表」而非单一出站配置表达网络出口。每条线路 MUST 至少包含
`id`（用户可读名）、`kind`（`direct`|`proxy`|`relay`）、`server`（目标 relay-server
地址，`host:port`）。可选字段：

| 字段 | 适用 kind | 语义 |
|---|---|---|
| `serverName` | 全部（**relay 指向 IP 时必填**） | TLS SNI。缺它则用 `server` 的 host 推导，IP 目标下必然 `x509: ... no IP SANs` |
| `localAddr` | `direct` / `relay` | 绑定出站源地址以绕开 TUN。relay 同样需要：中继只是服务端再转发一跳，**本机到中继这一跳仍是本机外连** |
| `proxy` | `proxy` | 合法 URL（`http`/`https`/`socks5`）。proxy 线路 MUST NOT 填 `localAddr`（CONNECT 由代理进程发起） |
| `transport` | 全部 | `auto`\|`quic`\|`tcp`；proxy 线路 MUST NOT 为 `quic` |
| `enabled` | 全部 | 缺省视为启用 |

`kind=relay` 与 `direct` 使用同一套建连代码路径，差别是 `server` 指向**另一台主机**——
因此它额外需要正确的 `serverName`（证书通常只签域名，不签中继机的 IP）。

列表长度 MUST 支持 1..N（N≥3 必须有测试覆盖），顺序由用户决定并持久化。

落地证据：`internal/route/route.go`（类型、Normalize、Validate 与 kind 交叉校验）、
`internal/transport/pools.go`（`DialerSpec` → 每线路一个独立 `PooledDialer`，
粘性 prefer 状态不跨线路共享）。

#### Scenario: 三类线路混搭共存

- **WHEN** 用户配置了 `{kind:direct}` + `{kind:proxy, proxy:socks5://...}` + `{kind:relay, server:other-host:28443}`
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

落地证据：`internal/route/estimate.go` 的 `Model` 在最近 3–24 条样本上做两参数
OLS，得 `a`（固定开销 ms）与 `bw`（KB/s）；`Estimate(bytes) = a + bytes/bw`。
样本来源是既有的 `MsWire`（隧道往返）与 `UpWire`/`DownWire`，新增 `Route` 维度后
按线路分别拟合。择路的**输入体积**是 `estimateWireBytes()`——用 `basefind`
预测的 delta 复用率换算**隧道实际传输量**，不是明文 body 长度（否则 delta 命中时
会把 1.5 MB 估成 1.5 MB，实际只传 129 B，差 4 个数量级）。

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

落地证据：`internal/route/pick.go` 的 `Picker` —— 惩罚乘子
`1 + 0.5 × EffectiveFails()`，`failCap = 32`，失败间隔超过上限后不再加；
`Model.EffectiveFails()` 用 `fails × 0.5^(elapsed/½life)`，`½life = 2 min`，
所以惩罚会自然衰减、线路自动回升。探索通道按 `30s × 2^probeFailures`
（封顶 10 min）放行未知/刚恢复的线路。`SelectExcluding` 保证**一次请求内
每条线路最多试一次**——否则从未成功过的线路样本数恒为 0，会反复被
「主动探测」选中，同一次请求反复打同一条死线路。

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
成功率、本次运行内被选中承载的**累计次数**、是否在用。这些指标 MUST 同时出现在 per-request 日志
（`client-*.jsonl`）与汇总统计中，并以 `route` 字段区分。

禁止只在内存里维护评分而不落盘：用户排障时必须能从日志回溯「当时为什么选了这条」。

#### Scenario: 日志能回答「当时为什么选这条」

- **WHEN** 查看任一条请求日志
- **THEN** 记录中包含该请求实际使用的 `route` 标识，以及该次传输的 wire 字节与耗时

#### Scenario: 面板显示实时质量

- **WHEN** 打开 GUI 设置页
- **THEN** 每条线路显示其预计耗时 / 带宽 / 成功率 / 使用状态，且随请求实时更新

### Requirement: 线路字段必须可在 GUI 里完整配置

用户 SHALL 能在 GUI 内完成任意一条线路的全部必填字段配置，不得要求手改
`settings.json` 或用 CLI 才能让一条线路变得可用。

理由（实现后自查发现的死路）：中继线路的 `server_name`（证书域名）与
`local_addr`（源地址）都是必填——缺 SNI 会 `x509: cannot validate
certificate for <ip>`，缺源地址在 TUN 全局代理下会 `tcp tls: EOF`。
若 GUI 只提供增删启停，用户新增中继线路后没有任何途径填这两项，等于
「加了就废」。

布局约束（经两轮视觉验收后定稿）：

- 同一时刻 MUST 最多展开一条线路的字段区。
- 列表本身在固定高度的内滚动容器内（`MaxHeight(4×41)`），保证展开列表不会把
  后续设置项无限推下去。
- 字段区 MUST 渲染在**该滚动容器之外**（列表下方），MUST NOT 放进容器内部。
  实测教训：放进容器内时，在最后一行展开会被视口底边横向切断——第一个输入框
  只剩半个（缺底边框、字形少 2px），用户看不到完整字段，也判断不出还有几项。
- 字段区 MUST 有明确的归属表达（「正在编辑「名称」」标题 + 完成按钮），
  MUST NOT 只靠行内按钮从「编辑」变「收起」来暗示归属。
- 字段 MUST 与页面其它表单项同列（标签 84px + 输入框撑满）。

#### Scenario: 新增中继线路并使其可用

- **WHEN** 用户在设置页点「＋中继」，再点该行的「编辑」
- **THEN** 出现服务器 / 证书域名 / 源地址三个输入框
- **AND** 填完后该线路通过 `route.Validate`，无需编辑任何文件

#### Scenario: 运行中不可改

- **WHEN** 线路正在承载请求（Runner 运行中）
- **THEN** 字段输入框置灰，并显示「运行中不可编辑，停止后可改」

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
