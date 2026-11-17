## ADDED Requirements

### Requirement: RFC 9842 对齐编码 dcz2

上行体 SHALL 支持编码 `dcz2`：40 字节头（8 字节 magic `5e 2a 4d 18 20 00 00 00` +
32 字节 **SHA-256**(字典)），随后为使用该字典的 zstd 流（raw 字典、**不写字典 ID**、
帧校验和打开）。该格式 SHALL 与现有私有 `dcz`（BLAKE3 摘要 + 私有字典 ID `0x52463938`）
并存且互不替代。

两种格式的 40 字节头无法靠内容嗅探区分，SHALL 由帧内显式 `Encoding` 字段声明；
解码器 SHALL 交叉拒绝（用 dcz 解码器解 dcz2 载荷、或反向）必须报错，不得产出字节。

内部内容寻址（store/缓存 key）SHALL 继续使用 BLAKE3；SHA-256 只用于线上头字段。

#### Scenario: 头布局符合 RFC

- **WHEN** 对一个已知字典与正文编码 dcz2
- **THEN** 头的第 1–8 字节等于 magic；第 9–40 字节等于该字典的 SHA-256；
  随后 zstd 帧首部不携带字典 ID；帧校验和位为 1

#### Scenario: 交叉解码被拒绝

- **WHEN** 用 dcz 解码器解一段合法 dcz2 载荷（或反向）
- **THEN** 解码失败且不输出任何明文

#### Scenario: 老协议不受影响

- **WHEN** 在 v1 连接上跑一次完整请求
- **THEN** 上行编码与字节流与引入 dcz2 之前完全一致（dcz 路径零改动）
