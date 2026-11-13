## Why

GUI 的 `logRing` 收集了最近 200 行运行日志，代码注释承诺「出问题时时能当场导出」：

```
cmd/gui/main.go:53   // 写到 stdout 的日志等于丢弃。留在内存里，出问题时时能当场导出。
```

但实际没有任何导出入口——`logRing.snapshot()` 定义了却**无任何调用方**：

```
rg -n 'logs\.snapshot|a\.logs\.' cmd/gui/*.go
→ 只有 cmd/gui/tray.go:113 的 a.logs.Write(...)
```

后果：Windows 用户（GUI 是 windowsgui 子系统、没有终端）出问题时**看不到任何日志**，
`logRing` 收集了却无法取出。这是「实现了采集、没实现出口」的半落地状态。

同时 `cmd/gui/view.go` 的 `noticeLine` 只显示 5 秒的操作提示（`noticeExp`），与真正的运行日志
是两件事，而运行日志在界面上完全不可见。

## What Changes

- 设置页（或「关于」区）新增「诊断」小节：展示最近 N 行日志（只读、可滚动、等宽字体），
  提供「复制全部」按钮（`mygo.Clipboard.WriteText`）。
- 可选：新增「导出日志文件…」保存到 `~/Desktop` 或用户选择的位置。若 mygo 没有文件选择
  对话框 API，则只做「复制到剪贴板」并在文档说明（不引入新依赖）。
- 日志行数上限保持 200（内存约 20 KB），不改成无限。
- `Runner` 的 `statsErr`（统计器打开失败的原因）也要出现在这个面板里——它当前也没有
  可见出口。

## Capabilities

### New Capabilities

无。

### Modified Capabilities

- `client-gui`: 追加「运行日志必须可从界面取出」需求。
- `observability`: 追加「GUI 侧日志的可观测出口」需求，移除「已知边界」里「无导出入口」
  的那一条。

## Impact

- 代码：`cmd/gui/view.go`（设置页新增诊断小节）、`cmd/gui/main.go`（`snapshot()` 接线 +
  复制动作）、`cmd/gui/view_test.go`（快照测试需覆盖新小节，注意窗口 510 高度是否够）。
- 无新增依赖（mygo 已有 Clipboard）。
- 窗口高度可能需要 +20~40px 或让设置页滚动区容纳新内容（设置 tab 已用 `ui.Scroll`，
  理论上无需加高）。
