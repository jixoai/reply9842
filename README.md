# rfc9842-reply

基于内容寻址基版本的增量压缩隧道，用于降低 Agent ↔ 网关之间的上行带宽。

## 它解决什么问题

自建 Agent 跑 LLM 请求时，每次都要把完整上下文（system prompt + tools schema +
多轮历史）重新上传。实测某 Agent 的负载：

| 指标 | 实测值 |
|---|---|
| 日请求量 | 22,000 |
| 日上行 | 44 GB |
| 日下行 | 3 GB（上行/下行 = 14.8:1） |
| 每请求上行 p50 | 791 KB |
| 相邻请求体积比 p50 | **1.003**（几乎完全相同） |

**关键洞察**：相邻请求的体积极少变化，说明绝大部分内容是重复传输。

配合 gzip 只能压到 36%（1.4MB → 530KB），因为 gzip 没有跨请求的状态。

## 方案：dcz 单基版本 delta

```
relay-client (本地)                      relay-server (远程)
──────────────────                       ──────────────────
Agent → :8899
  │
  ├─ 选基线：历史 body 中找最长公共前缀
  ├─ zstd --patch-from <base>  ← 1450:1 压缩
  ├─ 40 字节 dcz 头（密文 + BLAKE3 哈希）
  └─ QUIC/TCP 隧道发出
                                          │
                                          ├─ 按哈希从 store 取字典
                                          ├─ 解 delta + 校验 ResultHash
                                          ├─ 转发 127.0.0.1:3431（同机回环）
                                          └─ 存本次 body 供后续当基线
```

### 实测效果

| 场景 | 请求体 | 传输量 | 压缩比 |
|---|---|---|---|
| 首轮（无基线） | 205 KB | 451 B | 0.22% |
| 第 2 轮（命中基线） | 205 KB | **112 B** | **0.05%** |
| 第 3 轮 | 205 KB | **103 B** | **0.05%** |
| 大会话首轮 | 1.69 MB | 387 KB | 22.85% |
| 大会话后续 | 1.69 MB | **271 B** | **0.02%** |

对比现状（gzip 1.4MB → 530KB），**提升约 1955 倍**。

## 为什么不用块级去重（CDC）

调研实测（FastCDC, min 2KB / avg 8KB / max 64KB，292KB agent 请求）：

| 方案 | 成本 |
|---|---|
| FastCDC 块级去重 | **17 KB**（23 块，19 块可复用） |
| **整文档 zstd delta** | **1,297 B** |

**差 13 倍。** 原因：LLM 会话是尾部追加为主，LZ77 字典压缩天然对「未变的前缀」
只编码一个变长 match（2-3 字节），块级去重则把未变的 12.7KB 当成不可分原子。

**唯一例外**：跨会话的固定前缀（system + tools schema）。这个用共享字典
（`-D` / `WithEncoderDictRaw`）即可，不需要 CDC。

## 为什么不需要「字典目录」

一致性靠**内容哈希**，不靠版本号：

```
client: BaseHash = blake3(上一轮 body)   ← 32 字节
server: store[BaseHash] 存在 → 解码；不存在 → FrameDeltaMiss → 客户端重发全量
```

- **没有目录**：`store` 是纯 `hash → body` 的 KV
- **没有版本号**：不存在「第几版」这种需要双向同步的可变状态
- **没有会话 ID**：服务端完全无状态，可以水平扩展
- **协商成本固定 32 字节**，不随缓存规模增长

这与 RFC 9842（Compression Dictionary Transport, 2025-09 Standards Track）的设计一致。
其 §2.1.3 明文规定：**服务端 ID 不得用于保证字典内容，必须校验哈希**。

### 历史教训：SDCH 为什么死了

Chrome 59 移除 SDCH 的原因：字典老化太快，压缩率退化到不如 gzip，维持字典新鲜度
的成本超过收益。RFC 9842 之所以存活，是把「字典老化」转成 HTTP 缓存问题。

**启示**：不要设计「客户端块缓存 + 服务端块缓存 + 一致性协议」——那是 SDCH 的坑。
「上一轮 body 当字典」天然随会话推进而更新，不存在老化。

## 防静默损坏

这是本方案最危险的地方。zstd 的 frame checksum 可以关闭（`--no-check`），一旦关闭 +
字典错误 + 内容仍可解码，就会得到**语义正确但内容错乱**的 LLM 请求——模型不报错，
只给出奇怪回答，几天后才发现。

**三道防线**（全部强制开启，不可配置）：

1. `zstd.WithEncoderCRC(true)` — 帧级 checksum
2. dcz 头里的 BLAKE3 哈希，解压前与提供的字典校验
3. `ResultHash` — 客户端声明「解码后应得到的 body 哈希」，服务端解压后验证

任一失败都返回错误帧，客户端重发全量。

## 传输层：QUIC / TCP 双栈

```
-transport=auto|quic|tcp     默认 auto
```

**关于「QUIC 能否优雅回退」的澄清**：

HTTP/3 的自动回退是 **HTTP 协议族**的能力（浏览器用 `Alt-Svc` 探测，失败退回
HTTP/2 over TCP），**不是 QUIC 传输层自带**。`quic-go` 连不上就是连不上。

因此回退逻辑实现在应用层（`Dialer.Dial`）：先试 QUIC（默认 1s 超时），失败降级
TCP+TLS，并记住结果用于后续连接的优先级。

## 快速开始

### 服务端

```bash
go build -o relay-server ./cmd/relay-server

./relay-server \
  -listen 0.0.0.0:28443 \
  -upstream http://127.0.0.1:3431 \
  -uphost magpie.aiweb.xin \
  -cert /path/to/fullchain.pem \
  -key /path/to/privkey.pem \
  -cache-mb 512
```

**`-upstream` 必须是 `127.0.0.1`**。若填公网域名，流量会经过 NAT 网关绕一整圈公网
（实测：香港机公网 IP 不在网卡上，`172.17.61.35` 才是内网地址）。

### 客户端

```bash
go build -o relay-client ./cmd/relay-client

./relay-client \
  -listen 127.0.0.1:8899 \
  -server <server-ip>:28443 \
  -server-name magpie.aiweb.xin \
  -transport auto
```

然后 Agent 指向 `http://127.0.0.1:8899`。

## 参数

### relay-client

| 参数 | 默认 | 说明 |
|---|---|---|
| `-listen` | `127.0.0.1:8899` | 本地 HTTP 入口 |
| `-server` | `127.0.0.1:9000` | relay-server 的 host:port |
| `-server-name` | 自动推导 | TLS SNI |
| `-transport` | `auto` | `auto` / `quic` / `tcp` |
| `-insecure` | `false` | 跳过证书校验（自签观察期） |
| `-min-delta` | `16384` | 小于此值的请求体不做 delta |
| `-min-prefix` | `0.5` | 公共前缀低于此比例不做 delta |
| `-history` | `8` | 保留的历史基线条数 |
| `-log` / `-stats` | `~/.rfc9842-reply/` | 日志与统计路径 |

### relay-server

| 参数 | 默认 | 说明 |
|---|---|---|
| `-listen` | `0.0.0.0:9000` | 隧道监听（QUIC + TCP 同端口） |
| `-upstream` | `http://127.0.0.1:3431` | 上游 base URL |
| `-uphost` | `magpie.aiweb.xin` | 转发时覆盖的 Host 头 |
| `-cert` / `-key` | 必填 | TLS 证书 |
| `-cache-mb` | `512` | 基版本缓存上限 |

## 观测

### `~/.rfc9842-reply/client-stats.json`（每次请求后覆盖）

```json
{
  "requests": 8,
  "up_raw": 851777,
  "up_wire": 71449,
  "up_ratio": 0.0839,
  "base_hits": 2,
  "encodings": {"dcz(L19)": 2, "identity": 4, "zstd(L19)": 2},
  "transport": {"tcp": 8},
  "p50_ms": 50, "p90_ms": 68
}
```

### `~/.rfc9842-reply/client-<date>.jsonl`（逐请求追加）

字段：`t` `method` `path` `status` `enc` `base_hit` `up_raw` `up_wire`
`down_raw` `down_wire` `ms_total` `ms_encode` `ms_wire` `ms_decode`
`ms_upstream` `up_bps` `down_bps` `transport` `err`

```bash
tail -f ~/.rfc9842-reply/client-2026-11-12.jsonl | \
  jq -c '{path,status,enc,up_raw,up_wire,ms_total}'
```

## 目录结构

```
cmd/
├── relay-client/     本地 HTTP 入口 + delta 编码
└── relay-server/     隧道服务端 + 上游转发
internal/
├── meta/             帧格式（magic + 版本 + 类型 + 长度）
├── codec/            dcz 编解码、zstd 自适应 level、静默损坏防护
├── store/            内容寻址 LRU（零协商）
├── basefind/         客户端基线选择（最长公共前缀评分）
├── transport/        QUIC + TCP 双栈 + auto 回退
└── stats/            逐请求与汇总统计
```

## 自适应压缩 level

调研实测（1.86MB → 1.89MB 增量）：

| level | delta 大小 | 耗时 |
|---|---|---|
| -1 | 11,651 B | ~0s |
| -9 | 2,615 B | 0.01s |
| **-19** | **1,297 B** | **1.51s** |

从 -9 到 -19，**大小降 2 倍，时间涨 150 倍**。且 41MB 文档 -19 需 34.5s / 328MB RSS。

因此按体积自适应（`codec.PickLevel`）：

| 请求体 | level |
|---|---|
| ≤ 2 MB | 19 |
| ≤ 8 MB | 12 |
| > 8 MB | 9 |

## 已知限制

1. **响应不压缩**。当前只优化上行；下行 p50 约 18KB，暂未处理。
2. **QUIC 路径待真机验证**。TCP 已全绿，QUIC 在本地回环测试有 stream 时序问题。
3. **大文档内存**。30MB 级请求的 delta 需要显著内存，需按 `PickLevel` 降级。
4. **单请求体全量缓冲**。客户端与服务端都会把 body 读进内存（为统计与压缩）。
5. **跨用户字典隔离**。若将来共享字典，必须按租户隔离——混合公私数据的字典
   存在 CRIME/BREACH 类攻击面（RFC 9842 §9.2）。

## 参考规范

| 规范 | 用途 | 采纳程度 |
|---|---|---|
| RFC 9842 (Compression Dictionary Transport) | dcz 线格式、内容哈希协商 | 线格式一致；请求方向为自研扩展 |
| RFC 8878 (Zstandard) | raw content dictionary | 直接用 |
| RFC 9530 (Repr-Digest) | 表示身份摘要 | 语义参考 |
| RFC 7694 (Client-Initiated Content-Encoding) | 请求方向编码协商 | 语义参考 |
| RFC 8785 (JCS) | JSON 规范化 | 若做块哈希则必需（本方案未用） |
