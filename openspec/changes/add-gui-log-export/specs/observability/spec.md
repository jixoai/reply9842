## ADDED Requirements

### Requirement: GUI 侧日志与统计错误必须有可观测出口

GUI SHALL 把内存中的运行日志与统计器错误暴露到界面上（见本 change 的 client-gui delta）。
观测数据不得只存在于进程内存中而无任何读取路径。

#### Scenario: 观测数据可达

- **WHEN** 用户需要排查一次失败
- **THEN** 他能从界面取到运行日志与统计器错误原因，不必依赖终端（Windows 上也没有终端）
