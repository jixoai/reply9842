//go:build windows

package update

import "syscall"

// restartProcAttr 在 Windows 上把新进程放进独立进程组（仅当 detach 为真）。
//
// CREATE_NEW_PROCESS_GROUP 的实际收益：控制台 Ctrl-C / Ctrl-Break 只送往
// 新进程组而非整个控制台会话，于是「关掉启动它的 cmd 窗口」不会连带杀掉
// 刚重启的 GUI。真正的 setsid 在 Windows 上没有等价物（进程组不脱离会话），
// 但 GUI 的 windowsgui 子系统本来就不挂控制台，实际影响很小。
//
// 刻意**不**加 DETACHED_PROCESS/CREATE_NO_WINDOW：那会让 CLI/server 的
// 子进程失去 stdout，重启后的日志直接消失——比"可能被 Ctrl-C 带走"更糟。
func restartProcAttr(detach bool) *syscall.SysProcAttr {
	if !detach {
		return nil
	}
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
