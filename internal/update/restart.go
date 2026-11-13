// 重启：spawn 新进程后让调用方退出。
//
// 关键约束是**单实例锁**（cmd/gui/main.go 用 mygo 的 RequestSingleInstanceLock）。
// 读了 mygo 的实现（single_instance.go）后确认了具体的失败形态：
//
//	新进程启动 → 连上旧进程的 socket → 把 argv 交给旧进程 → 自己 return false 退出
//
// 也就是说「先 spawn 再退出」的朴素顺序在 GUI 上**一定不会重启**，而是变成
// 「把窗口叫到前面」。mygo 只重试 5 次、每次退避 20ms（合计约 300ms），远少于
// 一个 GUI 进程完整退出的时间，所以也不能指望新进程自己等。
//
// 因此 Restart 提供 BeforeSpawn 钩子：调用方在这里释放锁（GUI 侧是
// mygo.App.Quit() 或等价的锁释放），再 spawn，最后退出。
//
// 这里不 import mygo：本包不该依赖 UI 框架，而且 CLI/server 根本没有这个锁。
package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrNotExecutable 表示要重启的目标不可执行。
var ErrNotExecutable = errors.New("update: 目标不可执行")

// RestartOptions 控制新进程的启动方式。零值 = 完全继承当前进程的
// stdio / env / cwd，这是「重启看起来像没发生过」的默认语义。
type RestartOptions struct {
	// Env 是新进程的环境变量。nil = 继承 os.Environ()。
	Env []string
	// Dir 是新进程的工作目录。空串 = 继承当前目录。
	//
	// 继承是刻意的：CLI 的相对路径参数（-cert ./cert.pem）在重启后必须仍然有效。
	Dir string
	// Stdin/Stdout/Stderr 为 nil 时继承当前进程的对应流。
	//
	// 显式声明这三个字段是为了测试能捕获新进程输出；生产路径全 nil 就够。
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Detach 让新进程脱离当前进程的会话/进程组（Unix: setsid，Windows:
	// CREATE_NEW_PROCESS_GROUP）。
	//
	// GUI 需要它：否则关掉启动它的终端（或 IDE）会给整个进程组发 SIGHUP，
	// 刚重启的应用跟着死。CLI 不需要（用户就在那个终端里，希望 Ctrl-C 生效）。
	Detach bool
	// BeforeSpawn 在 spawn **之前**调用，用于释放单实例锁。
	//
	// 返回错误则中止，不 spawn——锁没释放就 spawn 只会得到一个「转发 argv
	// 然后退出」的假重启，不如直接报错让用户手工重启。
	BeforeSpawn func() error
	// WaitForExit 非 0 时，spawn 后等待新进程退出最多这么久再返回。
	//
	// 用途是测试（要看到子进程的输出与退出码）；生产路径应为 0——
	// 应用重启不该让父进程陪着等。
	WaitForExit time.Duration
}

// Restart 启动 exePath 的新实例。
//
// 返回后调用方应当立即退出当前进程（GUI 走 mygo 的 Quit，CLI 走 return）。
// 本函数**不会**自己退出进程：那会让调用方失去清理机会（关日志、存设置），
// 也让单元测试无法运行。
//
// 顺序（每一步的理由）：
//
//  1. 校验目标可执行——在锁释放之前做，失败时什么都没改变
//  2. BeforeSpawn 释放单实例锁——必须早于 spawn（见文件头）
//  3. spawn，stdio/env/cwd 按 opts 继承
//  4. 立刻返回，不 Wait（Wait 会让旧进程占着锁与端口）
func Restart(exePath string, args []string, opts RestartOptions) error {
	target, err := resolveTarget(exePath)
	if err != nil {
		return err
	}
	st, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("update: 重启目标不存在 %s: %w", target, err)
	}
	if st.IsDir() {
		return fmt.Errorf("%w：%s 是目录", ErrNotExecutable, target)
	}
	// Unix 上没执行位就一定起不来。Windows 没有执行位概念（os.Chmod 只切
	// 只读位），跳过错判。
	if runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
		return fmt.Errorf("%w：%s 缺少执行权限（mode=%s）", ErrNotExecutable, target, st.Mode())
	}

	if opts.BeforeSpawn != nil {
		if err := opts.BeforeSpawn(); err != nil {
			return fmt.Errorf("update: 重启前释放资源失败（未启动新进程）: %w", err)
		}
	}

	cmd := exec.Command(target, args...)
	if opts.Env != nil {
		cmd.Env = opts.Env
	} else {
		cmd.Env = os.Environ()
	}
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	} else if wd, err := os.Getwd(); err == nil {
		// 显式设置而不是留空：留空时 exec 在子进程里再 getcwd，而那时当前
		// 目录可能已被删除（用户从临时目录启动过）。提前解析能失败在明处。
		cmd.Dir = wd
	}
	cmd.Stdin = opts.Stdin
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	if opts.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	cmd.SysProcAttr = restartProcAttr(opts.Detach)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("update: 启动新进程 %s: %w", target, err)
	}

	if opts.WaitForExit > 0 {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case werr := <-done:
			if werr != nil {
				return fmt.Errorf("update: 新进程退出异常: %w", werr)
			}
		case <-time.After(opts.WaitForExit):
			// 超时不算失败：新进程可能正常常驻（GUI 就该一直跑）。
			// 不 kill 它——它是用户的应用，杀掉比留着危险。
			return nil
		}
		return nil
	}

	// 不等 Wait，但必须放掉 Process 句柄（Unix 上无影响；Windows 上不 Release
	// 会留下一个句柄）。Release 不影响子进程继续运行。
	_ = cmd.Process.Release()
	return nil
}

// RestartArgsFrom 返回「重放当前进程」应当传给新实例的参数。
//
// 默认就是 os.Args[1:]：命令行参数（-listen/-server/-light）是用户意图，
// 重启必须保留。过滤留给调用方——例如去掉一次性的 `--updated` 标记。
//
// exclude 的三条匹配规则（都是为了让「过滤一个 flag」不留下残骸）：
//
//	`--flag`        精确匹配整个参数
//	`--flag=value`  只比较等号前的部分
//	`--flag value`  若被排除的 flag 后面紧跟一个非 flag 参数，那个值也一并丢掉
//
// 第三条很关键：只删 `-listen` 而留下 `127.0.0.1:1234`，新进程会把它当成
// 位置参数——在 flag 库里通常是静默忽略，于是用户改的监听地址凭空消失。
// 代价是「`--flag` 后面恰好跟着一个本应保留的位置参数」时会被误删，
// 但本项目没有位置参数（cmd/*/main.go 全用 flag），这个折中是安全的。
func RestartArgsFrom(exclude ...string) []string {
	raw := os.Args[1:]
	if len(exclude) == 0 {
		return append([]string(nil), raw...)
	}
	skip := make(map[string]struct{}, len(exclude))
	for _, e := range exclude {
		skip[e] = struct{}{}
	}
	out := make([]string, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		a := raw[i]
		// 支持 `--flag=value` 形态的排除：只比较等号前的部分。
		key := a
		if j := strings.IndexByte(a, '='); j > 0 {
			key = a[:j]
		}
		if _, drop := skip[key]; !drop {
			out = append(out, a)
			continue
		}
		// 被排除的是 `--flag`（无 =），且下一项不像 flag → 那是它的值，一起丢。
		if key == a && i+1 < len(raw) && !strings.HasPrefix(raw[i+1], "-") {
			i++
		}
	}
	return out
}

// ExecutableName 返回目标文件名，供日志与错误信息使用（避免打印长路径）。
func ExecutableName(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Base(path)
}
