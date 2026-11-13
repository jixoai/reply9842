// relay-gui — reply9842 客户端的图形界面（mygo native UI + solid-glass 材质）
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
//	面板  上行压缩图、流量与延迟、统计摘要
//	设置  上游 / 监听 / 传输方式 / 开机自启
//
// 启动/停止与连接状态都在自绘标题栏上（图标开关），不在面板里重复一遍。
//
// 视觉照 OpenDesign 的设计稿转写成 native 绘制，材质见 internal/glass。
//
// 三个关键取舍：
//   - 压缩比 68px 巨号是运行态的主角；空态**不摆巨号**，只留一句引导——
//     空态显示「0×」会让用户以为压缩率是 0，摆个破折号又像坏掉的进度条
//   - 玻璃用 mygo 的 plugins/glass 做真模糊，不是画个半透明色块充数
//   - 每条信息只出现一次：状态在顶栏开关、版本在 footer、监听地址在顶栏徽标
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
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"

	"github.com/jixoai/reply9842/internal/autostart"
	"github.com/jixoai/reply9842/internal/glass"
	"github.com/jixoai/reply9842/internal/relayclient"
	"github.com/jixoai/reply9842/internal/stats"
	"github.com/jixoai/reply9842/internal/update"
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
	// localAddr 绑定出站源地址。用户开代理 TUN 全局模式时，隧道流量会被
	// 导进 utun 导致 TLS 握手被切断（表现为 tcp tls: EOF）；填物理网卡 IP
	// 可让内核直接走该网卡，不必改代理配置。
	localAddr string
	insecure  bool

	// tab 当前页（0=面板 1=设置），与 titleBar 里 tab 的顺序一致
	tab int
	// autoStart 开机自启。它不是「我们的偏好」，而是 OS 登录项状态的镜像：
	// 每次切换都写 OS 再读回，界面只显示 OS 的实际状态。
	autoStart bool

	// 最近一次统计快照（每秒刷新）。
	//
	// 这份数据的事实来源是磁盘日志（stats 包在 Open 时流式重建），所以
	// 「重启后数字还在」不需要 GUI 做任何恢复动作——它只是把一个已经算好的
	// 视图读出来。
	agg stats.Agg
	// snap 统计来源。正常运行时就是 runner.Snapshot；测试与自动化截图
	// 需要在不启监听的前提下喂数据，所以留一个可替换的接口（nil 时走 Runner）。
	snap func() stats.Agg
	// fakeRun / fakeErr 是给测试与自动化截图用的状态注入（nil / nil 时走 Runner）。
	// 生产路径永远不设这两个字段，所以它们不影响真实行为。
	fakeRun *bool
	fakeErr error

	// 两个时间跨度偏好（持久化在 settings.json，重启后保持）。
	//
	// 用「档位下标」而不是裸 Duration 当界面状态：下拉框的可选项是固定的
	// 6 档（relayclient.RetainOptions / WindowOptions），下标是唯一能无歧义
	// 表达「用户选了哪一项」的形式，也让「吸附到最近档位」变成一次比较。
	retainIdx int
	windowIdx int
	// settingsPath 偏好文件路径（测试可换到临时目录）
	settingsPath string
	// settingsErr 上次保存偏好失败的原因（nil 表示正常）
	settingsErr error

	// 自动更新偏好（持久化在 settings.json，默认关闭——自我替换有风险，
	// 必须由用户显式打开）。
	autoUpdate   bool
	updateMirror string

	// upd 更新会话的可见状态（阶段 / 进度 / 结果文案）。只在主线程读写。
	//
	// 与 runner 的状态分开：更新可以随时开始，与「relay 是否在跑」无关，
	// 而它的生命周期（下载几分钟、替换几秒）也完全不同于每秒刷新的统计。
	upd updateSession

	// skipAutoUpdate 表示本次启动不做自动检查。
	//
	// 由自动更新自己重启时注入（一个仅本次有效的一次性标记）：新版起来后若
	// 又查到同一个版本（替换没生效的病理情况），没有这个闸门就会无限重启。
	skipAutoUpdate bool

	// ---- 更新流程的注入点（测试与自动化用；生产路径永远为 nil）----
	//
	// 与 a.snap / a.fakeRun 同一套做法。不加这些钩子，「检查 → 下载 → 替换 →
	// 重启」整条链路就只能靠真下载真替换来验证——那会毁掉跑测试的机器。
	asyncFn    func(func())
	mainFn     func(func())
	checkFn    func(context.Context, update.Options) (update.Info, error)
	downloadFn func(context.Context, update.Options, update.Info) (string, error)
	applyFn    func(update.Options, string, string) (string, error)
	selfPathFn func() (string, error)
	relaunchFn func([]string)

	// quitting 已进入退出流程。托盘/Dock 的「显示窗口」在退出途中不该再建窗。
	quitting bool

	// 一次性操作反馈（启动/停止结果、保存失败），与顶栏开关的状态分开显示，
	// 避免同一个原因在开关与提示行各喊一遍
	notice    string
	noticeErr bool
	noticeExp time.Time

	// copiedAt 最近一次「复制地址」成功的时刻（零值 = 不在回显窗口里）。
	// 只有顶栏徽标读它：底栏不再有第二个复制入口，回显只在一处发生。
	copiedAt time.Time

	// 材质色板（深色 = solid-glass 的设计原型）
	pal *glass.Palette
}

func main() {
	// 越早装越好：初始化阶段也可能 panic。
	installCrashLog(relayclient.DefaultDir())
	// 普通 panic（非 runtime fatal）在这里落盘后再退出。没有它，从 Finder
	// 启动时整个进程会「无声消失」——用户只能报告「闪退」。
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			fmt.Fprintf(os.Stderr, "panic: %v\n%s", r, stack)
			_ = writeCrashReport(relayclient.DefaultDir(), r, stack)
			// 用非零退出码：macOS 登录项的 KeepAlive 是 SuccessfulExit=false，
			// 只有非零退出才会被 launchd 拉起来（settings.json 的 running 会让
			// 拉起后的进程自己恢复隧道）。recover 之后正常 return 等于 exit 0，
			// 那就不重启了——自恢复链条在这里断掉。
			os.Exit(2)
		}
	}()
	// Windows：go install 出来的 exe 是 console 子系统，先隐藏黑框（release
	// 构建另有 -H=windowsgui，两套都兜住）
	hideConsole()

	logRing := newLogRing(200)
	logger := log.New(logRing, "", log.LstdFlags)

	// 登录项标记必须赶在 flag.Parse 之前摘掉：--autostart 不是用户开关，
	// 而是 launchd 告诉我们「这次是登录启动」；交给 flag 解析会变成 unknown flag。
	openedAtLogin := loginAtLaunch()

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
	localAddr := flag.String("local-addr", cfg.LocalAddr,
		"绑定出站源地址（如 192.168.1.106）；用于绕过代理 TUN 全局模式对隧道的劫持")
	startNow := flag.Bool("start", false, "打开窗口并立即开始转发")
	// 旧名字保留为别名：INSTALL.md 与老脚本里写的是 -autostart（当时的含义是
	// 「启动后立即转发」）。登录项用的是双横线 --autostart，两者互不干扰。
	startAlias := flag.Bool("autostart", false, "与 -start 相同（旧名字，保留兼容）")
	light := flag.Bool("light", false, "使用亮色玻璃（默认深色）")
	// 自动更新自己重启时注入的一次性标记：本次启动不再检查更新。
	//
	// 不用环境变量（bundle 的 re-exec 会转发 argv，但 launchd 启动不会），
	// 也不用持久状态：它要表达的只是「这一次」。把同一个 flag 从 argv 里摘掉
	// 由 update.RestartArgsFrom 的 exclude 完成（见 restoreArgs 的调用方）。
	noAutoUpdate := flag.Bool("no-auto-update", false, "本次启动不检查更新（自动更新重启时内部使用）")
	// 时间跨度覆盖（自动化截图、临时放大窗口用）。0 = 用 settings.json 里的值；
	// 时间跨度覆盖（自动化截图、临时放大窗口用）。0 = 用 settings.json 里的值。
	//
	// 非 0 时按「最近档位」吸附（手敲 40m 不会造出第 7 档），且**只对本次运行生效**：
	// 不写回 settings.json——截图脚本不该顺手改掉用户真实的设置页选择。
	retain := flag.Duration("retain", 0, "本次运行的日志保留时长（覆盖设置页：30m|1h|6h|24h|168h|720h）")
	window := flag.Duration("window", 0, "本次运行的图表显示区间（覆盖设置页：1m|5m|30m|1h|6h|24h）")
	flag.Parse()
	if *printVersion {
		fmt.Println(versionLine(version))
		return
	}
	// macOS：裸二进制没有 bundle，Dock 只会显示通用黑色方块。首次 GUI 启动时
	// 在 ~/Applications/reply9842.app 拼出 bundle（二进制副本 + Info.plist
	// + 内嵌 icns），再 re-exec 副本取回图标与进程名。返回 true 表示子进程已起，
	// 本进程应当退出。其他平台是空实现。
	//
	// 位置很关键：要在 flag.Parse 之后（--version 这类纯命令行用法不该产生
	// 副作用）、建窗口之前（窗口一旦创建就绑定了当前进程身份）。
	if relaunchIntoBundleIfNeeded(version) {
		return
	}
	// Windows 的延迟替换：上次更新因目标被占用而降级成「待应用」时，在这里补完。
	//
	// 位置与理由（与 bundle re-exec 同一类约束）：
	//   - 必须在 flag.Parse 之后（--version 不该有副作用）
	//   - 必须在跑在 bundle 副本之后（要替换的是「下次启动会执行的那个文件」，
	//     macOS 上它是副本）
	//   - 必须远早于建窗口：替换完成后内存里跑的还是旧映像，必须立刻重新 exec，
	//     否则用户看到的版本号与磁盘不一致，且下次替换会撞同一个问题
	//
	// 只在 Windows 上真的可能返回 applied=true；其它平台这个调用是一次 stat。
	if applied, err := update.ApplyPendingIfAny(selfPathOrEmpty()); err != nil {
		logRing.Write([]byte("待应用的更新未能完成：" + err.Error() + "\n"))
	} else if applied {
		// 替换已经完成 → 立刻以新映像重启。这一步不能省：内存里跑的还是旧映像，
		// 不重启会让用户看到的版本号与磁盘不一致，且下次替换撞同一个问题。
		//
		// 此刻还**没有**取单实例锁（那在下面），所以直接 spawn 是安全的，
		// 不需要先释放锁——这正是它必须放在 RequestSingleInstanceLock 之前的原因。
		logRing.Write([]byte("已应用待替换的更新，正在重启\n"))
		if err := spawnSelfRestarted(); err != nil {
			logRing.Write([]byte("重启失败，请手工启动：" + err.Error() + "\n"))
		}
		return
	}
	cfg.Listen, cfg.Server, cfg.Transport = *listen, *server, *transport
	cfg.ServerName, cfg.Insecure = *serverName, *insecure
	cfg.LocalAddr = *localAddr
	// 默认值快照：用来判断某个 flag 是「用户真的指定了」还是「没动」。
	defCfg := relayclient.Config{}
	defCfg.Normalize()

	pal := glass.Dark()
	if *light {
		pal = glass.Light()
	}

	// 两个时间跨度偏好：settings.json 是唯一信源（flag 只用于自动化/多实例）。
	// 读不到就用默认值——偏好读不出来是可恢复降级，不该让 GUI 起不来。
	settingsPath := relayclient.SettingsPath()
	st := relayclient.LoadSettings(settingsPath)
	if *retain != 0 {
		st.RetainSec = int64(relayclient.RetainOptions[relayclient.OptionIndex(*retain, relayclient.RetainOptions)] / time.Second)
	}
	if *window != 0 {
		st.WindowSec = int64(relayclient.WindowOptions[relayclient.OptionIndex(*window, relayclient.WindowOptions)] / time.Second)
	}
	st.Normalize()
	cfg.Retain, cfg.Window = st.Retain(), st.Window()

	// 连接参数：settings.json 也要能记住它们。
	//
	// 背景（2026-11-14）：用户机器开着 TUN 全局代理，隧道服务器 IP 被导进
	// utun（route get 47.82.76.102 → 198.18.0.1 / utun5），必须把「源地址」
	// 绑到物理网卡才能连上。这些值是 flag-only 的，GUI 重启就回默认值 ——
	// 表现就是「更新到 0.3 之后连不上了」。现在写进配置，且 flag 显式给了
	// 值（≠ 默认值）时仍然以 flag 为准。
	pick := func(flagVal, defVal, saved, cur string) string {
		if flagVal != defVal {
			return cur // 命令行显式指定，尊重它
		}
		if saved != "" {
			return saved
		}
		return cur
	}
	cfg.Listen = pick(*listen, defCfg.Listen, st.Listen, cfg.Listen)
	cfg.Server = pick(*server, defCfg.Server, st.Server, cfg.Server)
	cfg.ServerName = pick(*serverName, defCfg.ServerName, st.ServerName, cfg.ServerName)
	cfg.LocalAddr = pick(*localAddr, defCfg.LocalAddr, st.LocalAddr, cfg.LocalAddr)
	cfg.Transport = pick(*transport, defCfg.Transport, st.Transport, cfg.Transport)

	a := &app{
		logs:           logRing,
		listen:         cfg.Listen,
		server:         cfg.Server,
		transport:      cfg.Transport,
		serverName:     cfg.ServerName,
		localAddr:      cfg.LocalAddr,
		insecure:       cfg.Insecure,
		pal:            pal,
		retainIdx:      relayclient.OptionIndex(st.Retain(), relayclient.RetainOptions),
		windowIdx:      relayclient.OptionIndex(st.Window(), relayclient.WindowOptions),
		settingsPath:   settingsPath,
		autoUpdate:     st.AutoUpdate,
		updateMirror:   st.UpdateMirror,
		skipAutoUpdate: *noAutoUpdate,
	}
	a.runner = relayclient.New(cfg)
	a.runner.SetLogger(logger)
	a.runner.OnEvent = a.onRunnerEvent
	// 开机自启的初值来自 OS（不是我们的配置）：用户可能在系统设置里改过
	if autostart.Available() {
		if on, err := autostart.Open(); err == nil {
			a.autoStart = on
		}
	}

	// SetName 必须早于 RequestSingleInstanceLock：单实例锁、Windows 的 Run
	// 值、用户数据目录都按这个名字派生，改名的代价是全部错位。
	mygo.App.SetName("reply9842")

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
		if shouldStartOnLaunch(login, *startNow || *startAlias, st) {
			a.start()
		}
		// 自动更新：只在用户打开开关、且不是被自己重启起来的这次启动时检查。
		//
		// 放在 WhenReady 的最后：检查要发网络请求，而窗口与托盘应该先出现——
		// 让用户先看到界面，而不是盯着一个空窗口等 GitHub 的响应。
		a.maybeAutoUpdate()
	})

	// 退出流程开始：先标记，挡住退出途中冒出来的「显示窗口」
	mygo.App.OnBeforeQuit(func(e *mygo.QuitEvent) {
		a.quitting = true
		// 更新正在进行时退出：立刻取消后台下载/校验。
		//
		// 这个顺序不能反：替换（Apply）是原子 rename，被中断只会留下 .new 与
		// 备份，目标文件完好；而下载的 ctx 若不取消，进程退出会留下一个半截
		// .part（下次能续传，但白占磁盘）。所以在关监听之前先停下载。
		a.cancelUpdateQuietly()
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

// versionLine 是 `--version` 输出的那一行。
//
// 必须含产品名 `reply9842`：自动更新的自检（internal/update 的 selfCheck）
// 用它拦「下载到了别的产品的二进制」——只验退出码的话，一个被替换成 /bin/true
// 的产物也会通过。
//
// 这里曾经只打 `relay-gui <ver>`，于是**每一次真实更新都会在替换前中止**
// （「新二进制自检失败：输出里没有产品标识」）。这是端到端实测抓到的：
// 真下载 v0.2.1 的产物 → 沙箱 Apply → ErrSelfCheck。
//
// 保留 relay-gui 前缀（INSTALL.md 与用户既有习惯），把产品名放进括号：
// 一行输出同时满足「人读得懂」与「机器认得出」。抽成函数是为了让测试断言的是
// 这一段真实代码，而不是在测试里重抄一遍格式串。
func versionLine(v string) string {
	return fmt.Sprintf("relay-gui %s (reply9842)", v)
}

// envLogin 把「本次是登录启动」这个事实传给 bundle 副本。
//
// 为什么必须用环境变量：macOS 上裸二进制会先 relaunch 进 ~/Applications 的
// .app（见 bundle_darwin.go），而那次 re-exec 只转发 os.Args——argv 里的
// --autostart 已经被 takeLoginMarker 摘掉了。结果就是「登录启动」这个事实在
// 交接时丢失，副本以为自己是普通启动，照样弹出一个窗口（实测如此）。
// 环境变量随 exec 继承，正好补上这条缝。
const envLogin = "RFC9842_OPENED_AT_LOGIN"

// loginAtLaunch 报告本次是否由登录项拉起（argv 或环境变量任一为真）。
func loginAtLaunch() bool {
	if os.Getenv(envLogin) == "1" {
		return true
	}
	return takeLoginMarker()
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
	if found {
		// 传下去给可能的 bundle 副本
		os.Setenv(envLogin, "1")
	}
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

// 出货窗口尺寸。改这里就够了：测试与官网截图都读这两个常量，
// 散落的 560/660 字面量出现过一次「改了窗口、测试还在量旧尺寸」的事故。
const (
	defaultWinWidth  = 580
	defaultWinHeight = 640
)

// createWindow 建面板窗口。主线程调用。
func (a *app) createWindow() {
	win := mygo.NewWindow(mygo.WindowOptions{
		Title: "reply9842",
		// 尺寸演进：480×496 → 560×660 → **580×640**（2026-11-14 owner 定稿）。
		//
		// 宽度 580：两张图的刻度列 + 绘图区在 480 宽时只剩约 370，48 根柱子每根
		// 不到 8px，堆叠柱会糊成一片；580 让单柱约 10px，也摆脱了「手机比例」。
		//
		// 高度 640：不是拍的，是量出来的（ui.Tester 逐元素取底边）。设置页最坏的
		// 组合是「更新失败两行 + 保存失败两行」，此时内容底边约 611——640 减去
		// 顶栏 48 与底栏 33 后仍放得下。这条约束由 TestSettingsFitsShippedWindow
		// 守着：它按出货尺寸渲染最坏组合，断言每一行都在 footer 之上。
		// 窗口再矮就交给设置页自己滚（比切掉一行好）；MinHeight=400 是硬下界。
		//
		// 面板 tab 的两张图等高（各 Grow(1)），窗口拉高时两张一起变高。
		Width:  defaultWinWidth,
		Height: defaultWinHeight,
		// 窗口可以拉大，但别小到把顶栏右侧那一簇挤成两行（顶栏靠收起应用名
		// 兜到 468，见 titleBar 的 showName）。
		MinWidth: 440,
		// 太矮时设置 tab 会滚（比切开好），但别矮到连顶栏都放不下
		MinHeight: 400,
		// 记住位置/尺寸（含最大化），下次从托盘打开时还原。
		// 键名固定，改它等于丢掉用户摆好的位置。
		StateKey: "main",
		// 自绘 titlebar：系统标题栏是一条灰带，和设计稿的窗口不是一回事。
		// HiddenInset 保留 macOS 的红绿灯（traffic lights），
		// 让它压在自绘 titlebar 上；其他平台等同 Hidden。
		TitleBarStyle: mygo.TitleBarHiddenInset,
		// 红绿灯的位置：不指定时 AppKit 把它们放在顶边 19 处，而 hiddenInset
		// 窗口会带一条 66 高的 NSToolbar 撑出来的标题区——灯因此落在自绘
		// 顶栏（48 高）的上半截，视觉上偏上 7pt（真机截图实测：灯心 26，
		// 顶栏中线 24，而灯本应在 24）。
		//
		// 指定 y=17 让 14 高的灯占据 17..31、灯心正好 24 = 顶栏中线，同时
		// （19, 17）也基本是 macOS 自己的默认位置，不会显得「灯被挪走了」。
		// 见 view.go 的 titleBarHeight。
		TrafficLightPosition: &mygo.Point{X: 19, Y: 17},
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
	cfg.LocalAddr = strings.TrimSpace(a.localAddr)
	cfg.Insecure = a.insecure
	if err := a.runner.SetConfig(cfg); err != nil {
		a.setNotice(humanError(err), true)
		return
	}
	if err := a.runner.Start(); err != nil {
		// 不再额外弹一条「启动失败」：面板的 failureLine 已经把「启动失败 + 原因」
		// 常驻显示出来了，同一件事说两遍只会让用户怀疑自己漏看了什么。
		return
	}
	// 把「正在运行」持久化：下次启动（含登录项、崩溃后重开）应当自动接着跑。
	a.persistRunState()
	// 启动成功不弹提示：顶栏开关整个变成绿色停止块、footer 出现「运行中 00:02」、
	// 徽标换成真实端口——三处同时变，比一行会推挤布局的提示条说得更清楚。
}

func (a *app) stop() {
	if err := a.runner.Stop(context.Background()); err != nil {
		a.setNotice(humanError(err), true)
	}
	// 手动停止同样要落盘：用户明确说过「停」，重启后就不该自己跑起来。
	a.persistRunState()
	// 同上：停止成功由开关与 footer 表达，不需要额外一行字。
}

// persistRunState 把当前 Runner 的运行状态写进设置文件。
//
// 写盘失败不弹提示：这是「记住状态」的附带动作，失败也只影响下次启动的默认值，
// 不该打断正在启动/停止的主流程。错误留在 settingsErr 里，设置页会显示。
func (a *app) persistRunState() {
	_ = a.saveSettings()
}

// shouldStartOnLaunch 决定这次启动要不要立刻开始转发。
//
// 四个来源，优先级从「显式」到「继承」：
//   - login：登录项启动，进系统就是为了接着跑
//   - flagStart：命令行显式 -start / -autostart
//   - running：上次退出时的状态（用户手动停过就是 false，不许自动复活）
func shouldStartOnLaunch(login, flagStart bool, st relayclient.Settings) bool {
	return login || flagStart || st.Running
}

// toggle 启动 / 停止（托盘菜单与主按钮共用同一条路径）
func (a *app) toggle() {
	if a.running() {
		a.stop()
	} else {
		a.start()
	}
}

// copyURL 把本机 relay 的 HTTP 地址放进剪贴板。
//
// 只复制裸 URL：这个代理是通用的 HTTP 转发口，不绑定任何一家 SDK 的环境变量
// 名字。上一版复制的是 `export ANTHROPIC_BASE_URL=...`，那等于替用户决定了
// 用什么客户端——而同一个地址 OpenAI 兼容客户端、curl、浏览器都能用。
//
// 两条写入口（c 非 nil 走视图、nil 走托盘）：
//
//	c != nil   ui.Context.WriteClipboardThen —— 框架的正路，写完异步回调，
//	           无头测试里也走同一份实现（ui.Tester 自带剪贴板）
//	c == nil   托盘菜单回调没有 Context，直接 mygo.Clipboard
//
// 不用 mygo.Clipboard 统一处理：它要求应用事件循环已在跑，在视图里调用会在
// 测试环境下卡死（onMain 没人泵）。
//
// 复制成功才置闪光：写剪贴板可能因为别的进程占着而失败，装成功会让用户以为
// 贴出来的是新地址。失败走 noticeLine 说清。
func (a *app) copyURL(c *ui.Context) {
	url := a.localURL()
	done := func(err error) {
		if err != nil {
			a.setNotice("复制失败："+err.Error(), true)
			a.invalidate()
			return
		}
		a.setNotice("已复制 "+url, false)
		a.copiedAt = time.Now()
		a.invalidate()
	}
	if c != nil {
		c.WriteClipboardThen(url, done)
		return
	}
	if err := mygo.Clipboard.Write(transfer.TextData(url)); err != nil {
		done(err)
		return
	}
	done(nil)
}

// copyFlash 复制成功的回显时长。
//
// 1.6 秒：短到不会赖在界面上，长到能覆盖「点击 → 视线移到目标 → 意识到成功」
// 这一整段。0.6 秒那种一闪，用户经常连自己点没点中都判断不了。
const copyFlash = 1600 * time.Millisecond

// copyFlashing 是否还在复制成功的回显窗口里（顶栏徽标读它）。
//
// 视图每秒重建一次（心跳），所以不需要额外的计时器去清它：时间一过，
// 下一个心跳帧自然就恢复原样了。
func (a *app) copyFlashing() bool {
	return !a.copiedAt.IsZero() && time.Since(a.copiedAt) < copyFlash
}

// currentSettings 把 app 上的全部偏好收成一个 Settings（写盘与「写后读回」共用）。
//
// 存在的理由：偏好现在有 4 个来源不同的状态（两个下拉的档位下标、一个开关、
// 一个文本框），任何一处漏写都会在下次启动时悄悄退回默认值——那类 bug 用户
// 只会描述成「设置没保存」。
func (a *app) currentSettings() relayclient.Settings {
	retain := relayclient.RetainOptions[clampIndex(a.retainIdx, len(relayclient.RetainOptions))]
	window := relayclient.WindowOptions[clampIndex(a.windowIdx, len(relayclient.WindowOptions))]
	return relayclient.Settings{
		RetainSec:    int64(retain / time.Second),
		WindowSec:    int64(window / time.Second),
		AutoUpdate:   a.autoUpdate,
		UpdateMirror: strings.TrimSpace(a.updateMirror),
		Server:       strings.TrimSpace(a.server),
		ServerName:   strings.TrimSpace(a.serverName),
		Listen:       strings.TrimSpace(a.listen),
		LocalAddr:    strings.TrimSpace(a.localAddr),
		Transport:    a.transport,
		// 运行态由 Runner 的**实际**状态给出，不是某个「想让它跑」的标记：
		// 启动失败时不能让文件写成 true，否则下次启动会再失败一次。
		Running: a.running(),
	}
}

// saveSettings 落盘（失败记在 settingsErr 并返回，调用方决定怎么提示）。
func (a *app) saveSettings() error {
	a.settingsErr = relayclient.SaveSettings(a.settingsPath, a.currentSettings())
	return a.settingsErr
}

// applyTimeSettings 把两个下拉的选择写盘并立刻生效。
//
// 顺序：先收敛约束 → 写文件 → 通知 Runner 重算。
// 为什么写盘失败也要继续重算：设置在本进程内已经生效，只是下次启动会退回
// 旧值；把「这次也白点了」一起惩罚用户没有意义。失败用提示行说明。
//
// 约束在这里集中处理（而不是在下拉的行里）：save/reload 也走这条路径，
// 任何入口都必须得到同一个合法组合。
func (a *app) applyTimeSettings() {
	retain := relayclient.RetainOptions[clampIndex(a.retainIdx, len(relayclient.RetainOptions))]
	window := relayclient.WindowOptions[clampIndex(a.windowIdx, len(relayclient.WindowOptions))]

	// 硬约束：显示区间不超过保留时长。超了就把显示区间压到保留时长所在的档位，
	// 并同步下拉的选中项——否则界面上那个选项看起来是「选中的但没生效」。
	if window > retain {
		window = retain
		a.windowIdx = relayclient.OptionIndex(window, relayclient.WindowOptions)
		a.setNotice("显示区间不能超过日志保留时长（"+humanWindow(int(retain/time.Second))+"），已按上限设置", true)
	}

	// 两个时间跨度与更新偏好共用一份文件，所以这里也必须把更新字段一并写进去
	// （currentSettings 已包含它们）——否则调一次下拉就会把开关状态冲掉。
	if err := a.saveSettings(); err != nil {
		a.setNotice("保存设置失败："+a.settingsErr.Error(), true)
	}

	if err := a.runner.SetStatsSettings(retain, window); err != nil {
		a.setNotice("统计重建失败："+err.Error(), true)
		return
	}
	a.invalidate()
}

// clampIndex 把下标夹进 [0, n)，防越界（下拉项被裁剪时不至于 panic）
func clampIndex(i, n int) int {
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
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
