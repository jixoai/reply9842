//go:build !windows

package main

// hideConsole 在非 Windows 平台无事可做：macOS/Linux 的 GUI 进程本就没有
// 附带的控制台窗口。留着这个空实现是为了让 main 里的调用无条件成立。
func hideConsole() {}
