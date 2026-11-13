## ADDED Requirements

### Requirement: 超大请求体的显式上限与降级策略

客户端 SHALL 有显式的请求体上限（`-max-body-mb`）。超过上限时 SHALL 降级为 identity
转发而不是失败：功能保持可用，只是放弃压缩收益。降级必须在 stats 里可识别（编码名为
`identity` 且带可区分的原因标记），便于事后统计这类请求的占比。

上限必须在**读取 body 之前**尽可能判定（`ContentLength` 预检），避免先吃掉内存再判断。

#### Scenario: 巨体请求仍成功

- **WHEN** 请求体超过 `-max-body-mb`
- **THEN** 请求被 identity 转发并成功返回，进程内存不因此爆掉

#### Scenario: 正常请求的压缩比不受影响

- **WHEN** 请求体在限内
- **THEN** delta 与压缩行为与改动前完全一致

#### Scenario: 降级原因可统计

- **WHEN** 查看 stats 中该请求的记录
- **THEN** 能看出它是「因超大而未压缩」，而不是把它误读为「基线未命中」
