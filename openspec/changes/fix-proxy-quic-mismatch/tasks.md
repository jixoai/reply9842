# Tasks: fix-proxy-quic-mismatch

- [x] 1.1 `Dialer.Dial`：代理 + 显式 QUIC 立即报错（不拨号、不等待）
- [x] 1.2 `Dialer.Dial`：auto + 代理与 `prefer` 互斥，代理链路永不尝试 QUIC
- [x] 1.3 transport 测试：显式 QUIC+代理快速失败；stale prefer 下仍不出现 quic 拨号
- [x] 2.1 GUI：代理模式只渲染「自动 / TCP」，`transportForNetwork` 纠正 quic→auto
- [x] 2.2 启动加载与切换网络模式时应用纠正，并提示「代理模式不支持 QUIC」
- [x] 2.3 GUI 测试：选项过滤、纠正逻辑
- [x] 3.1 `humanError`：代理+QUIC、tls: EOF、quic 超时三条可操作文案
- [x] 3.2 GUI 测试：三条错误映射
- [x] 4.1 `go test ./...` / `go vet ./...` / `go build ./...` 全绿
