# client-gui Specification

## Purpose

图形客户端 `relay-gui`：mygo native UI（Go 里写的界面、GPU 自绘），**托盘才是应用本体**，
窗口只是托盘随时可开关的一块面板。需求链原文（2026-11-13 kzf）：「mygo 开发极简客户端，
go install 即可用 CLI 或 GUI」「托盘才是本体，窗口可以随时关闭」「还需要实现开机自启」
「设置和主面板分开，用 tabs（顶部右上角）切换」「图标就用 9842 四个字符，复古字体，
硬朗效果」。

## Requirements

### Requirement: 托盘为应用本体，关窗不退出

进程生命周期 SHALL 由托盘管理：关闭窗口只是收起面板，只有托盘菜单「退出」才结束进程。
只有托盘创建失败时才退回「关窗即退出」——否则会留下既看不见也够不着的后台进程。

落地证据：`cmd/gui/main.go:304`（`OnWindowAllClosed`：`a.tray == nil` 时才 Quit）、
`cmd/gui/tray.go:90`（`initTray`）、`cmd/gui/tray.go:113`（托盘创建失败写日志）、
`cmd/gui/main.go:311`（`OnWillQuit` 收起托盘，避免留下点不动的幽灵图标）。

#### Scenario: 关窗后进程仍在

- **WHEN** 用户关闭窗口且托盘已创建成功
- **THEN** 进程继续运行、转发不停，点托盘图标或菜单「打开面板」可召回窗口

#### Scenario: 托盘不可用时不装死

- **WHEN** 托盘创建失败
- **THEN** 关窗即退出，不留下无法触达的进程

#### Scenario: 登录启动不弹窗

- **WHEN** 本次启动来自登录项
- **THEN** 只驻留托盘、不显示窗口；但托盘不可用时必须开窗（否则用户看不到任何东西）

### Requirement: 托盘菜单承载摘要与唯一退出路径

托盘菜单 SHALL 只有：打开面板 / 三条只读状态摘要 / 启动·停止 / 复制配置 / 退出。
三条状态行必须是**禁用菜单项**（macOS 会自动变灰且键盘导航跳过），不能用独立子菜单
或弹窗——那会把「看一眼」变成两步操作。

落地证据：`cmd/gui/tray.go:1`（菜单结构完整的 ASCII 说明与理由）、
`cmd/gui/tray.go:52`（菜单项常量）、`cmd/gui/tray.go:263`
（`tick` 用同一份 `snapshot()` 更新菜单文字，与窗口口径不漂移）。

#### Scenario: 托盘与面板数字一致

- **WHEN** 请求进行中同时看托盘摘要与窗口
- **THEN** 两者的请求数/压缩比/节省流量一致（同一心跳、同一快照来源）

#### Scenario: 退出只有一条路

- **WHEN** 用户想彻底结束程序
- **THEN** 必须走托盘「退出」；关窗不是退出

### Requirement: 一个窗口两个 tab（面板 / 设置）

界面 SHALL 为一个窗口两个 tab：面板（状态 + 主操作、上行压缩 hero、三格统计、复制配置）
与设置（上游 / 服务器域名 / 源地址 / 监听地址 / 传输方式 / 开机自启 / 日志保留 /
显示区间）。tabs 位于 titlebar 右上角，紧邻端口徽标左侧。

落地证据：`cmd/gui/view.go:35`（`view` 的布局注释）、
`cmd/gui/view.go:89`（`titleBar`）、`cmd/gui/view.go:115`（tabs 位置注释
「用户明确要求」）、`cmd/gui/view.go:643`（`settingsPane`）。

#### Scenario: 运行中连接设置不可改

- **WHEN** 正在运行时用户试图改动上游/监听/传输等连接类字段
- **THEN** 字段禁用，并显示「连接设置在运行中不可修改，先停止再改」+ 一个「停止」按钮

#### Scenario: 时间跨度随时可改

- **WHEN** 运行中修改「日志保留」或「显示区间」
- **THEN** 立刻生效，不要求先停止（它们不碰监听）

### Requirement: 空态不摆巨号、不摆空统计

空态 SHALL NOT 显示 68px 压缩比巨号，也 SHALL NOT 显示三格统计。理由：空态没有数字，
摆「—」是一根读作「坏掉的进度条」的粗横条，摆「0×」会被读成「压缩率是 0」；空卡占
25% 面积却零信息量。空态的信息由 hero 的一句话承担。

落地证据：`cmd/gui/view.go:10`（「空态的两条硬规则」注释）、
`cmd/gui/view.go:539`（`heroNote`：空态与运行态文案分叉）、
`cmd/gui/view.go:343`（`emptyChartNote`）。

#### Scenario: 首次打开窗口

- **WHEN** 尚未启动转发
- **THEN** 只看到状态行 + 一句引导文案，没有「0×」也没有空统计格

#### Scenario: 有数据后换成运行态

- **WHEN** 开始转发并有请求
- **THEN** hero 显示压缩比巨号，三格统计出现，文案改为「每 1,000 字节实际发多少」

### Requirement: solid-glass 材质用真模糊

界面 SHALL 用 mygo 的 `plugins/glass` 做真模糊（`glass.Blur`），不得用半透明纯色模拟。
模糊半径随层级变化（Panel 12 / Card 18 / Popover 24）。

落地证据：`internal/glass/glass.go:22`（为什么用真模糊的注释）、
`internal/glass/glass.go:260`（`BlurPanel/BlurCard/BlurPopover`）、
`internal/glass/glass.go:288`（`frost.PaintMaterial` 的 Blur → 色调两步）、
`internal/glass/glass.go:334`（`Surface`）、
`internal/glass/glass.go:122`（`Dark`）/ `:192`（`Light`）。

#### Scenario: 壁纸变化时卡片可读

- **WHEN** 桌面壁纸为浅色
- **THEN** 页面保持稳定深色底、卡片在其上方有厚度差，不出现「卡片比页面暗」的层深反转

#### Scenario: 亮色模式

- **WHEN** 用 `-light` 启动
- **THEN** 使用 `glass.Light()` 调色板，控件与 Surface 同步切换

### Requirement: 开机自启以操作系统状态为真相

自启开关 SHALL 每次读操作系统真实状态（macOS 读 LaunchAgent plist 并校验、Windows 走
HKCU Run），不得用自己缓存的 bool——用户可能在系统设置里改过，三处会互相说谎。
macOS 必须自建 `~/Library/LaunchAgents/dev.jixoai.reply9842.plist`（mygo 的
`SetOpenAtLogin` 走 SMAppService，要求 app bundle，裸二进制会被拒）。

落地证据：`internal/autostart/autostart.go:1`（「真相是操作系统状态」的理由）、
`internal/autostart/autostart.go:23`（`ID`）与 `:31`（`Marker`）、
`internal/autostart/darwin.go:1`（为什么不用 mygo 的 SetOpenAtLogin）、
`internal/autostart/darwin.go:56`（`open` 的四条读回校验）、
`internal/autostart/windows.go:1`（委托 mygo 写 HKCU Run）、
`cmd/gui/view.go:808`（`autoStartRow`）。

#### Scenario: 用户在系统设置里关掉后界面跟着关

- **WHEN** 用户在「系统设置 → 登录项」里删除本应用
- **THEN** 下次打开设置页时开关显示为关（读的是 plist 实际状态）

#### Scenario: 二进制被移动后读成关

- **WHEN** plist 里记录的路径已不是当前可执行文件
- **THEN** 读成关（而不是假装开着），用户重新打开开关即用新路径重写

#### Scenario: Linux 上不显示假开关

- **WHEN** 在 Linux 上（无 GUI 产物，仅示意）
- **THEN** `autostart.Available()` 为 false，设置页那行显示「当前平台不支持」

### Requirement: macOS 首次启动自建 app bundle

macOS 上 SHALL 在首次 GUI 启动时于 `~/Applications/reply9842.app` 现场拼一个最小 bundle
（当前二进制的副本 + Info.plist + 内嵌 AppIcon.icns），然后 re-exec 副本。目的只是 Dock
身份：裸 Mach-O 没有 bundle，Dock 里是通用黑色方块、进程名是文件名。产物仍是**一个
可执行文件**，bundle 是运行时在用户磁盘上生成的，不参与分发。

落地证据：`cmd/gui/bundle_darwin.go:48`（`relaunchIntoBundleIfNeeded`）、
`cmd/gui/bundle_darwin.go:75`（`ensureAppBundle`）、
`cmd/gui/bundle_darwin.go:35`（`bundleID = dev.jixoai.reply9842`）、
`cmd/gui/bundle_darwin.go:214`（`infoPlist`）、
`cmd/gui/bundle_darwin.go:40`（`RFC9842_BUNDLED=1` 跳过开关）。

#### Scenario: 重复启动不重写文件

- **WHEN** 第二次启动 GUI，二进制与图标都没变
- **THEN** 不重写 bundle 内任何文件（内容没变就跳过），正在运行的进程映像不被替换

#### Scenario: 已在 .app 内运行不再套娃

- **WHEN** 从已打包的 .app 内启动（路径含 `.app/Contents/MacOS/`）
- **THEN** 不再 relaunch，直接运行

#### Scenario: 想跑裸二进制

- **WHEN** 设置 `RFC9842_BUNDLED=1`
- **THEN** 跳过 bundle 逻辑，图标退化为通用样式但功能正常

### Requirement: Windows 不留控制台黑框

Windows 上 SHALL 保证双击不出现控制台窗口，两条机制并存互为兜底：Release 产物用
`-H=windowsgui` 链接；`go install` 无法传该 flag，故 `cmd/gui/console_windows.go` 在
`main()` 开头调用 `GetConsoleWindow` + `ShowWindow(SW_HIDE)` + `FreeConsole`。

落地证据：`cmd/gui/console_windows.go:1`（完整理由）、
`cmd/gui/main.go:154`（`hideConsole()` 调用）、
`Makefile` 的 `LDFLAGS_GUI_WIN`（`-H=windowsgui`）、
`.github/workflows/release.yml` 的 Self-check 步骤（断言 gui-windows 是 GUI 子系统）。

#### Scenario: go install 出来的 exe 也不弹黑框

- **WHEN** 用户 `go install .../cmd/gui@latest` 后双击 exe
- **THEN** 控制台窗口被隐藏并释放，只出现应用窗口

### Requirement: 单实例

GUI SHALL 只允许一个实例：第二次启动把 argv 交给第一个实例然后退出；再点一次图标
等于把已有窗口叫到前面，而不是起第二个服务抢端口。

落地证据：`cmd/gui/main.go:248`（`SetName` 必须在 `RequestSingleInstanceLock` 之前）、
`cmd/gui/main.go:254`（`RequestSingleInstanceLock`）、
`cmd/gui/main.go:259`（`OnSecondInstance` → `showWindow`）。

#### Scenario: 重复点击不双开

- **WHEN** 用户再次双击应用图标
- **THEN** 已有窗口被显示到前面，不产生第二个进程

## 已知边界

- **GUI 无 Linux 产物**：mygo native UI 的 Linux 后端依赖 GTK3，本项目只面向 macOS 与
  Windows；Linux 用 `relay-cli`。
- 启动日志留在内存环形缓冲（200 行），因为 Windows 上 GUI 没有终端，写 stdout 等于丢弃。
  当前**没有导出入口**（`logRing.snapshot()` 无调用方），见 changes/
  `add-gui-log-export`。
