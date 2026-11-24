# transport Specification

## Purpose

隧道传输层：QUIC 与 TCP+TLS 在同一端口双栈监听，客户端支持应用层 auto 回退，
并支持绑定出站源地址以绕过代理 TUN 劫持。帧格式自定界，两种传输共用同一套编码。
客户端支持多条线路（直连 / 代理 / 中继）同时可用，按「预计完成时间」择路。

## Requirements

### Requirement: QUIC 与 TCP+TLS 同端口双栈

服务端 SHALL 在同一个 `-listen` 地址上同时启动 QUIC（UDP）与 TCP+TLS 监听，
把两类连接统一成 `Accept{Kind, Conn}` 送进同一条 accept 通道。ALPN 一律为
`reply9842/1`（新旧客户端不兼容是改名后的刻意结果）。

落地证据：`internal/transport/tunnel.go:307`（`Server.Listen`，同时起 QUIC
listener 与 `tls.Listen`）、`internal/transport/tunnel.go:39`（`ALPN`）、
`internal/transport/tunnel_test.go:25` 与 `:85`（`TestTCPRoundTrip` / `TestQUICRoundTrip`）。

#### Scenario: 只放通 TCP 也能工作

- **WHEN** 服务端 UDP 端口被防火墙拦掉
- **THEN** TCP/TLS 路径照常服务，客户端 auto 模式降级成功

#### Scenario: 一条 QUIC 连接承载多请求

- **WHEN** 客户端在一条 QUIC 连接上先后打开多个 stream
- **THEN** 每个 stream 独立处理一次请求，某条连接退出不影响其它连接

### Requirement: 应用层 auto 回退

客户端 SHALL 在 `-transport auto` 下先试 QUIC（探测超时 `QuicProbe`，默认 1 s），
失败降级 TCP+TLS，并**记住成功的那一种**用于后续连接优先级。回退必须实现在应用层：
HTTP/3 的自动回退是 HTTP 协议族能力（Alt-Svc），不是 QUIC 传输层自带。

落地证据：`internal/transport/tunnel.go:101`（`Dial`）、
`internal/transport/tunnel.go:159`（`dialQUIC`）、
`internal/transport/tunnel.go:233`（`dialTCP`）、
`internal/transport/tunnel.go:47`（`QuicProbe` 字段）、
`internal/transport/tunnel.go:61`（粘性记忆注释）。

#### Scenario: QUIC 握手失败后 TCP 接管

- **WHEN** 链路上 QUIC 从未握手成功（该情况在生产链路上实测过）
- **THEN** 同一次请求在探测超时后自动改走 TCP+TLS 并成功，用户不感知

#### Scenario: 记住成功模式

- **WHEN** 某次连接以 TCP 成功
- **THEN** 后续连接优先走 TCP，不再每次都等一个 QUIC 探测超时

### Requirement: 帧必须自定界

隧道帧 SHALL 为「4 字节大端长度 + payload」，读取必须用 `io.ReadFull`。禁止手写
「循环 Read，遇 err 立即返回」——`io.Reader` 允许同时返回数据与 `io.EOF`，QUIC 的
stream 在最后一帧正是 `(n>0, io.EOF)`，手写循环会丢掉这一帧（实测为「服务端 write
成功、客户端读到 EOF」的根因）。

落地证据：`internal/transport/tunnel.go:404-412`（`readFrame` 及 io.ReadFull 的理由）、
`internal/transport/tunnel_test.go:178`（`TestReadFrameEOFWithData`）、
`internal/transport/tunnel_test.go:211`（`TestWriteAllPartialWrites`）。

#### Scenario: 末帧带 EOF 也不丢

- **WHEN** 对端写完最后一帧立即 FIN
- **THEN** 该帧被完整读出，不会因为同时返回的 `io.EOF` 被丢弃

#### Scenario: 半包写入被补齐

- **WHEN** 底层连接一次只写入了部分字节
- **THEN** `writeAll` 继续写直到写完，帧边界不被打断

### Requirement: 支持绑定出站源地址

客户端 SHALL 支持 `-local-addr`（CLI / GUI 设置页「源地址」）绑定出站源地址：
TCP 走 `net.Dialer.LocalAddr`，QUIC 走 `net.ListenUDP` + `quic.Dial`（`DialAddr`
不允许指定源地址）。只给 IP 时自动补 `:0`；无效地址必须**明确报错**，不得静默回退
（静默回退会让用户以为绑定生效，实际仍在走 TUN）。

落地证据：`internal/transport/tunnel.go:275`（`localAddr` 归一化）、
`internal/transport/tunnel.go:233`（TCP 路径的 `LocalAddr`）、
`internal/transport/tunnel.go:159`（QUIC 路径的 `ListenUDP`）、
`internal/transport/localaddr_test.go:104`（`TestDialLocalAddr`）、
`:152`（bare IP）、`:173`（无效地址报错）、
`cmd/gui/view.go:665`（GUI「源地址」字段）。

#### Scenario: 代理 TUN 全局模式下仍能连通

- **WHEN** 本机代理把 `0.0.0.0/1` + `128.0.0.0/1` 全部导进 utun，隧道流量被劫持
- **THEN** 填物理网卡 IP 后连接立刻打通（生产实测：ALPN 协商成功、真实请求 200）

#### Scenario: 无效地址显式失败

- **WHEN** `-local-addr` 填写了不存在的地址
- **THEN** 启动即报错并说明，不静默降级为「让内核选」

### Requirement: TLS SNI 可显式指定，IP 直连必须给

客户端 SHALL 支持 `-server-name` 指定 TLS SNI。用 IP 直连时必须显式给出证书上的域名：
服务端证书 SAN 通常只有 DNS 没有 IP，留空会按 host（那个 IP）推导 SNI，必然
`x509: cannot validate certificate ... doesn't contain any IP SANs`。
内置默认组合（`-server` 默认 IP）由 `Config.Normalize` 自动补 `DefaultServerName`；
用户自定义 `-server` 时**不得**强塞内置 SNI。

落地证据：`internal/relayclient/runner.go:44`（`DefaultServerName`）、
`internal/relayclient/runner.go:84`（`Normalize` 的补值条件）、
`internal/transport/tunnel.go:262`（`serverName()` 的推导）、
`internal/relayclient/default_test.go`（两个回归护栏
`TestDefaultsAreConnectable` / `TestCustomServerKeepsEmptyServerName`）。

#### Scenario: 内置默认组合可直接连

- **WHEN** 用户不做任何配置直接启动客户端
- **THEN** SNI 已被补成证书域名，不会因 IP SAN 缺失而握手失败

#### Scenario: 自定义上游不被污染

- **WHEN** 用户把 `-server` 改成别家的 `example.com:28443` 且不给 SNI
- **THEN** 不自动填内置域名，交给 transport 按 host 推导

### Requirement: TLS 版本下界

实现 SHALL 对 TCP/TLS 与服务端监听使用 `MinVersion: tls.VersionTLS12`，QUIC 使用
`VersionTLS13`（QUIC 强制要求 TLS 1.3）。

落地证据：`internal/transport/tunnel.go:238`（TCP 客户端）、
`internal/transport/tunnel.go:315`（服务端）、
`internal/transport/tunnel.go:164`（QUIC 客户端）。

#### Scenario: 拒绝降级到 TLS 1.1

- **WHEN** 对端只支持 TLS 1.1 及更早
- **THEN** 握手失败，不降级

### Requirement: 连接复用（串行池）

客户端 SHALL 复用隧道连接而不是每请求新建。服务端 SHALL 支持在一条连接上连续处理
多个请求（帧自带长度前缀，串行处理故无串台风险）。

动机是实测数据：本机→香港单次 TCP+TLS 握手 747/2330/2821/773/3188 ms（均值约 2 s），
而原实现每请求都新建连接、用完即关，等于每个请求白扔 1–3 秒。这比隧道省下的带宽值钱。

选**串行复用**而非多路复用：多路复用要引入请求 ID、乱序响应、流量控制，改动面覆盖
frame / clientstream / 服务端 accept 循环，出错就是数据串台；而串行复用已吃掉绝大部分
收益，并发时退化为「各占一条连接」不比原实现差。

落地证据：`internal/transport/pool.go`（`PooledDialer`：LIFO 取用、90 s 超龄淘汰、
maxIdle=4）、`cmd/server/main.go`（`serve` 循环 + `serveOne` 返回 keepAlive）、
`internal/relayclient/runner.go`（Get/Put 时机）、`internal/transport/pool_test.go`。

#### Scenario: 响应未读完不能归还

- **WHEN** `Drain` 中途出错，连接里可能还有未读帧
- **THEN** 丢弃连接（`Drop`）而不是归还——否则下一个请求会读到上一个的残留数据

#### Scenario: 串台

- **WHEN** 同一连接上先后跑长度不同的多个请求
- **THEN** 每次读到的都是自己那条响应（`TestPoolNoCrossTalk` 用 4 种长度验证）

#### Scenario: 续传不复用

- **WHEN** 走 `FrameResume` 续传
- **THEN** 该连接不归还池——续传本就发生在异常之后，多条提前 return 的失败路径可能留半截帧

#### Scenario: 超龄连接

- **WHEN** 空闲连接超过 90 s
- **THEN** 取用时丢弃并新建，避免复用一条对端早已关闭的连接

### Requirement: 握手耗时必须可观测

每条连接 SHALL 记录建立耗时，且只有本次真的新建连接才计入统计（复用命中为 0）。
该值进入逐请求记录的 `ms_handshake` 与时间桶的握手计数，让「连接池是否真的生效」
可被观测，而不是靠一次性测试。

落地证据：`internal/transport/tunnel.go`（`Conn.Handshake()` / `Conn.Reused()`）、
`internal/stats/stats.go`（`Record.MsHandshake`、`Bucket.HsSum/HsCount`）、
`internal/relayclient/runner.go`（仅在 `!conn.Reused()` 时填）。

#### Scenario: 复用生效

- **WHEN** 连续请求命中连接池
- **THEN** `ms_handshake` 为 0，界面摘要显示「全程复用连接」

#### Scenario: 池未命中

- **WHEN** 每个请求都非零握手耗时
- **THEN** 说明池没命中，每个请求仍在付约 2 秒的握手成本

### Requirement: ALPN 双版本协商

传输层 SHALL 支持两个 ALPN：`reply9842/1`（老客户端，私有 dcz 上行编码）与
`reply9842/2`（RFC 9842 对齐编码 dcz2）。服务端按 `Dcz2` 开关决定 accept 列表；
客户端 SHALL 同时 offer 两个版本（顺序仅表达偏好，选择权在服务端）。

依据：`crypto/tls` 的 `negotiateALPN` 按服务端偏好顺序取第一个交集，**无交集时握手失败**
（不是降级）。因此只 offer `/2` 的客户端对老服务端必然连不上——这是需要守住的行为边界，
必须有反例测试。

- 关闭 `Dcz2`（kill switch）时服务端 SHALL 只 accept `/1`，且拒绝解码 `EncDcz2` 帧，
  行为与该开关引入前完全一致。

#### Scenario: 老客户端连新服务端

- **WHEN** 客户端只 offer `reply9842/1`，服务端 accept `[reply9842/2, reply9842/1]`
- **THEN** 协商结果为 v1，链路行为（含上行编码选择）与开关引入前一字不变

#### Scenario: 新客户端连老服务端

- **WHEN** 客户端 offer `[reply9842/2, reply9842/1]`，服务端只 accept `reply9842/1`
- **THEN** 协商结果为 v1，客户端自动降级到私有 dcz，不报错

#### Scenario: 客户端只 offer v2

- **WHEN** 客户端只 offer `reply9842/2`，服务端只 accept `reply9842/1`
- **THEN** 握手失败（ALPN 无交集），不得静默降级——此用例锁住"客户端必须 offer v1"

#### Scenario: kill switch 关闭

- **WHEN** 服务端以 `-dcz2=false` 启动
- **THEN** 只接受 v1 连接；若客户端硬发 `EncDcz2` 帧，服务端明确拒绝并说明原因

### Requirement: 代理链路禁止 QUIC 且失败必须可操作

客户端 SHALL 在配置了 `Proxy` 时永不尝试 QUIC：显式 `quic` + 代理必须**立即**
返回可操作错误（提示改用 tcp/auto），`auto` 模式的代理分支不得被历史 QUIC
失败偏好覆盖。GUI 在代理模式下 SHALL 只提供「自动 / TCP」，并自动纠正旧的
「代理 + QUIC」配置。TLS 被中间网络切断（`tls: EOF`）时 SHALL 提示用户切换
「直连」或「代理」，而不是原样展示底层错误。

落地证据：`internal/transport/tunnel.go`（Dial 早退与 prefer 互斥）、
`internal/transport/proxy_mode_test.go`、`cmd/gui/view.go`（transportChoices /
transportForNetwork）、`cmd/gui/format.go`（humanError）、`cmd/gui/network_mode_test.go`、
`cmd/gui/format_test.go`。

#### Scenario: 代理 + 显式 QUIC

- **WHEN** 网络出口为代理且传输方式被设为 QUIC
- **THEN** 拨号在 300ms 内失败，错误明确包含「代理模式不支持 QUIC」，不产生 UDP 探测

#### Scenario: 历史 QUIC 失败后切到代理

- **WHEN** 客户端曾因 QUIC 失败把 prefer 置为 TCP，随后启用代理并以 auto 拨号
- **THEN** 只尝试 TCP（经代理 CONNECT），错误中不出现 quic 尝试

#### Scenario: TUN 劫持导致 TLS 中断

- **WHEN** 拨号返回 `tcp tls: EOF`
- **THEN** GUI 失败行提示「把网络切到直连或代理后重新启动」，并说明疑似代理 TUN 劫持


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

## 已知边界

- **QUIC 生产未启用**：有回环端到端测试通过，生产链路仍走 TCP/TLS，启用需放通 UDP
  端口并验证中间设备。这一条是当前事实，不是设计目标（见 changes/
  `enable-quic-in-production`）。
- `FrameAck`（类型 5）已定义为「保留」，当前无使用方。
