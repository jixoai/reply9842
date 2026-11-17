# fix-passthrough-losses

## Why

为验证 magpie↔magpie 的 RFC 9842 dcz 协商（一组自定义头 + `Content-Encoding: dcz` 的 body）
能否逐字节穿过我们的链路，做的透传普查钉出三处**既有**缺陷——它们在真实流量里还没咬人，
但都属于"静默改变语义"：

1. **多值头只剩首值**：`meta.EncodeHeaders` 只写 `vs[0]`，任何重复头（Set-Cookie、
   X-Magpie-Kept-Body、多值 Accept…）在 client↔server 两个方向静默丢值。
2. **query string 丢失**：客户端只把 `r.URL.Path` 放进帧，`POST /v1/messages?beta=true`
   到上游变成裸路径；幂等键也只含 path，不同 query 会撞同一幂等键。
3. **统计说谎**：上游 4xx/5xx 在服务端逐请求日志里被记成 200（`serveOne` 用
   streamToClient 的返回值——推送完成度——无条件覆盖了真实状态）。

## What Changes

- `EncodeHeaders` 每个值各写一行（`DecodeHeaders` 本就 append 成多值，线格式不用改）。
- 帧 `Path` 改用 `RequestURI`（path + query）；`idem.Key` 纳入 `RawQuery`。
- `serveOne` 仅在 `rec.Status == 0` 时用返回值兜底。
- 三条都转成回归测试（`cmd/server/passthrough_regression_test.go`），此前以"必然失败"的
  钉住用例存在（不允许 t.Skip）。
