//go:build windows

// Windows 的登录项：交给 mygo 的 HKCU Run 条目。
//
// 这里刻意只有两行委托——mygo v0.9.0 已经把 Windows 侧的所有细节做对了：
//   - 值名用 App.Name()（main 里 SetName("rfc9842-reply")），写在
//     HKCU\Software\Microsoft\Windows\CurrentVersion\Run 下
//   - 数据是 `"<绝对路径>" --mygo-opened-at-login`（路径经过符号链接解析并加引号）
//   - OpenAtLogin 只有在该值与当前可执行文件一致、且 StartupApproved 没标记为
//     禁用时才为 true（用户在任务管理器里关掉 → 我们读成 off）
//   - 关闭时同时删除 Run 值与 StartupApproved 值（清掉上一次的禁用记录）
//   - --mygo-opened-at-login 由 mygo 在包 init 阶段从 os.Args 剥掉，
//     mygo.App.WasOpenedAtLogin() 报告它
//
// 自己重写这套只会引入不一致（尤其是 StartupApproved 那个字节），没有收益。
package autostart

import "github.com/egoist/mygo"

const supported = true

func open() (bool, error) { return mygo.App.OpenAtLogin(), nil }

func set(on bool) error { return mygo.App.SetOpenAtLogin(on) }
