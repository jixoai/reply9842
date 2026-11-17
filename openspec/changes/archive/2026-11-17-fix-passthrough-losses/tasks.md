# Tasks: fix-passthrough-losses

- [x] 1.1 EncodeHeaders 每值一行 + 多值头往返测试（请求/响应两个方向）
- [x] 1.2 帧 Path 带 query（RequestURI）；idem.Key 纳入 RawQuery
- [x] 1.3 serveOne 状态兜底只在 rec.Status==0 时生效 + 400 记录回归测试
- [x] 1.4 钉住用例转正为回归保护（passthrough_regression_test.go）
