# fix-proxy-quic-mismatch

## Why

2026-11-20 实测（用户开启 Clash 系统代理/TUN 后）：

1. 网络出口选「代理」+ 传输方式选「QUIC」时，客户端仍然去拨 QUIC（UDP），
   白等一个握手超时，最终报 `dial: quic dial: context deadline exceeded`——
   看起来像服务端故障，实际是代理不承载 UDP。
2. `auto` 模式本已按「代理只承载 TCP」把顺序定为 `[TCP]`，但紧随其后的
   「上次 QUIC 失败过」粘性偏好又把它改回 `[TCP, QUIC]`，代理链路上会再次
   等一次 QUIC 超时。
3. 用户改选 TCP 后报 `dial: tcp tls: EOF`：这是代理 TUN 把到 28443 的连接
   劫走、在 TLS 握手中间切断的既有已知形态（见 `Dialer.LocalAddr` 注释），
   但界面只展示原始错误，没有告诉用户该切「直连」或「代理」。

## What Changes

- `transport.Dial`：`Proxy != ""` 且 `Mode == KindQUIC` 时**立即**返回可操作
  错误（代理只承载 TCP），不再尝试拨号。
- `transport.Dial`：auto 模式下代理分支与 `prefer` 分支互斥；代理链路永远
  只走 TCP，不因历史 QUIC 失败记录回头再试。
- GUI：代理模式下传输方式只提供「自动 / TCP」；老配置里的「代理 + QUIC」
  在启动与切换网络时自动纠正为 `auto`，并提示原因。
- GUI `humanError`：把 `代理模式不支持 QUIC`、`tls: EOF`、`quic ... context
  deadline exceeded` 翻译成三条可操作提示（改传输方式 / 切直连或代理）。
- 新增测试锁住上述行为（transport 两条、GUI 三条）。
