## ADDED Requirements

### Requirement: ALPN 双版本协商

传输层 SHALL 支持两个 ALPN：`reply9842/1`（老客户端，私有 dcz 上行编码）与
`reply9842/2`（RFC 9842 对齐编码 dcz2）。服务端按 `Dcz2` 开关决定 accept 列表；
客户端 SHALL 同时 offer 两个版本（顺序仅表达偏好，选择权在服务端）。

依据：`crypto/tls` 的 `negotiateALPN` 按服务端偏好顺序取第一个交集，**无交集时握手失败**
（不是降级）。因此只 offer `/2` 的客户端对老服务端必然连不上——这是需要守住的行为边界，
必须有反例测试。

- 关闭 `Dcz2`（kill switch）时服务端 SHALL 只 accept `/1`，且拒绝解码 `EncDcz2` 帧，
  行为与该开关引入前完全一致。

#### Scenario: 老客户端连新服务端

- **WHEN** 客户端只 offer `reply9842/1`，服务端 accept `[reply9842/2, reply9842/1]`
- **THEN** 协商结果为 v1，链路行为（含上行编码选择）与开关引入前一字不变

#### Scenario: 新客户端连老服务端

- **WHEN** 客户端 offer `[reply9842/2, reply9842/1]`，服务端只 accept `reply9842/1`
- **THEN** 协商结果为 v1，客户端自动降级到私有 dcz，不报错

#### Scenario: 客户端只 offer v2

- **WHEN** 客户端只 offer `reply9842/2`，服务端只 accept `reply9842/1`
- **THEN** 握手失败（ALPN 无交集），不得静默降级——此用例锁住"客户端必须 offer v1"

#### Scenario: kill switch 关闭

- **WHEN** 服务端以 `-dcz2=false` 启动
- **THEN** 只接受 v1 连接；若客户端硬发 `EncDcz2` 帧，服务端明确拒绝并说明原因
