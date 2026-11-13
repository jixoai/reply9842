// tray.go — 托盘图标与菜单（应用的本体）
//
// 产品决定（2026-11-13 kzf）：「托盘才是本体，窗口可以随时关闭」。所以这里
// 承担三件事，缺一件产品形态就不成立：
//
//  1. 让进程在关窗后继续活着（配合 main 里的 OnWindowAllClosed）
//  2. 提供回到窗口的入口（菜单「打开面板」/ 左键点图标）
//  3. 承载「退出」——这是唯一真正的结束路径
//
// 菜单结构（刻意只有这几项，做减法）：
//
//	打开面板
//	──────
//	● 已连接 · 127.0.0.1:19010   （禁用行）
//	1,204 请求 · 20.0× 压缩       （禁用行）
//	节省 8.2 MB                   （禁用行）
//	──────
//	启动 / 停止
//	──────
//	复制 ANTHROPIC_BASE_URL
//	──────
//	退出 reply9842
//
// 三条状态行是**禁用**的菜单项而不是 disabled 菜单：macOS 的禁用项会自动
// 变灰、也不会在键盘导航里被选中，正好是「只读摘要」该有的行为。用独立
// 子菜单或弹窗展示统计会让「看一眼」变成两步操作。
package main

import (
	_ "embed"
	"time"

	"github.com/egoist/mygo"

	"github.com/jixoai/reply9842/internal/autostart"
	"github.com/jixoai/reply9842/internal/stats"
)

// trayIcon 是打包好的模板图标（纯黑 + 透明 + 2×2「9842」）。
//
// 为什么 go:embed 而不是读磁盘：分发产物只有一个可执行文件，资源必须内置。
// 为什么是 48px 而不是 16/32：菜单栏图标 18pt（Retina 36px），Windows 通知
// 区域按 DPI 取 16/24/32；48 是唯一一档能在全部 DPI 下干净降采样的尺寸，
// mygo 会按高度等比缩放到 18pt。
//
//go:embed assets/tray.png
var trayIcon []byte

// trayItem 状态行与动作行的 ID（更新菜单时要按键找人）
const (
	itemOpen   = "open"
	itemState  = "state"
	itemStats1 = "stats.requests"
	itemStats2 = "stats.saved"
	itemToggle = "toggle"
	itemCopy   = "copy"
	itemQuit   = "quit"
)

// trayUI 托盘图标的运行期状态。
type trayUI struct {
	a    *app
	tray *mygo.Tray

	// 三个状态行的菜单项指针：每秒只改它们的文字，不重建整个菜单。
	//
	// 为什么不 SetMenu(全新菜单)：重建会（1）在 macOS 上让正在弹出的菜单
	// 闪烁/丢失高亮，（2）丢掉用户用手指在菜单里移动的位置。改 Label 是原地
	// 更新，系统只重绘那一行。
	stateItem  *mygo.MenuItem
	statsItem1 *mygo.MenuItem
	statsItem2 *mygo.MenuItem
	toggleItem *mygo.MenuItem
	autoStart  *mygo.MenuItem

	// 最近一次写入菜单的文案。相同就不动——每秒一次的无谓 setLabel 会让
	// AppKit 反复重排菜单项宽度。
	lastState, lastStats1, lastStats2, lastToggle string

	// title 已经写进菜单栏的那段文字（macOS 的 SetTitle）
	lastTitle string

	stopCh chan struct{}
}

// initTray 建托盘。必须在 WhenReady 里调用：非就绪状态的主线程调 NewTray 会 panic。
//
// 失败不致命：返回 nil 时 main 会退回「关窗即退出」的行为（否则会留下一个
// 既看不见也够不着的后台进程）。
func (a *app) initTray() {
	t := &trayUI{a: a}
	menu := t.buildMenu()

	tray, err := mygo.NewTray(mygo.TrayOptions{
		// ID 让 macOS 记住用户 ⌘ 拖拽的位置，让 Windows 记住是放在任务栏
		// 还是收在溢出区。改 ID 等于让用户重摆一次位置。
		ID: "main",
		// 模板图标：只有 alpha 有意义，系统按菜单栏明暗把整张图染成黑或白。
		// 原彩色图标（深灰底 + 暖白字）在深色菜单栏上会变成一块白色方块，
		// 比旁边的系统图标重得多；模板图标则与它们同一视觉重量。
		Icon:           trayIcon,
		IconIsTemplate: true,
		// macOS 的 Title 显示在图标右侧。这里放压缩比——托盘常驻时它是
		// 唯一「一眼可见」的成效指标，比图标本身更有信息量。
		// 初值留空：还没数据时显示「0×」是误导，等有数了再出现。
		Title:   "",
		ToolTip: "reply9842 · 按内容寻址压缩上行带宽",
		Menu:    menu,
	})
	if err != nil {
		// 记日志而不是弹窗：托盘不可用时用户仍在窗口里，下次真的需要
		// 关窗时 OnWindowAllClosed 会按 tray == nil 退出。
		a.logs.Write([]byte("托盘创建失败：" + err.Error() + "\n"))
		return
	}

	t.tray = tray
	// 左键点图标：macOS 上托盘有菜单时点图标直接弹菜单（系统行为），
	// 这里注册 OnClick 只影响「SetMenu(nil)」的情况，但保留它让
	// 「菜单被临时移除」时也不会点不动。
	tray.OnClick(a.showWindow)
	a.tray = t

	// 首帧就对齐真实状态，不必等用户先点开菜单
	t.refresh()

	// 每秒刷新：菜单里的状态与统计要跟窗口同步。
	// 用 time.Ticker 而不是 ui.Context.After —— 后者只在窗口存在时才会跑，
	// 而托盘正是「窗口关了也要更新」的那个东西。
	t.startTicker()
}

// buildMenu 建托盘菜单。只在初始化时调一次；之后用 refresh 原地改文字。
func (t *trayUI) buildMenu() *mygo.Menu {
	a := t.a

	t.stateItem = &mygo.MenuItem{ID: itemState, Label: "—", Disabled: true}
	t.statsItem1 = &mygo.MenuItem{ID: itemStats1, Label: "—", Disabled: true}
	t.statsItem2 = &mygo.MenuItem{ID: itemStats2, Label: "—", Disabled: true, Hidden: true}
	t.toggleItem = &mygo.MenuItem{ID: itemToggle, Label: "启动", Click: func(*mygo.MenuItem, *mygo.Window) {
		a.toggle()
	}}

	items := []*mygo.MenuItem{
		{ID: itemOpen, Label: "打开面板", Click: func(*mygo.MenuItem, *mygo.Window) {
			a.showWindow()
		}},
		mygo.Separator(),
		t.stateItem,
		t.statsItem1,
		t.statsItem2,
		mygo.Separator(),
		t.toggleItem,
		mygo.Separator(),
		{ID: itemCopy, Label: "复制 ANTHROPIC_BASE_URL", Click: func(*mygo.MenuItem, *mygo.Window) {
			a.copyEnv()
		}},
	}

	// 开机自启：托盘里也给一个入口。用户「退出」过一次之后，最可能想改的
	// 就是「下次别再自动起来」，让他在托盘就地能关，比先打开面板再翻设置近一步。
	//
	// 只在平台支持时出现：Linux 上显示一个恒灰的项只会让人以为坏了。
	if autostart.Available() {
		t.autoStart = &mygo.MenuItem{
			ID:      "autostart",
			Label:   "开机自启",
			Type:    mygo.MenuItemCheckbox,
			Checked: a.autoStart,
			// mygo 在调用 Click **之前**已经把 Checked 翻成用户想要的新状态
			Click: func(it *mygo.MenuItem, _ *mygo.Window) {
				want := it.Checked
				// 走和设置页同一条路径：写 OS → 读回 → 以 OS 为准
				a.autoStart = want
				a.applyAutostart()
				// 把控件调回 OS 的真实状态（例如写失败时它可能没变）
				it.SetChecked(a.autoStart)
				// 设置 tab 若开着，开关也要跟着动
				a.invalidate()
			},
		}
		items = append(items,
			mygo.Separator(),
			t.autoStart,
		)
	}

	items = append(items,
		mygo.Separator(),
		&mygo.MenuItem{ID: itemQuit, Label: "退出 reply9842", Click: func(*mygo.MenuItem, *mygo.Window) {
			// 唯一的真退出路径。Quit 会在主线程跑完 OnBeforeQuit（停监听）
			// 与 OnWillQuit（收托盘），然后结束事件循环。
			mygo.App.Quit()
		}},
	)
	return mygo.NewMenu(items)
}

// startTicker 每秒刷一次菜单文字。
//
// 为什么是独立 goroutine 而不是挂在视图的 After 上：窗口关掉后视图不跑，
// 但托盘里「已连接 · 1,204 请求」仍然必须继续走。这正是「托盘是本体」的
// 具体含义——它不能依赖窗口存在。
func (t *trayUI) startTicker() {
	t.stopCh = make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-t.stopCh:
				return
			case <-ticker.C:
				// 菜单项的文字只能在主线程改（AppKit/Shell 都是单线程的）
				mygo.RunOnMain(t.refresh)
			}
		}
	}()
}

// stopTicker 停心跳。
//
// 不能只把 chan 置 nil：goroutine 会一直等在那个 chan 上，进程虽会随那次
// 退出结束，但若是「托盘重建」这种未来场景就会泄漏。close 是明确的一次性信号。
func (t *trayUI) stopTicker() {
	if t.stopCh != nil {
		close(t.stopCh)
		t.stopCh = nil
	}
}

// destroy 收图标
func (t *trayUI) destroy() {
	t.stopTicker()
	if t.tray != nil {
		t.tray.Destroy()
		t.tray = nil
	}
}

// refresh 把当前状态写进菜单。主线程调用。
//
// 每一项都做「值没变就不写」的比较：每秒一次的 setLabel 会让 macOS 重新
// 测量菜单项宽度，菜单正开着时用户会看到文字轻微抖动。
func (t *trayUI) refresh() {
	a := t.a
	if t.tray == nil {
		return
	}

	kind, label, detail := a.state()
	dot := "○"
	switch kind {
	case stateRunning:
		dot = "●"
	case stateFailed:
		dot = "!"
	}
	state := dot + " " + label + " · " + detail
	// 菜单项没有颜色，● / ○ / ! 是唯一的语义区分手段：实心=在跑，空心=停着，
	// 感叹号=出错。用「×」会和菜单的关闭语义混淆。

	agg := a.snapshot()
	stats1, stats2 := trayStats(agg, a.running())

	toggle := "启动"
	if a.running() {
		toggle = "停止"
	}

	if state != t.lastState {
		t.stateItem.SetLabel(state)
		t.lastState = state
	}
	if stats1 != t.lastStats1 {
		t.statsItem1.SetLabel(stats1)
		t.lastStats1 = stats1
	}
	if stats2 != t.lastStats2 {
		t.statsItem2.SetLabel(stats2)
		// 第二行（节省量）只在有数据时出现：0 字节时多一行「节省 0 B」
		// 只是噪声，而它出现/消失会让菜单高度变化——这是可接受的，
		// 因为它只在第一次真的省下流量时出现一次。
		t.statsItem2.SetVisible(stats2 != "")
		t.lastStats2 = stats2
	}
	if toggle != t.lastToggle {
		t.toggleItem.SetLabel(toggle)
		t.lastToggle = toggle
	}

	// 菜单栏标题：运行且有压缩比时显示倍数，否则清空。
	//
	// 为什么用 U+2007（FIGURE SPACE）补齐：mygo 的 SetTitle 文档明确说了
	// 「数字等宽，只要文本长度不变，项的宽度就不变」。1.8× → 12.0× 只差一个
	// 字符，但如果从「9.4×」跳到「10.2×」就会宽一格，图标与它旁边的项会跟着
	// 动。补齐到 4 个字符宽度可避免这种抖动。
	title := ""
	if a.running() && agg.UpRaw > 0 {
		if x, ok := ratioFactor(agg.UpRatio); ok {
			title = padFigure(x + "×")
		}
	}
	if title != t.lastTitle {
		t.tray.SetTitle(title)
		t.lastTitle = title
	}

	// tooltip 带上最有用的一条事实：没跑时说怎么开始，跑起来说监听在哪
	tip := "reply9842 · 已停止"
	if a.running() {
		tip = "reply9842 · 监听 " + a.runner.Addr()
	}
	t.tray.SetToolTip(tip)
}

// padFigure 把短文本用 U+2007 FIGURE SPACE 右补齐到固定宽度。
//
// 只在左侧垫空格（而不是右侧）会让数字看起来往右漂；这里统一右侧补齐，
// 数字始终贴左，视觉上稳定。
func padFigure(s string) string {
	const width = 5 // "100×" 是 4 个字符，留一格余量
	for len([]rune(s)) < width {
		s += "\u2007"
	}
	return s
}

// trayStats 菜单里的两行统计。返回空串表示该行不显示。
//
// 口径与窗口里的三格统计一致（humanBytes / groupDigits / ratioFactor），
// 两处显示同一个数字时不会一个说 1.4 MB、一个说 1400 KB。
func trayStats(agg stats.Agg, running bool) (line1, line2 string) {
	if !running {
		return "代理未运行", ""
	}
	if agg.Requests == 0 {
		return "等待第一个请求", ""
	}
	saved := agg.UpRaw - agg.UpWire
	if saved < 0 {
		saved = 0
	}
	line1 = groupDigits(agg.Requests) + " 请求"
	if x, ok := ratioFactor(agg.UpRatio); ok {
		line1 += " · " + x + "× 压缩"
	}
	line2 = "节省 " + humanBytes(saved)
	return line1, line2
}
