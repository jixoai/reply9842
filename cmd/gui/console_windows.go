//go:build windows

// Windows 隐藏控制台窗口。
//
// 为什么需要这个文件：`go install ...@latest` 无法传 -ldflags "-H=windowsgui"，
// 编出来的 exe 是 console 子系统——双击就会先弹一个黑框再开窗。这里在 main 开头
// 主动把那个控制台隐藏并释放掉，行为与 -H windowsgui 链接的产物一致。
//
// 只用 syscall（kernel32/user32 的公开导出），不引入 CGO，也不额外增加依赖。
package main

import "syscall"

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	user32            = syscall.NewLazyDLL("user32.dll")
	procFreeConsole   = kernel32.NewProc("FreeConsole")
	procGetConsoleWin = kernel32.NewProc("GetConsoleWindow")
	procShowWindow    = user32.NewProc("ShowWindow")
)

// hideConsole 隐藏并释放本进程附带的控制台窗口。没有控制台时是无害的 no-op。
func hideConsole() {
	if h, _, _ := procGetConsoleWin.Call(); h != 0 {
		const swHide = 0
		procShowWindow.Call(h, swHide)
	}
	procFreeConsole.Call()
}
