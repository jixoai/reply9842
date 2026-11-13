// relay-gui — rfc9842-reply 客户端的图形界面（mygo native UI + solid-glass 材质）
//
// 产品形态（2026-11-13 kzf 需求「托盘才是本体，窗口可以随时关闭」）：
//
//	托盘图标 = 应用的本体，常驻菜单栏 / 通知区域
//	窗口     = 托盘随时可开关的一块面板
//
// 所以进程生命周期归托盘管：关窗只是收起面板，「退出」才结束进程。只有托盘
// 创建失败时才退回「关窗即退出」——否则会留下一个既看不见也够不着的后台进程。
//
// 界面（一个窗口，两个 tab）：
//
//	面板  状态 + 主操作、上行压缩 hero、三格统计、复制配置
//	设置  上游 / 监听 / 传输方式 / 开机自启
//
// 视觉照 OpenDesign 的设计稿转写成 native 绘制，材质见 internal/glass。
//
// 三个关键取舍：
//   - 压缩比 68px 巨号是运行态的主角；空态**不摆巨号**，只留一句引导——
//     空态显示「0×」会让用户以为压缩率是 0，摆个破折号又像坏掉的进度条
//   - 玻璃用 mygo 的 plugins/glass 做真模糊，不是画个半透明色块充数
//   - 每条信息只出现一次：状态在状态行、版本在 footer、端口在 titlebar
//
// 刻意不做 hosts 劫持与配置文件改写：两者都需要提权或改动用户既有配置，
// 收益（省一步手工）与风险（全局副作用、格式易碎）不成比例。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/jixoai/rfc9842-reply/internal/autostart"
	"github.com/jixoai/rfc9842-reply/internal/glass"
	"github.com/jixoai/rfc9842-reply/internal/relayclient"
	"github.com/jixoai/rfc9842-reply/internal/stats"
)

// version 由 -ldflags "-X main.version=..." 注入
var version = "dev"

// logRing 把 Runner 的运行日志留在内存里。
//
// 为什么不用标准输出：GUI 在 Windows 上是 windowsgui 子系统，没有终端，
// 写到 stdout 的日志等于丢弃。留在内存里，出问题时时能当场导出。
type logRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newLogRing(max int) *logRing { return &logRing{max: max} }

func (l *logRing) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		l.lines = append(l.lines, line)
		if len(l.lines) > l.max {
			l.lines = l.lines[len(l.lines)-l.max:]
		}
	}
	return len(p), nil
}

func (l *logRing) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.lines))
	copy(out, l.lines)
	return out
}

// app 界面状态。所有字段只在主线程（视图里、托盘回调里）读写。
//
// 这条约束是硬性的：mygo 的 native UI 在 main 线程构建视图，托盘回调也在
// main 线程；跨 goroutine 只能经 mygo.RunOnMain 进入。
type app struct {
	runner *relayclient.Runner
	logs   *logRing
	win    *mygo.Window
	tray   *trayUI

	// 配置区（输入框直接绑这些字段）
	listen     string
	server     string
	transport  string
	serverName string
	insecure   bool

	// tab 当前页（0=面板 1=设置），与 titleBar 里 tab 的顺序一致
	tab int
	// autoStart 开机自启。它不是「我们的偏好」，而是 OS 登录项状态的镜像：
	// 每次切换都写 OS 再读回，界面只显示 OS 的实际状态。
	autoStart bool

	// 最近一次统计快照（每秒刷新）
	agg stats.Agg
	// snap 统计来源。正常运行时就是 runner.Snapshot；测试与自动化截图
	// 需要在不启监听的前提下喂数据，所以留一个可替换的接口（nil 时走 Runner）。
	snap func() stats.Agg
	// fakeRun / fakeErr 是给测试与自动化截图用的状态注入（nil / nil 时走 Runner）。
	// 生产路径永远不设这两个字段，所以它们不影响真实行为。
	fakeRun *bool
	fakeErr error

	// quitting 已进入退出流程。托盘/Dock 的「显示窗口」在退出途中不该再建窗。
	quitting bool

	// history 上一次会话的统计（从 client-stats.json 读回）。
	//
	// 用途只有一个：空态时告诉回访用户「上次效果如何」。新用户看不到这一行
	// （history 为 nil），看到的是「本次会话还没有请求」。
	history *stats.Agg

	// 一次性操作反馈（复制成功/失败），与启动失败的错误分开显示，
	// 避免同一个原因在状态区与提示行各喊一遍
	notice    string
	noticeErr bool
	noticeExp time.Time

	// 材质色板（深色 = solid-glass 的设计原型）
	pal *glass.Palette
}

func main() {
	// Windows：go install 出来的 exe 是 console 子系统，先隐藏黑框（release
	// 构建另有 -H=windowsgui，两套都兜住）
	hideConsole()

	logRing := newLogRing(200)
	logger := log.New(logRing, "", log.LstdFlags)

	// 登录项标记必须赶在 flag.Parse 之前摘掉：--autostart 不是用户开关，
	// 而是 launchd 告诉我们「这次是登录启动」；交给 flag 解析会变成 unknown flag。
	openedAtLogin := takeLoginMarker()

	// 允许用 flag 覆盖初值：多实例并存（换端口）、自动化截图都需要
	// 「不改界面就能换参数」。不带参数时仍用 relayclient 的默认值。
	cfg := relayclient.Config{}
	cfg.Normalize()
	printVersion := flag.Bool("version", false, "打印版本后退出")
	listen := flag.String("listen", cfg.Listen, "监听地址初值")
	server := flag.String("server", cfg.Server, "上游地址初值")
	transport := flag.String("transport", cfg.Transport, "传输方式初值: auto|quic|tcp")
	serverName := flag.String("server-name", cfg.ServerName,
		"TLS SNI；上游用域名证书时必须显式指定，留空则按上游地址推导（自签/回环场景要 -insecure）")
	insecure := flag.Bool("insecure", cfg.Insecure, "跳过服务端证书校验（自签证书或回环调试）")
	startNow := flag.Bool("start", false, "打开窗口并立即开始转发")
	// 旧名字保留为别名：INSTALL.md 与老脚本里写的是 -autostart（当时的含义是
	// 「启动后立即转发」）。登录项用的是双横线 --autostart，两者互不干扰。
	startAlias := flag.Bool("autostart", false, "与 -start 相同（旧名字，保留兼容）")
	light := flag.Bool("light", false, "使用亮色玻璃（默认深色）")
	flag.Parse()
	if *printVersion {
		fmt.Printf("relay-gui %s\n", version)
		return
	}
	// macOS：裸二进制没有 bundle，Dock 只会显示通用黑色方块。首次 GUI 启动时
	// 在 ~/Applications/rfc9842-reply.app 拼出 bundle（二进制副本 + Info.plist
	// + 内嵌 icns），再 re-exec 副本取回图标与进程名。返回 true 表示子进程已起，
	// 本进程应当退出。其他平台是空实现。
	//
	// 位置很关键：要在 flag.Parse 之后（--version 这类纯命令行用法不该产生
	// 副作用）、建窗口之前（窗口一旦创建就绑定了当前进程身份）。
	if relaunchIntoBundleIfNeeded(version) {
		return
	}
	cfg.Listen, cfg.Server, cfg.Transport = *listen, *server, *transport
	cfg.ServerName, cfg.Insecure = *serverName, *insecure

	pal := glass.Dark()
	if *light {
		pal = glass.Light()
	}

	a := &app{
		logs:       logRing,
		listen:     cfg.Listen,
		server:     cfg.Server,
		transport:  cfg.Transport,
		serverName: cfg.ServerName,
		insecure:   cfg.Insecure,
		pal:        pal,
	}
	a.runner = relayclient.New(cfg)
	a.runner.SetLogger(logger)
	a.runner.OnEvent = a.onRunnerEvent
	// 上次会话的成绩：只有「本次会话还没有请求」时才会显示，用来回答
	// 回访用户最关心的问题（上次到底省了多少）。
	if prev, err := stats.ReadAgg(cfg.StatsPath); err == nil && prev != nil && prev.Requests > 0 {
		a.history = prev
	}
	// 开机自启的初值来自 OS（不是我们的配置）：用户可能在系统设置里改过
	if autostart.Available() {
		if on, err := autostart.Open(); err == nil {
			a.autoStart = on
		}
	}

	// SetName 必须早于 RequestSingleInstanceLock：单实例锁、Windows 的 Run
	// 值、用户数据目录都按这个名字派生，改名的代价是全部错位。
	mygo.App.SetName("rfc9842-reply")

	// 单实例：第二次启动把 argv 交给第一个实例然后退出。
	// 于是「再点一次图标」= 把已有窗口叫到前面，而不是起第二个服务抢端口。
	if !mygo.App.RequestSingleInstanceLock() {
		return
	}

	// 第二个实例 → 显示已有窗口（它可能已被收进托盘）
	mygo.App.OnSecondInstance(func([]string, string) { a.showWindow() })

	// 点 Dock 图标 → 显示窗口（macOS 的常规期待）
	mygo.App.OnActivate(func(hasVisible bool) {
		if !hasVisible {
			a.showWindow()
		}
	})

	mygo.App.WhenReady(func() {
		login := openedAtLogin || mygo.App.WasOpenedAtLogin()
		a.initTray()
		// 登录启动只驻留托盘：用户刚登录，不该被一个窗口糊脸。
		// 但托盘没建起来时必须开窗——否则这是个既看不见也够不着的进程。
		if !login || a.tray == nil {
			a.showWindow()
		}
		if login {
			if a.tray == nil {
				logger.Printf("托盘不可用，改为显示窗口")
			} else {
				logger.Printf("登录启动：只驻留托盘")
			}
		}
		// 登录启动的意义就是立刻开始工作；-start 是同样的语义，只是要看窗口。
		if login || *startNow || *startAlias {
			a.start()
		}
	})

	// 退出流程开始：先标记，挡住退出途中冒出来的「显示窗口」
	mygo.App.OnBeforeQuit(func(e *mygo.QuitEvent) {
		a.quitting = true
		// 系统要关机/注销：从这里返回后进程可能立刻没了，别做 5 秒排空
		// （那会把 logout 拖住）。监听端口随进程一起消失，无需善后。
		if e.SessionEnding {
			return
		}
		if a.runner.Running() {
			a.runner.Stop(context.Background())
		}
	})

	// 关窗不退出：托盘还在。只有托盘建不起来时，关窗才等于退出——否则
	// 用户再也找不到这个进程。
	mygo.App.OnWindowAllClosed(func() {
		if a.tray == nil {
			mygo.App.Quit()
		}
	})

	// 事件循环停止前收起托盘图标（留着会变成一个点不动的幽灵图标）
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) {
		if a.tray != nil {
			a.tray.stopTicker()
			a.tray.destroy()
			a.tray = nil
		}
	})

	if err := mygo.App.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "mygo:", err)
		os.Exit(1)
	}
}

// takeLoginMarker 从 os.Args 里摘掉登录项标记，并报告本次是否来自登录启动。
//
// 只认双横线的 `--autostart`：它与用户可见的 `-autostart`（"启动后立即转发"
// 的旧开关，保留为 -start 的别名）必须区分开，否则老用户敲 `-autostart`
// 会发现窗口不弹出来。
//
// 允许它出现在任意位置：launchd 写在第二位，但用户完全可能手工再加别的参数。
func takeLoginMarker() bool {
	found := false
	args := os.Args[:1]
	for _, arg := range os.Args[1:] {
		if arg == autostart.Marker {
			found = true
			continue
		}
		args = append(args, arg)
	}
	os.Args = args
	return found
}

// ---- 状态读取（界面与托盘共用同一处口径）----

// statusKind 状态的语义分类。颜色由界面映射；托盘菜单项没法上色，用 ● 字符占位。
type statusKind int

const (
	stateStopped statusKind = iota // 用户自己的选择，不是错误
	stateRunning                   // 正常运行
	stateFailed                    // 确实出错
)

// state 状态三元组（语义 / 大字 / 说明）。
//
// 三种语义分开，不把「没启动」画成「故障」：未启动用灰不用红。
func (a *app) state() (kind statusKind, label, detail string) {
	switch {
	case a.running():
		// 运行时长不放进这一行：它和上游地址叠起来会顶到主操作按钮上。
		// 时长只在 footer 出现一次。
		return stateRunning, "已连接", "上游 " + a.server
	case a.lastErr() != nil:
		// 启动失败：只说一句人话（原始错误前缀叠了三层，见 humanError）
		return stateFailed, "启动失败", humanError(a.lastErr())
	default:
		return stateStopped, "已停止", "本地流量不经过压缩"
	}
}

// snapshot 当前统计快照（可被测试替换）
func (a *app) snapshot() stats.Agg {
	if a.snap != nil {
		return a.snap()
	}
	return a.runner.Snapshot()
}

// running 是否在运行（可被测试注入）
func (a *app) running() bool {
	if a.fakeRun != nil {
		return *a.fakeRun
	}
	return a.runner.Running()
}

// lastErr 最近一次启动错误（可被测试注入）
func (a *app) lastErr() error {
	if a.fakeErr != nil {
		return a.fakeErr
	}
	return a.runner.Err()
}

// ---- 窗口 ----

// showWindow 显示面板：没有就建，有就还原并提到前面。
//
// 用「销毁 + 重建」而不是隐藏：托盘的常驻进程里，窗口是可有可无的视图，
// 重建出来的是一份干净状态；而真正需要活着的状态（runner、统计、配置）
// 都在 app 上，不在窗口里。
//
// 为什么需要 RunOnMain：托盘菜单回调虽是主线程，但 OnSecondInstance 与
// OnActivate 由 mygo 经 postMain 投递，稳妥起见统一走过主线程入口。
func (a *app) showWindow() {
	mygo.RunOnMain(func() {
		if a.quitting {
			return
		}
		if a.win != nil {
			// Show 不会把最小化的窗口还原，必须先 Restore
			if a.win.IsMinimized() {
				a.win.Restore()
			}
			a.win.Show()
			a.win.Focus()
			mygo.App.Focus()
			return
		}
		a.createWindow()
	})
}

// createWindow 建面板窗口。主线程调用。
func (a *app) createWindow() {
	win := mygo.NewWindow(mygo.WindowOptions{
		Title: "rfc9842-reply",
		// 480×400。实测（TestSnapshotFrames 的 480×480 帧 + 元素矩形）：
		// 面板 tab 运行态到「复制」底边约 355px，空态约 235px，设置 tab
		// （含运行中的提示行）约 290px。400 让三种状态都一屏放下，且空态
		// 剩下的空白读作留白；再高就会在空态露出一大片空。
		Width:  480,
		Height: 400,
		// 窗口可以拉大，但别小到把状态行与主按钮挤成两行
		MinWidth:  440,
		MinHeight: 340,
		// 记住位置/尺寸（含最大化），下次从托盘打开时还原。
		// 键名固定，改它等于丢掉用户摆好的位置。
		StateKey: "main",
		// 自绘 titlebar：系统标题栏是一条灰带，和设计稿的窗口不是一回事。
		// HiddenInset 保留 macOS 的红绿灯（traffic lights），
		// 让它压在自绘 titlebar 上；其他平台等同 Hidden。
		TitleBarStyle: mygo.TitleBarHiddenInset,
		// 桌面透进来，玻璃才有真实内容可模糊（见 internal/glass 包注释）。
		// 不开这项，页面底板虽是半透明，背后仍是窗口的纯色，blur 等于白做。
		Vibrancy: mygo.VibrancyUnderWindow,
		Content:  ui.View(a.view),
	})
	win.OnClosed(func() {
		// 关窗只是收起面板：清掉指针，下次从托盘打开时重建一个干净的。
		if a.win == win {
			a.win = nil
		}
	})
	a.win = win
}

func (a *app) invalidate() {
	if a.win != nil {
		a.win.Invalidate()
	}
}

// onRunnerEvent Runner 状态变化（启动/停止/出错）。
//
// Runner 在自己的 goroutine 里回调，所以必须切到主线程：它会碰窗口与托盘指针。
func (a *app) onRunnerEvent() {
	mygo.RunOnMain(func() {
		if a.tray != nil {
			a.tray.refresh()
		}
		a.invalidate()
	})
}

// ---- 动作 ----

func (a *app) setNotice(msg string, isErr bool) {
	a.notice, a.noticeErr = msg, isErr
	a.noticeExp = time.Now().Add(5 * time.Second)
}

// localURL 本机 relay 的 base URL（把 0.0.0.0 / :port 归一成 127.0.0.1）
func (a *app) localURL() string {
	addr := a.listen
	if strings.HasPrefix(addr, "0.0.0.0:") || strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1:" + addr[strings.LastIndex(addr, ":")+1:]
	}
	return "http://" + addr
}

func (a *app) start() {
	cfg := a.runner.Config()
	cfg.Listen = strings.TrimSpace(a.listen)
	cfg.Server = strings.TrimSpace(a.server)
	cfg.Transport = a.transport
	cfg.ServerName = strings.TrimSpace(a.serverName)
	cfg.Insecure = a.insecure
	if err := a.runner.SetConfig(cfg); err != nil {
		a.setNotice(humanError(err), true)
		return
	}
	if err := a.runner.Start(); err != nil {
		// 不再额外弹一条「启动失败」：状态行已经把「启动失败 + 原因」
		// 常驻显示出来了，同一件事说两遍只会让用户怀疑自己漏看了什么。
		return
	}
	a.setNotice("已启动，监听 "+a.runner.Addr(), false)
}

func (a *app) stop() {
	if err := a.runner.Stop(context.Background()); err != nil {
		a.setNotice(humanError(err), true)
		return
	}
	a.setNotice("已停止", false)
}

// toggle 启动 / 停止（托盘菜单与主按钮共用同一条路径）
func (a *app) toggle() {
	if a.running() {
		a.stop()
	} else {
		a.start()
	}
}

// copyEnv 复制 Agent 需要的环境变量
func (a *app) copyEnv() {
	line := "export ANTHROPIC_BASE_URL=" + a.localURL()
	mygo.Clipboard.WriteText(line)
	a.setNotice("已复制："+line, false)
}

// applyAutostart 把开关写进操作系统，再把 OS 的真实状态读回来。
//
// 「写后读回」不是多余的保险：开关的真相在 OS，写失败、写了一半、
// 被系统策略拒绝，都会让我们以为开了其实没开。只有读回成功才更新界面。
func (a *app) applyAutostart() {
	want := a.autoStart
	if err := autostart.Set(want); err != nil {
		a.autoStart = !want
		a.setNotice("开机自启设置失败："+err.Error(), true)
		return
	}
	got, err := autostart.Open()
	if err != nil {
		a.autoStart = !want
		a.setNotice("开机自启读回失败："+err.Error(), true)
		return
	}
	a.autoStart = got
	if got {
		a.setNotice("已开启开机自启（下次登录生效）", false)
	} else {
		a.setNotice("已关闭开机自启", false)
	}
}
