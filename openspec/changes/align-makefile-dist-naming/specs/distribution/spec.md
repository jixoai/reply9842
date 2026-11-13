## ADDED Requirements

### Requirement: 本地 dist 产物与 Release 命名及清单完全一致

`make dist` SHALL 产出与 CI 相同命名（`<kind>-<os>-<arch>[.exe]`）与相同清单（16 个）的
产物，并在结尾做完整性断言（逐一比对，缺失或空文件即失败）。Makefile 注释里关于「便于
本地验证 release 产物」的承诺必须由这一条兑现。

#### Scenario: 命名与 Release 对得上

- **WHEN** `make dist` 后执行 `ls dist`
- **THEN** 文件名与 GitHub Release 的 Assets 一一对应，没有 `relay-` 前缀差异

#### Scenario: 少一个产物会被发现

- **WHEN** 构建过程中某个目标失败
- **THEN** `make dist` 以非零码退出并指出缺哪个文件，不留下一个「看起来成功」的不完整
      `dist/`

#### Scenario: server 也被构建

- **WHEN** `make dist`
- **THEN** 除了 cli 5 个与 gui 3 个，还有 server 5 个，共 16 个
