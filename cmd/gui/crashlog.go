package main

// crashlog.go —— 让「进程整个消失」留下证据。
//
// 2026-11-14 实测：GUI 从 Finder 启动时，Go 的 panic / fatal error 写到 stderr
// 就没了——Dock 图标直接消失，System 的 DiagnosticReports 里也没有 .ips（Go
// 不是信号崩溃，运行时会自己打栈后 exit(2)）。结果就是用户只能说「闪退」，
// 我们这边零线索。
//
// 两条路都堵上：
//   - runtime 致命错误（concurrent map writes、栈溢出、fatal throw）走
//     debug.SetCrashOutput，Go 1.24+ 专门为此提供；
//   - 普通 panic 在 main 的 defer 里 recover，把栈写进同一个文件再退出。
//
// 文件固定为 <数据目录>/crash.log，追加写：一次会话一次崩溃最要紧的是「有」，
// 不需要每次一个新文件把目录塞满。

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"
)

// crashLogPath 崩溃日志的落盘位置。抽成函数是为了测试能在临时目录里验。
func crashLogPath(dir string) string { return filepath.Join(dir, "crash.log") }

// installCrashLog 把运行时致命错误重定向到崩溃日志。
//
// 返回的 *os.File 必须保持打开（SetCrashOutput 在崩溃时才写），所以这里不关。
func installCrashLog(dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(crashLogPath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	// 忽略返回值：拿不到这个能力也不该阻止应用启动。
	_ = debug.SetCrashOutput(f, debug.CrashOptions{})
}

// writeCrashReport 把一次普通 panic 追加到崩溃日志，并以时间戳开头。
//
// 返回写失败的错误仅仅是为了测试能断言；调用方（main 的 defer）在退出路径上，
// 不该因为日志写不进去再抛一次。
func writeCrashReport(dir string, r any, stack []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(crashLogPath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s panic: %v\n\n%s\n",
		time.Now().Format(time.RFC3339), r, stack)
	return err
}
