# Tasks: bound-request-body-size

## 1. 客户端

- [ ] 1.1 `relayclient.Config` 新增 `MaxBodyMB`（默认建议 64），CLI 加
      `-max-body-mb`，GUI 设置页加一行
- [ ] 1.2 `ServeHTTP` 读 body 前先做长度预检：`r.ContentLength > limit` 时**直接降级**
      （跳过 delta 与压缩，identity 转发），不发 `MaxBytesReader` 错误——保持功能可用
- [ ] 1.3 body 已开始读之后才发现超限（chunked 无 Content-Length）时，仍必须能完成转发：
      用 `io.LimitReader(limit+1)` 探测，超限则把已读部分 + 剩余流式转发（不做压缩）
- [ ] 1.4 stats 记 `enc=identity` 且 `err` 或独立字段标明「body_too_large_for_delta」，
      便于事后统计这类请求占比

## 2. 服务端

- [ ] 2.1 `cmd/server` 新增 `-max-body-mb`（默认与客户端一致）
- [ ] 2.2 `serveOne` 解码前检查 `h.BodyLen` / 解出的长度，超限返回明确错误帧
      （`FrameError` + 可识别文本），不转发上游、不写 store
- [ ] 2.3 记录为 413 到 stats

## 3. 测试与文档

- [ ] 3.1 单测：`TestClientDegradesOnOversizeBody`（超限请求成功且 enc=identity）、
      `TestClientNormalBodyStillDeltas`（不超限不受影响）
- [ ] 3.2 单测：`TestServerRejectsOversizeBody`（返回错误帧、无上游请求、stats 计 413）
- [ ] 3.3 README 已知限制第 5 条改写；参数表补 `-max-body-mb`
- [ ] 3.4 与帧级 256 MB 上界的关系写清（帧界是传输层兜底，body 界是应用层策略）

## 验收标准

- 发送一个超过 `-max-body-mb` 的请求：客户端不 OOM、请求仍成功（identity 转发）、
  stats 里能看出它没走压缩。
- 服务端收到超限请求：返回明确错误，不产生上游请求，不占用 spool/store。
- 正常请求的压缩比不受任何影响。
