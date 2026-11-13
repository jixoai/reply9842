# uplink-delta Specification

## Purpose

上行请求体的增量压缩：客户端在历史请求里挑一个基线当 zstd 字典，把新请求体压成
dcz 帧；服务端按内容哈希取到同一份字典后还原。这是整个项目收益的来源（生产实测
293 MB → 659 KB，0.225%），因此这一能力规格要求每条行为都能指到实现。

## Requirements

### Requirement: 内容寻址的单基版本 delta 编码

客户端 SHALL 用 `codec.EncodeDcz(body, baseHash, level, dict)` 产出 dcz 帧：40 字节
skippable 帧头（magic + 版本 + BLAKE3 哈希）+ zstd 流，zstd 侧以基线 body 作为 raw
content dictionary（私有 dictID `0x52463938`）。编码端与服务端的 dictID 必须一致，
否则解码报 magic number mismatch。

落地证据：`internal/codec/dcz.go:147`（`EncodeDcz`）、`internal/codec/dcz.go:167`
（`DecodeDcz`）、`internal/codec/dcz.go:31`（`rawDictID`）。

#### Scenario: 相邻请求压到千字节级

- **WHEN** 同一会话连续发出两条 1.3 MB 级请求，第二条有可用基线
- **THEN** 第二条的 wire 体积落到千字节量级（生产 p50 实测 1,052 B），
  且服务端还原出的 body 与客户端发出的字节完全一致

#### Scenario: 无基线时退回普通压缩

- **WHEN** 历史窗口里没有达到复用率阈值的基线
- **THEN** 请求体改走无字典 zstd（`codec.Compress`），编码名记为 `zstd(L<n>)`，
  且 `rec.BaseHit` 保持 false——不得把无基线的压缩记成 delta 命中

### Requirement: 基线选择用 4 KB 块哈希命中率

客户端 SHALL 用「块哈希命中率」而非「从头的最长公共前缀」判定基线是否值得复用：
基线切 4 KB 块建索引，新请求滑动匹配，统计可复用字节占基线总字节的比例，达到
`minReuseRatio = 0.5` 才使用。评分函数、时间远近、体积接近度一律不参与（实测为噪音）。

落地证据：`internal/basefind/finder.go:92`（`Pick`）、`internal/basefind/finder.go:119`
（`blockReuseRatio`）、`internal/basefind/finder.go:36`（`minReuseRatio`）、
`internal/basefind/finder.go:26`（`blockSize = 4096`）。

#### Scenario: 跳过前 64 字节的分歧点仍能命中

- **WHEN** 新请求与某基线的第 45 字节起就不同（`max_tokens` 数值变化），
  但第 64 字节之后 90% 以上逐字节相同
- **THEN** 该基线仍然被选中（回归护栏 `TestPickSkipsLeadingDivergence` 钉住此行为）

#### Scenario: 跨簇请求不被误配

- **WHEN** 不同会话的请求（块哈希命中率仅 1.4–2.2%）
- **THEN** 不选它当基线，改走无字典 zstd，不产生「错字典」导致的语义错乱

### Requirement: 历史窗口按字节预算而非条数淘汰

客户端历史表 SHALL 按总字节预算（`-history-mb`，默认 64 MB）淘汰最旧条目，而不是按
条数。同一内容只存一份。不做 LRU：会话单调推进，新请求天然更可能当基线，FIFO 就够。

落地证据：`internal/basefind/finder.go:53`（`New(budgetMB)`）、
`internal/basefind/finder.go:61`（`Remember`）、
`internal/relayclient/runner.go:49`（`DefaultHistoryMB = 64`）。

#### Scenario: 高并发交错不挤出可用基线

- **WHEN** 多 Agent 并发交错发请求，相邻同簇请求间隔达 21 条
- **THEN** 64 MB 预算仍能容纳（约 35 条 1.8 MB 请求），基线不被条数限制误淘汰

### Requirement: 按体积自适应 zstd level

编码 level SHALL 按请求体体积自适应：≤ 2 MB 用 19，≤ 8 MB 用 12，> 8 MB 用 9。
不得对大文档无脑用 19——实测 41 MB 文档用 19 需 34.5 s / 328 MB RSS。

落地证据：`internal/codec/dcz.go:110`（`PickLevel`），调用点在
`internal/relayclient/runner.go` 的 `ServeHTTP`（delta 路径与无基线路径两处）。

#### Scenario: 大文档自动降级

- **WHEN** 请求体为 30 MB 级
- **THEN** 取 level 9，编码耗时不随体积线性爆炸，且产物仍小于明文

#### Scenario: 小请求不做 delta

- **WHEN** 请求体小于 `-min-delta`（默认 16384 字节）
- **THEN** 不走基线选择，直接在超过 1 KB 时用无字典 zstd，否则原样 identity

### Requirement: 三道防静默损坏防线不可关闭

系统 SHALL 同时强制三层校验：① zstd 帧 CRC；② dcz 头 BLAKE3 哈希与所用字典一致；
③ `ResultHash`——客户端声明「解码后应得到的 body 哈希」，服务端解压后验证。任一失败
必须返回错误帧让客户端重发全量，**没有关闭开关**。

落地证据：`internal/codec/dcz.go:87`（`zstd.WithEncoderCRC(true)`，注释写明「强制
checksum，防静默损坏」）、`internal/codec/dcz.go:167`（`DecodeDcz` 内的哈希校验）、
`cmd/server/main.go:296` 附近的 `ResultHash` 校验分支（失败即 `result hash mismatch
(silent corruption guard)`）。

#### Scenario: 字典不匹配被拒绝

- **WHEN** 客户端声明的 `BaseHash` 在服务端 store 中不存在
- **THEN** 服务端返回 `FrameDeltaMiss`，客户端以 identity/zstd 重发全量，
  绝不尝试「用相似字典凑一下」

#### Scenario: 内容错乱被 ResultHash 拦下

- **WHEN** 解码成功但解出的 body 与客户端声明的 `ResultHash` 不等
- **THEN** 服务端返回错误帧、不转发上游、不写入基线缓存，该请求记为 422

### Requirement: 服务端字典缓存是纯内容寻址 LRU

服务端 SHALL 维护 `hash → body` 的纯 KV（`store`），按字节上限 LRU 淘汰（`-cache-mb`，
默认 256 MB），无目录、无版本号、无会话 ID、无协商协议。淘汰后自然 miss。

落地证据：`internal/store/store.go:50`（`Put`）、`internal/store/store.go:70`（`Get`）、
`internal/store/store.go:84`（`Has`）、`internal/store/store.go:91`（`evictLocked`）、
`cmd/server/main.go:140`（`store.New(c.cacheMB << 20)`）。

#### Scenario: 淘汰后客户端收到 DeltaMiss

- **WHEN** 某基线已因预算被淘汰，客户端仍以它的哈希发起 delta 请求
- **THEN** 服务端返回 `FrameDeltaMiss`，客户端重发全量并在日志里把该条记为
  `resend-full`

#### Scenario: 协商成本恒定

- **WHEN** 缓存规模从 256 MB 扩大到任意大小
- **THEN** 每次请求的协商开销恒为 32 字节哈希，不随缓存条目数增长

## 非目标

- 不做块级去重（CDC/FastCDC）：实测比整文档 delta 差 13 倍
- 不做多基线投票、不做跨会话共享前缀字典（后者见 changes/
  `add-shared-crosssession-dict`）
- 不做 JSON 规范化（RFC 8785 JCS）——当前方案不需要
