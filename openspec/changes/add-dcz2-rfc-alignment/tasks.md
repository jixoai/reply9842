# Tasks: add-dcz2-rfc-alignment

## 1. 编码

- [x] 1.1 `internal/codec/dcz2.go`：`EncodeDcz2/DecodeDcz2/PeekDcz2Hash`、SHA-256 头、
      zstd raw 字典 ID 0、CRC 打开；现 dcz 一行不改
- [x] 1.2 单元测试：头布局向量（magic / 第 9–40 字节 = 外部计算的 SHA-256 / 帧内无字典 ID /
      CRC=1）、往返、**双解码器互斥**（dcz2 载荷用 dcz 解必须报错且不吐字节，反向同理）

## 2. 协商

- [x] 2.1 `transport.ALPNv1/ALPNv2`；`Server.Dcz2` 开关决定 accept 列表（零值=只收 v1）
- [x] 2.2 客户端 offer `[v2,v1]`；`Conn.SupportsDcz2()` 作为唯一判据选择上行编码
- [x] 2.3 服务端 `-dcz2`（默认 true）kill switch + 启动日志；关闭时拒解 EncDcz2
- [x] 2.4 测试：ALPN 四格矩阵 + 只 offer v2 时对老服务端握手失败的反面用例

## 3. 数据通路

- [x] 3.1 `cmd/server` `decodeBody` 新增 EncDcz2 分支（BLAKE3 查 store + SHA-256 复核），
      解不开按 miss 处理（客户端重发全量）
- [x] 3.2 客户端解码能力覆盖 identity/zstd/dcz/dcz2；未知编码明确失败
- [x] 3.3 E2E：新×新 上行 EncDcz2 → 上游逐字节收到原文；kill switch 关闭时降级 v1 / 拒解
- [x] 3.4 池中连接写失败换新连接重发一次（ALPN 选码把探活→写窗口拉长后的收口）

## 4. 文档与回滚

- [x] 4.1 README 补 dcz2/ALPN/flag 说明
- [ ] 4.2 发布后：生产 `-dcz2=false` 演练一次回滚路径（仅验证开关语义，随后恢复 true）
