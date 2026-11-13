//go:build !windows

package update

import "syscall"

// restartProcAttr 让新进程脱离当前会话（仅当 detach 为真）。
//
// Setsid 的作用：新进程不继承旧进程的控制终端，于是旧进程退出时终端发送的
// SIGHUP 不会连坐。GUI 从终端/IDE 启动时这是必需的——否则「更新后重启」
// 会在用户关掉那个终端时跟着死。
//
// 不 detach 时返回 nil：CLI 用户就在前台，Ctrl-C 应该同时结束服务与
// 本转发进程，这是他们的期待。
func restartProcAttr(detach bool) *syscall.SysProcAttr {
	if !detach {
		return nil
	}
	return &syscall.SysProcAttr{Setsid: true}
}
