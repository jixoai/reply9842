# auto-update Specification

## Purpose

可关闭的自动更新：查 GitHub Release、按与 npm 引导器一致的镜像链下载、强制 SHA256 校验、
防变砖地自我替换并重启恢复服务。默认关闭——自我替换的风险必须由用户显式接受。

## Requirements

### Requirement: 版本检查与 semver 比较

系统 SHALL 通过 GitHub API（`releases/latest`）查询最新版本，与当前版本做 **semver
比较**而非字符串比较（`v0.10.0` 必须大于 `v0.2.1`）。API 限流时必须返回明确错误而非
崩溃，并支持 `GITHUB_TOKEN` 提升限额。

落地证据：`internal/update/check.go`（Check / 限流分类）、
`internal/update/semver.go`（比较实现）、`internal/update/semver_test.go`。

#### Scenario: 两位数版本号

- **WHEN** 当前 `v0.2.1`、最新 `v0.10.0`
- **THEN** 判定有新版本（字符串比较会得出相反结论）

#### Scenario: GitHub API 限流

- **WHEN** 未认证配额（60 次/小时）用尽，API 返回 403/429
- **THEN** 返回可读的限流错误，不 panic、不阻塞界面，用户可稍后重试

### Requirement: 源码构建禁止自动替换

系统 SHALL 识别本地构建并**禁止**自我替换。仅靠「semver 解析失败」不够：
`git describe` 注入的 `v0.1.0-3-gabcdef` 能合法构成预发布段并通过解析。判定必须覆盖
`""` / `dev` / `dev-<sha>` / `git-describe` / `-dirty` 五种形态。

落地证据：`internal/update/semver.go`（`IsDevBuild`）、
`internal/update/semver_test.go`、`cmd/gui/update.go`（`updateBlockedReason`）。

#### Scenario: 开发机上点更新

- **WHEN** 二进制由 `go build` 产出（版本为 `dev` 或 git-describe 形态），用户在界面点「更新」
- **THEN** 连**下载都不发生**，状态行说明「源码构建不参与自动替换」

#### Scenario: 手改配置也不能绕过

- **WHEN** 用户手动把 settings.json 改成 `auto_update: true`
- **THEN** 启动时的自动检查仍跳过（记一行日志，不弹提示）

### Requirement: 下载契约与 npm 引导器一致

下载逻辑 SHALL 与 `npm/lib/mirror.js` 使用同一套镜像契约：同一个 `REPLY9842_MIRROR`
变量、同样的 CSV 解析规则（trim / 跳空项 / `direct` 大小写不敏感 / 无有效项即直连 /
末尾补斜杠 / 保序去重）、同样的 URL 拼接方式。差异仅两处且必须有理由：本包 sha256
校验**强制**（GUI 自我替换失败没有命令行可退），且支持断点续传。

落地证据：`internal/update/mirror.go`、`internal/update/mirror_test.go`（表驱动）、
`internal/update/download.go`、`npm/lib/mirror.js`。

#### Scenario: 环境变量与显式配置的优先级

- **WHEN** 调用方传入 `Options.Mirrors` 且环境变量也设了
- **THEN** 以调用方传入的为准；环境变量只在未传时生效；都没有才用内置默认列表

#### Scenario: 「未设置」与「设为空」语义不同

- **WHEN** `REPLY9842_MIRROR` 完全未设置
- **THEN** 用内置默认列表（含直连兜底）

- **WHEN** `REPLY9842_MIRROR` 设为空串
- **THEN** 结果只有直连，**不回退**内置默认（显式设置即表达意图）

### Requirement: 断点续传

下载 SHALL 支持跨进程续传：`.part` 落盘、重试带 `Range` 头、正确处理 `206`/`200`/`416`。
`.part` 长度与服务端不符时必须**丢弃重来**，不能把中间片段接在尾部——那会产出内容错误
但大小正确的文件，只有 sha256 能发现。

落地证据：`internal/update/download.go`（`classifyResponse` 的
append / restart / refetch / complete 四分支）、`internal/update/download_test.go`。

#### Scenario: 坏 .part 不能永久卡死

- **WHEN** 本地 `.part` 是旧版本残留、长度大于服务端文件
- **THEN** 服务端返回 416 → 删除 `.part` 并立即重发一次无 `Range` 的请求

#### Scenario: 服务端不支持 Range

- **WHEN** 带 `Range` 请求却收到 `200 OK`（body 从 0 开始）
- **THEN** 截断 `.part` 从头写，不叠加

### Requirement: 强制 SHA256 校验

替换前 SHALL 用 release 的 `SHA256SUMS.txt` 校验下载产物。校验失败必须删除临时文件
并重试**下一个镜像**（镜像可能返回了错误内容，换一个正好能救），全部失败才整体报错。

落地证据：`internal/update/download.go`（`verifyChecksum` 逐镜像尝试）、
`internal/update/download_test.go`（`TestChecksumMismatchRetriesNextMirror`）。

#### Scenario: 镜像返回错误内容

- **WHEN** 首个镜像下回来的字节 sha256 不匹配
- **THEN** 删掉 `.part`，换下一个镜像重下；全部失败才报 `ErrChecksum` 并保留原二进制

### Requirement: 换前自检新二进制

替换前 SHALL 执行下载到的二进制并校验三件事：能启动（架构/动态库/执行位）、退出码 0
（半截 ELF 的 SEGV 在此暴露）、输出含产品标识 `reply9842`（拦住「下载到了别的可执行文件」，
`/bin/true` 也过不了）。任一条不满足则放弃替换。

落地证据：`internal/update/apply.go`（`probe`，`expectedWord` 判定）、
`cmd/gui/main.go`（`versionLine` 输出 `relay-gui <ver> (reply9842)`）。

#### Scenario: 产品标识缺失

- **WHEN** 新二进制 `--version` 输出 `relay-gui 0.2.1`（不含 `reply9842`）
- **THEN** 放弃替换并报「输出里没有产品标识」——**这条在真实端到端测试中抓到过一次**，
  若不修则每次更新都会在最后一步中止

#### Scenario: 半截二进制

- **WHEN** 下载到的 ELF 代码段被截断（传输中断）
- **THEN** 试跑时崩溃 → 自检失败 → 不替换，原二进制完好

### Requirement: 同目录原子替换并保留回滚备份

替换 SHALL 走「同目录 `.new` → `chmod 0755` → `rename` 覆盖」，并把旧二进制
`rename` 成 `.old-<版本>` 保留回滚。rename 失败时自动把备份换回去。权限不足直接报错，
**不尝试提权**（sudo 会留下 root 属主文件，用户下次普通权限更新就失败）。

落地证据：`internal/update/apply.go`（Apply / 备份命名 / 清理策略）。

#### Scenario: 跨文件系统

- **WHEN** 临时文件与目标不在同一文件系统
- **THEN** 不可能发生——`.new` 就建在目标同目录

#### Scenario: rename 覆盖失败

- **WHEN** 旧文件已改名成 `.old-x`，新文件 rename 上位失败
- **THEN** 把 `.old-x` 换回原名，恢复可用状态

### Requirement: 重启并恢复更新前的运行状态

更新完成后 SHALL 重启进程并恢复 relay 的运行状态：更新前在跑就自动再跑起来
（托盘常驻态用 `--autostart`、否则 `-start`），更新前是停止的就不能自己起来。
重启顺序必须保证单实例锁已释放——GUI 走 `mygo.App.Relaunch`（其 `finish()` 在
`OnQuit` 之后才 spawn，顺序被框架编码），而不是在 `BeforeSpawn` 里投递异步 `Quit`。

落地证据：`internal/update/restart.go`（Restart / RestartArgsFrom）、
`cmd/gui/update.go`（`restoreArgs`、`Relaunch` 调用点及注释）。

#### Scenario: 锁未释放导致假重启

- **WHEN** 旧进程还持有单实例锁时 spawn 新进程
- **THEN** 新进程只当转发器后退出（实测到的 FAKE RESTART）——所以必须等 `OnQuit` 之后

### Requirement: 设置页可开关、可手动检查、可配镜像

设置项 SHALL 包含：自动更新开关（默认**关闭**）、更新镜像输入框、检查更新按钮与状态行。
位于设置页「其他」分组，与「连接」分组同一张卡片内用组标题区隔（不拆卡，避免吃掉高度）。

落地证据：`cmd/gui/update.go`（三行 UI + 状态机）、
`internal/relayclient/settings.go`（`AutoUpdate` / `UpdateMirror` 字段）、
`cmd/gui/view_test.go`（`TestSettingsFitsShippedWindow` / `TestSettingsHasTwoGroups`）。

#### Scenario: 老配置文件缺字段

- **WHEN** settings.json 是加这两个字段之前写下的
- **THEN** 反序列化为零值（= 关闭），无需迁移代码

#### Scenario: 更新失败的可操作错误

- **WHEN** 下载失败 / 校验失败 / 权限不足
- **THEN** 分别给出不同文案：网络失败建议配镜像、校验失败提示可能被劫持、权限不足提示手动处理

### Requirement: macOS bundle 更新后不被回滚

在 macOS 上 GUI 会 re-exec 到 `~/Applications/reply9842.app/Contents/MacOS/reply9842`，
替换的是 bundle 内副本。更新后 SHALL 在 `Contents/` 写版本标记，使 bundle 重建逻辑
尊重它（标记比源新 → 副本赢），否则用户从裸二进制重启时会被拷回旧版、等于回滚一次。
同时刷新 `Info.plist` 的版本号（否则 Finder「简介」长期显示旧版本）。

落地证据：`cmd/gui/bundle_darwin.go`（`.reply9842-updated` 标记、
`updatedBundleBeats`、`refreshOwnPlist`）、`cmd/gui/update_test.go`
（`TestBundleStampBlocksRollback`）。

#### Scenario: 从裸二进制重启

- **WHEN** 更新完成后用户直接跑 `./gui`（裸二进制）
- **THEN** bundle 的 `copyIfChanged` 看到标记比源新，保留已更新的副本

#### Scenario: 开发者反复重建

- **WHEN** 源二进制比标记新（`go build` 迭代）
- **THEN** 照常覆盖，不卡住开发流程
