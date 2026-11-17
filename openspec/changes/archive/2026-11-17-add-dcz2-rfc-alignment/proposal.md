# add-dcz2-rfc-alignment

## Why

我们的 dcz 是 RFC 9842 §5 的**私有变体**：40 字节头布局与 magic 逐字节符合 RFC，但头内摘要是
BLAKE3-256（RFC 要求 SHA-256），且 zstd 用了私有字典 ID `0x52463938`（RFC 的 dcz 用 raw 字典、
靠头里的哈希匹配、不写字典 ID）。当初这样选是因为两端都是自家实现（代码注释即写明"仅作隧道内
标识，不对接外部 dcz 解码器"）。

2026-11-16 起第三方（yetone/magpie 的 `internal/gateway/remote_dict.go`）按 RFC 实现了同一套
思路（其注释注明 idea 源自本仓库），并已声明"暂不能与 reply9842 直接对上"。要让 reply9842 与
RFC 生态（magpie 之间、将来 CDN/浏览器）互通，必须提供**字节级符合 RFC 9842 的编码**；同时
老客户端（只认私有 dcz）必须继续可用——线上 auto-update 是关闭的，版本矩阵会长期共存。

## What Changes

- 新增编码 **dcz2**：同一 40 字节头（8B magic + 32B **SHA-256**(dict)），zstd **raw 字典、ID 0**，
  CRC 打开；压缩参数与现 dcz 对齐。现 dcz（BLAKE3 + 私有 ID）**一行不改**。
- **协商走 ALPN 双版本**：`reply9842/1`（老，私有 dcz）/ `reply9842/2`（新，dcz2）。客户端
  offer `[v2, v1]`，服务端按 `Dcz2` 开关决定 accept 列表；协议在握手期定论，零额外 RTT，
  老服务端无需理解 v2（从 offer 里挑 v1 即可）。
  依据（已按 Go 源码核实）：`crypto/tls` 的 `negotiateALPN` 按服务端偏好取第一个交集，
  **无交集直接握手失败**，所以客户端必须同时 offer v1 才能对老服务端降级。
- 上行编码由客户端按连接版本选择（v2→dcz2、v1→dcz），服务端**两种都能解**（靠帧内显式
  `Encoding` 字段，与连接版本无关）；下行仍只有 identity/zstd 生产路径，但客户端解码能力
  覆盖 identity/zstd/dcz/dcz2，且**未知编码不再按明文兜底**（历史上那正是静默损坏的形态）。
- 服务端新增 kill switch `-dcz2`（默认 true）：关闭时只 accept `/1` 且拒解 `EncDcz2`，
  行为与 v0.3.10 完全一致，作为生产回滚开关。
- 内部内容寻址（store/缓存 key）继续用 BLAKE3：dcz2 只把 SHA-256 用在线上头字段，
  避免缓存冷启动；代价是每请求多算一次 SHA-256（实测本机 1MB ≈0.94ms、香港机 ≈2.7ms）。

## 兼容矩阵（必须全部成立，均有测试）

| 客户端 | 服务端 | 结果 |
|---|---|---|
| 老（offer `/1`） | 新（accept `[v2,v1]`） | 选 v1 → 私有 dcz，行为与今天一字不变 |
| 新（offer `[v2,v1]`） | 老（accept `[/1]`） | 选 v1 → 私有 dcz，不报错 |
| 新 | 新 | 选 v2 → 上行 EncDcz2 |
| 新（只 offer `/2`） | 老 | **握手失败** —— 反面用例锁住，防止有人"优化"掉 v1 |
| 任意 | 新 + `-dcz2=false` | 落 v1；硬发 EncDcz2 被明确拒绝（错误文案含 kill switch） |
