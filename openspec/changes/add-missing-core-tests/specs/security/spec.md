## ADDED Requirements

### Requirement: 幂等键的跨调用者隔离必须有测试证明

`internal/idem` SHALL 有单元测试证明两条安全不变量：① 仅 `callerKeyId` 不同则幂等键不同；
② 无凭据时稳定退化为 `anon`（因此无凭据用户之间会互相命中——这是已知缺口，测试要显式
把它记录成事实而不是静默）。

#### Scenario: 不同 token 不同键

- **WHEN** 两个请求除 Authorization 外完全相同
- **THEN** 它们的幂等键不同

#### Scenario: 同一 token 稳定

- **WHEN** 同一 token 的同一请求计算两次
- **THEN** 两次得到同一个键

#### Scenario: 无凭据的已知缺口被记录

- **WHEN** 两个请求都不带 Authorization
- **THEN** 二者键相同（都归 `anon`），该行为被测试显式断言，以免有人误以为已隔离
