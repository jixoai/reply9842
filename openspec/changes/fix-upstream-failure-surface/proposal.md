# fix-upstream-failure-surface

## Why

2026-11-16 生产事故：`reply9842-server` 的 systemd 单元把 `-upstream` 配成了
`http://127.0.0.1:3431`——那是 magpie 蓝绿部署里 **b 槽 UI 的 standby 端口**
（`magpie-pool.conf` 标 `down`），网关的活跃端口是 `3425`。于是每个请求在服务端
都 `connection refused`，而这批失败在客户端侧的表现是：

```
"status": 200, "down_raw": 0, "err": "server: Post \"http://127.0.0.1:3431/v1/messages\": connect: connection refused"
```

即上游失败被渲染成了 **200 + 空 body**，调用方只看到「空返回」，真实原因完全传不出去。
两个独立缺陷叠加：

1. **失败没有真实的 HTTP 状态**：服务端在响应头产生前失败时只发 `FrameError`（帧级
   信号）；客户端 handler 没写过状态码，`net/http` 在 Handler 返回时自动补 `200`，
   下游拿到空 200。幂等键连续两次 miss（`resend-full` 也失败）走的是同一条真空路径。
2. **上游端口写死**：magpie 是蓝绿部署，`rolling-update.sh` 会翻转 a/b 两槽的端口
   （3425/3426）。写死单个端口意味着每次轮转之后 relay 都会整体失联，且现象同上，
   排查成本极高。

## What Changes

- 服务端：上游失败且响应头尚未产生时，发送**完整响应**（`FrameResponse` 头
  502/504 + JSON 错误体 + `FrameDone`），而不是只发 `FrameError`。任何版本的客户端
  都能如实显示 5xx 与原因。
- 服务端：`-upstream` 接受逗号分隔的候选列表，拨号失败时按序尝试下一个
  （只对「请求一个字节都没发出去」的拨号失败做故障转移，POST 不可重放）。
  默认值改为 `http://127.0.0.1:3425,http://127.0.0.1:3426`。
- 客户端：一个响应头都没收到就结束时，`finalizeNoHead` 给下游写真实 502 + 原因，
  绝不留下空的 200（覆盖 FrameError 与幂等键连续 miss 两条路径）。
- 服务端：spool 恢复不再在启动时全量扫盘重建 SSE 事件索引，改为首次按 event id
  续传时惰性重建。2GB idem 区启动从 90 秒降到秒级（同一事故排查中实测）。
- 文档/规范：`-upstream` 默认值与候选列表语义同步到 README / DEPLOY / npm README /
  site / specs。
