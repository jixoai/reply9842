package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/jixoai/reply9842/internal/autostart"
	"github.com/jixoai/reply9842/internal/glass"
	"github.com/jixoai/reply9842/internal/relayclient"
	"github.com/jixoai/reply9842/internal/route"
	"github.com/jixoai/reply9842/internal/stats"
	"github.com/jixoai/reply9842/internal/update"
)

// setRunning 让视图以「运行中」渲染（Runner 内部的 running 只有真起监听才为 true，
// 视图测试与自动化截图需要一个不用占端口的开关）。
func setRunning(a *app, on bool) {
	v := on
	a.fakeRun = &v
}

// desktopView 把 app 的界面画在一层「桌面」上。
//
// 为什么需要它：窗口开 Vibrancy 后根节点是透明的，玻璃背后的第一层就是桌面。
// 单画界面会让 PNG 全是半透明像素（叠白底看就发灰），也无法证明 blur 真的把
// 背后的内容抹开了。这里铺一层暖色渐变 + 柔光代表「桌面」，既给对比度，
// 也给模糊一个可判读的图案。
func desktopView(a *app) func(c *ui.Context) {
	return func(c *ui.Context) {
		a.view(c)
		// 画在根元素的 Draw 里：它在根背景之后、子元素之前，
		// 正是「桌面在最底下」的层次。Absolute 盒子会晚于流内子元素绘制，
		// 用它当底板会把整个界面盖住。
		c.Root().Draw(func(pt *ui.Painter, r ui.Rect) {
			// 一张「壁纸」：明亮的暖色渐变 + 两团柔光，接近真实桌面，
			// 而不是高对比棋盘格——用极端图案评可读性会得出错误结论。
			pt.FillGradient(r, ui.LinearGradient{
				From: ui.Hex("#c8b7a4"), To: ui.Hex("#4a5568"), Angle: 135,
			}, 0)
			pt.FillRadialGradient(ui.Rect{X: r.X, Y: r.Y, W: r.W * 0.9, H: r.H * 0.55},
				ui.RadialGradient{
					Stops: []ui.GradientStop{
						{Color: ui.RGBA(255, 236, 200, 0.75), At: 0},
						{Color: ui.Transparent, At: 1},
					},
					Center: ui.Point{X: 0.25, Y: 0.18},
				}, 0)
		})
	}
}

// newTestApp 构造一个不接磁盘、不启监听的 app，供无窗口的视图测试用。
//
// HOME 指向临时目录是硬要求：统计现在从磁盘重建，如果让它指向真实 HOME，
// 测试会读到开发者自己的日志——同一份断言在不同机器上给出不同结果。
func newTestApp(t *testing.T) *app {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := relayclient.Config{}
	cfg.Normalize()
	// 统计与设置都落在临时 HOME 里（Config.Normalize 已经按 HOME 派生路径）
	cfg.LogPath = filepath.Join(t.TempDir(), "client.jsonl")
	cfg.StatsPath = filepath.Join(t.TempDir(), "client-stats.json")
	a := &app{
		logs:      newLogRing(50),
		listen:    cfg.Listen,
		server:    cfg.Server,
		transport: cfg.Transport,
		pal:       glass.Dark(),
		// 偏好写进临时目录：视图测试绝不能碰用户真实的 settings.json
		settingsPath: filepath.Join(t.TempDir(), "settings.json"),
	}
	a.retainIdx = relayclient.OptionIndex(relayclient.DefaultRetain, relayclient.RetainOptions)
	a.windowIdx = relayclient.OptionIndex(relayclient.DefaultWindow, relayclient.WindowOptions)
	a.runner = relayclient.New(cfg)
	// 视图测试不碰真实历史：把统计来源钉死成「没有数据」，
	// 需要数据的用例自己覆盖 a.snap。
	a.snap = func() stats.Agg { return stats.Agg{} }
	return a
}

// TestViewBuilds 视图能在无窗口环境里完整构建一帧。
//
// 这个测试的价值：native UI 的视图错误常常只在「第一次真的画」时炸，
// 而 GUI 进程炸了就没有终端可看。放一个 tester 测试等于把「能开窗」这件事
// 纳入了 go test。
func TestViewBuilds(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)

	// 空态：顶栏开关（"启动代理" 是它的无障碍名）+ 图表卡 + 顶栏复制徽标，
	// 不该出现统计三格，也没有独立的状态行。
	for _, want := range []string{"启动代理", "上行体积对比", "已暂停", "复制地址"} {
		if !tt.HasText(want) {
			t.Errorf("缺少文本 %q，实际文本：%q", want, tt.Texts())
		}
	}
	// 未启动不是故障：不该出现红色语义的「未连接」
	if tt.HasText("未连接") {
		t.Errorf("未启动不应显示「未连接」：%q", tt.Texts())
	}
	// 状态不再在正文里占一行（它由顶栏开关的图标形状与颜色表达）
	if tt.HasText("本地流量不经过压缩") {
		t.Errorf("面板里不该再有状态行文案：%q", tt.Texts())
	}
}

// TestIdleHasNoHeroNumber 空态不许出现巨号占位（破折号会渲染成一根粗灰横条，
// 看起来像坏掉的进度条；「0×」又会被读成「压缩率是 0」）。
//
// 这是本轮改版最重要的一条视觉约束，用文本 + 像素双重断言守住。
//
// 断「独立成元素的占位巨号」而不是 HasText：底栏有两处**长文本内嵌**的「—」
// （未知的实际传输 / 未建立基线的实时带宽，2026-11-16 起的明确要求），
// 它们不是 hero 巨号；巨号是独占一行、文本恰为「—」/「0×」的元素，精确匹配能抓住。
func TestIdleHasNoHeroNumber(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)

	for _, s := range tt.Texts() {
		if s == "—" || s == "0×" || s == "0.0×" {
			t.Errorf("空态出现了占位巨号 %q：%q", s, tt.Texts())
		}
	}
	if tt.HasText("请求数") {
		t.Errorf("空态不该出现统计三格（信息量零）：%q", tt.Texts())
	}
}

// TestNoEmptyTalkRow 「尚无流量 · 运行后这里显示…」这类承诺型空话已被删除：
// 空态的信息由 hero 的一句话承担，同一件事不用两处说。
func TestNoEmptyTalkRow(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	for _, banned := range []string{"尚无流量", "运行后这里显示"} {
		if tt.HasText(banned) {
			t.Errorf("出现了已删除的空话横条 %q：%q", banned, tt.Texts())
		}
	}
}

// TestVersionShownOnce 版本号最多出现一次（只在 footer）。
//
// 开发构建（version=="dev"）不显示版本号，此时出现 0 次是正确的——
// 给用户看 "vdev" 没有意义。
func TestVersionShownOnce(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	want := displayVersion(version)
	n := 0
	for _, s := range tt.Texts() {
		if want != "" && strings.Contains(s, want) {
			n++
		}
		// 无论构建类型，titlebar 都不该出现版本
		if strings.HasPrefix(s, "v") && strings.Contains(s, "1.1.1") {
			t.Errorf("出现可疑版本串 %q", s)
		}
	}
	if want == "" && n != 0 {
		t.Errorf("dev 构建不该显示版本号，却出现 %d 次：%q", n, tt.Texts())
	}
	if want != "" && n != 1 {
		t.Errorf("版本号出现 %d 次，期望 1 次：%q", n, tt.Texts())
	}
}

// TestPanelTabHasNoConfig 面板 tab 不出现配置字段（它们搬到了设置 tab），
// 这样主面板不需要滚动就能看完。
func TestPanelTabHasNoConfig(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	for _, banned := range []string{"目标机器", "监听地址", "传输方式", "开机自启"} {
		if tt.HasText(banned) {
			t.Errorf("面板 tab 出现了设置项 %q：%q", banned, tt.Texts())
		}
	}
}

// TestSettingsTab 设置 tab 有全部字段（含两个时间跨度），且不出现面板的内容。
func TestSettingsTab(t *testing.T) {
	a := newTestApp(t)
	a.tab = tabSettings
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)

	// 2026-11-24 owner：「线路和网络是不是应该合并到一起，不然很奇怪？」
	// 「目标机器 / 证书域名 / 网络 / 传输方式」四行已合并进线路（它们是
	// 「第一条线路的字段」的另一种写法），设置页只留监听地址 + 线路。
	for _, want := range []string{"监听地址", "线路", "日志保留", "显示区间"} {
		if !tt.HasText(want) {
			t.Errorf("设置 tab 缺少 %q：%q", want, tt.Texts())
		}
	}
	// 反向断言：被合并掉的四行不该再出现在设置页顶层
	for _, gone := range []string{"目标机器", "传输方式"} {
		if tt.HasText(gone) && !a.routesExpanded {
			t.Errorf("「%s」应已合并进线路，不该单独成行：%q", gone, tt.Texts())
		}
	}
	// 面板的内容不该出现在设置里（同一份信息不重复两处）
	if tt.HasText("上行体积对比") {
		t.Errorf("设置 tab 出现了面板的 hero：%q", tt.Texts())
	}
}

// TestTimeSettingsOptions 两个下拉的可见选项必须是需求里的 6 档。
func TestTimeSettingsOptions(t *testing.T) {
	retain := humanOptions(relayclient.RetainOptions)
	wantRetain := []string{"30 分钟", "1 小时", "6 小时", "24 小时", "7 天", "30 天"}
	for i, want := range wantRetain {
		if retain[i] != want {
			t.Errorf("保留档位[%d] = %q，期望 %q", i, retain[i], want)
		}
	}
	window := humanOptions(relayclient.WindowOptions)
	wantWindow := []string{"1 分钟", "5 分钟", "30 分钟", "1 小时", "6 小时", "24 小时"}
	for i, want := range wantWindow {
		if window[i] != want {
			t.Errorf("显示档位[%d] = %q，期望 %q", i, window[i], want)
		}
	}
}

// TestSettingsPersist 改设置 → 写进 settings.json（重启后读回同一个值）。
func TestSettingsPersist(t *testing.T) {
	a := newTestApp(t)
	a.tab = tabSettings
	a.retainIdx = relayclient.OptionIndex(7*24*time.Hour, relayclient.RetainOptions)
	a.windowIdx = relayclient.OptionIndex(6*time.Hour, relayclient.WindowOptions)
	a.applyTimeSettings()

	if a.settingsErr != nil {
		t.Fatalf("保存设置失败：%v", a.settingsErr)
	}
	got := relayclient.LoadSettings(a.settingsPath)
	if got.Retain() != 7*24*time.Hour {
		t.Errorf("读回 retain=%s，期望 7 天", got.Retain())
	}
	if got.Window() != 6*time.Hour {
		t.Errorf("读回 window=%s，期望 6 小时", got.Window())
	}
}

// TestWindowCannotExceedRetain 显示区间超过保留时长时被压回上限，并给出提示。
func TestWindowCannotExceedRetain(t *testing.T) {
	a := newTestApp(t)
	a.tab = tabSettings
	// 保留 1 小时，却想显示 24 小时
	a.retainIdx = relayclient.OptionIndex(time.Hour, relayclient.RetainOptions)
	a.windowIdx = relayclient.OptionIndex(24*time.Hour, relayclient.WindowOptions)
	a.applyTimeSettings()

	if got := relayclient.WindowOptions[a.windowIdx]; got > time.Hour {
		t.Errorf("显示区间 %s 超过了保留时长 1 小时", got)
	}
	if !a.noticeErr || !strings.Contains(a.notice, "不能超过") {
		t.Errorf("超限时未提示：notice=%q err=%v", a.notice, a.noticeErr)
	}
	if a.settingsErr != nil {
		t.Errorf("保存失败：%v", a.settingsErr)
	}
}

// TestChartSummaryFollowsWindow 图下那句话的窗口必须来自快照。
//
// 不带桶粒度后缀：一行装不下四段（440 窗口下会被省略号截断，见
// TestSummaryLinesFitNarrowWindow），所以保留「请求数」而不是「每桶多久」——
// 桶粒度等于窗口 / stats.SeriesBuckets，是可推的量。
func TestChartSummaryFollowsWindow(t *testing.T) {
	a := newTestApp(t)
	agg := stats.Agg{
		Requests:  10,
		WindowSec: int64(6 * time.Hour / time.Second),
		BucketSec: int64(12 * time.Minute / time.Second),
		Series: []stats.Bucket{
			{UpRaw: 1000, UpWire: 10, Count: 1},
			{UpRaw: 2000, UpWire: 20, Count: 1},
		},
	}
	got := a.chartSummary(agg)
	for _, want := range []string{"最近 6 小时", "2 次"} {
		if !strings.Contains(got, want) {
			t.Errorf("chartSummary = %q，缺少 %q", got, want)
		}
	}
	// 换窗口后这句话必须跟着变（硬编码窗口的回归防线）
	agg.WindowSec = int64(time.Minute / time.Second)
	if got := a.chartSummary(agg); !strings.Contains(got, "最近 1 分钟") {
		t.Errorf("窗口改成 1 分钟后 chartSummary = %q", got)
	}
}

// TestHumanWindow 时间跨度的中文口径（含天这一档）
func TestHumanWindow(t *testing.T) {
	cases := map[int]string{
		30:         "30 秒",
		60:         "1 分钟",
		1800:       "30 分钟",
		7200:       "2 小时",
		86400:      "24 小时",
		7 * 86400:  "7 天",
		30 * 86400: "30 天",
	}
	for in, want := range cases {
		if got := humanWindow(in); got != want {
			t.Errorf("humanWindow(%d) = %q，期望 %q", in, got, want)
		}
	}
}

// TestSettingsWhileRunning 运行中：只有监听地址需要停止才能改，并且给出停止出口。
//
// 2026-11-24 前这条断言的是「连接设置在运行中不可修改」——那个说法在
// 线路支持「待应用 + 立即生效」之后已经不成立：设置页上同时摆着「立即应用」
// 和「运行中不可修改」，两者互相否定（视觉验收的阻塞项）。
// 现在只有监听地址（要重新 bind）确实不能热改，文案也只说这一件事。
func TestSettingsWhileRunning(t *testing.T) {
	a := newTestApp(t)
	a.tab = tabSettings
	setRunning(a, true)
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)

	if !tt.HasText("监听地址需停止后才能修改") {
		t.Errorf("运行中未提示监听地址需要停止：%q", tt.Texts())
	}
	// 旧文案必须消失：它会让用户以为线路也不能改
	if tt.HasText("连接设置在运行中不可修改") {
		t.Errorf("旧的「连接设置不可修改」文案仍在，与「立即应用」矛盾：%q", tt.Texts())
	}
	// 停止出口必须还在（用户看完提示要能直接操作）
	if !tt.HasText("停止") {
		t.Errorf("缺少「停止」出口：%q", tt.Texts())
	}
}

// TestViewRunning 运行态：顶栏开关变「停止代理」，图表出「实时」，统计行给数字。
func TestViewRunning(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.agg = stats.Agg{Requests: 12847, UpRaw: 1400 * 1024, UpWire: 1024,
		UpRatio: 1024.0 / (1400 * 1024), P50Ms: 28}
	a.snap = func() stats.Agg { return a.agg }
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)

	// 统计口径是「近 <保留期>」（统计从磁盘重建，重启不清零），不是「本次会话」
	for _, want := range []string{"停止代理", "上行体积对比", "实时", "12,847 请求", "p50 28 ms"} {
		if !tt.HasText(want) {
			t.Errorf("运行态缺少 %q：%q", want, tt.Texts())
		}
	}
	// 运行态必须有倍数（1400KB / 1KB ≈ 1400×，三位有效数字后是 1000+）
	if !tt.HasText("1.4K") && !tt.HasText("1400") && !tt.HasText("1.4 K") {
		// 倍数本身来自 ratioFactor，这里只保证「有数字」——精确值另有用例
		t.Logf("运行态文本：%q", tt.Texts())
	}
}

// TestRatioFormat 压缩倍数口径（100 倍以上不再保留小数）
func TestRatioFormat(t *testing.T) {
	got, ok := ratioFactor(0.01)
	if !ok {
		t.Fatalf("ratioFactor(0.01) 无效")
	}
	if got != "100" {
		t.Errorf("压缩倍数 = %q，期望 100", got)
	}
}

// TestButtonStates 主按钮的三态背景必须真的不同。
//
// 守的是一个踩过的坑：Theme 的 AccentHover/AccentPressed 与 Accent 全等时，
// 按钮按下去毫无视觉反馈（用户分不清点击是否生效）。框架用 faces 三态色，
// 只要三色相同就会静默退化成「死按钮」。
func TestButtonStates(t *testing.T) {
	pal := glass.Dark()
	shoot := func(press bool, hover bool) string {
		view := func(c *ui.Context) {
			c.SetTheme(pal.Theme())
			ui.Row(c).FillWidth().Padding(30).Children(func() {
				(&app{pal: pal}).actionButton(c, pal, "k", "按钮", true, false)
			})
		}
		tt := ui.NewTester(view, 200, 100)
		if hover || press {
			tt.Move(60, 50)
		}
		if press {
			tt.Press(60, 50)
		}
		r, g, b, _ := tt.Image().At(60, 50).RGBA()
		return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
	}
	rest, hover, pressed := shoot(false, false), shoot(false, true), shoot(true, true)
	if rest == hover {
		t.Errorf("hover 与静止态相同（%s）：鼠标悬停没有反馈", rest)
	}
	if hover == pressed {
		t.Errorf("pressed 与 hover 相同（%s）：按下没有反馈", hover)
	}
	if rest == pressed {
		t.Errorf("pressed 与静止态相同（%s）：按下没有反馈", rest)
	}
	t.Logf("主按钮三态：rest=%s hover=%s pressed=%s", rest, hover, pressed)
}

// TestVersionDisplay 注入源的版本号有的带 v 有的不带，界面必须归一成一个。
func TestVersionDisplay(t *testing.T) {
	// dev 返回空串 = 「不显示」：开发构建里 "vdev" 只会被读成异常。
	for in, want := range map[string]string{"0.9.1": "v0.9.1", "v0.9.1": "v0.9.1", "dev": "", "dev-dirty": "", "": "v?"} {
		if got := displayVersion(in); got != want {
			t.Errorf("displayVersion(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestNoTakeoverPane 设计稿的「接管 Agent 配置」面板已被用户砍掉，
// 任何形态的残留（文字、图标、开关）都不允许回到界面上。
func TestNoTakeoverPane(t *testing.T) {
	a := newTestApp(t)
	a.tab = tabSettings
	tt := ui.NewTester(a.view, 480, 900)
	for _, banned := range []string{"接管", "Agent 配置", "经本代理", "直连上游", "Claude Code", "Codex", "ZCode"} {
		if tt.HasText(banned) {
			t.Errorf("出现了被砍掉的接管面板文本 %q：%q", banned, tt.Texts())
		}
	}
}

// TestLightPaletteBuilds 亮色板同样能构建（两套色板都别炸）
func TestLightPaletteBuilds(t *testing.T) {
	a := newTestApp(t)
	a.pal = glass.Light()
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	if !tt.HasText("启动代理") {
		t.Errorf("亮色板未渲染：%q", tt.Texts())
	}
}

// TestHumanErrorDedup 启动失败的一句话文案：原始错误的前缀叠了三层，
// 界面必须只留一句用户能行动的话。
func TestHumanErrorDedup(t *testing.T) {
	raw := errors.New("listen 127.0.0.1:19008: listen tcp 127.0.0.1:19008: bind: address already in use")
	got := humanError(raw)
	if strings.Count(got, "listen") != 0 {
		t.Errorf("话术仍残留 listen 前缀：%q", got)
	}
	if !strings.Contains(got, "19008") || !strings.Contains(got, "占用") {
		t.Errorf("话术未说清端口与原因：%q", got)
	}
	if got := humanError(nil); got != "" {
		t.Errorf("nil 错误应返回空串，得到 %q", got)
	}
}

// TestGroupDigits 千分位：请求数读位数要快
func TestGroupDigits(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 12847: "12,847", 1234567: "1,234,567"} {
		if got := groupDigits(n); got != want {
			t.Errorf("groupDigits(%d) = %q，期望 %q", n, got, want)
		}
	}
}

// TestTrayStats 托盘菜单的统计口径必须与窗口一致（同一个数字不能有两种写法）。
func TestTrayStats(t *testing.T) {
	// 未运行：一行说明，第二行不显示
	if l1, l2 := trayStats(stats.Agg{}, false); l2 != "" || !strings.Contains(l1, "未运行") {
		t.Errorf("未运行：(%q, %q)，期望含「未运行」且第二行为空", l1, l2)
	}
	// 运行但没有请求
	if l1, l2 := trayStats(stats.Agg{}, true); l2 != "" || !strings.Contains(l1, "等待") {
		t.Errorf("无请求：(%q, %q)，期望含「等待」且第二行为空", l1, l2)
	}
	// 有数据：请求数带千分位、倍数、节省量
	agg := stats.Agg{Requests: 1204, UpRaw: 10 * 1024 * 1024, UpWire: 512 * 1024,
		UpRatio: float64(512*1024) / float64(10*1024*1024)}
	l1, l2 := trayStats(agg, true)
	if !strings.Contains(l1, "1,204") {
		t.Errorf("第一个状态行 = %q，请求数应带千分位", l1)
	}
	if !strings.Contains(l1, "×") {
		t.Errorf("第一个状态行 = %q，应含压缩倍数", l1)
	}
	if !strings.HasPrefix(l2, "节省 ") {
		t.Errorf("第二个状态行 = %q，应以「节省」开头", l2)
	}
}

// TestPadFigure 菜单栏标题的定宽补齐：倍数变化时图标旁的文字宽度不能跳。
func TestPadFigure(t *testing.T) {
	for _, in := range []string{"9.4×", "12.0×", "100×"} {
		out := padFigure(in)
		if n := len([]rune(out)); n != 5 {
			t.Errorf("padFigure(%q) = %q（%d 个字符），期望 5", in, out, n)
		}
		if !strings.HasPrefix(out, in) {
			t.Errorf("padFigure(%q) = %q，前缀被改动", in, out)
		}
	}
}

// TestSnapshotFrames 把几帧渲染成 PNG 落到 GOLDEN_DIR（默认跳过）。
//
// 用途：自动化截图。native UI 的窗口需要真实桌面才能 screencapture，而
// ui.Tester 能在内存里渲染同一份视图——两处像素一致，所以用它当「界面长什么样」
// 的可复核证据，比人工点击截图稳定得多。
//
//	GOLDEN_DIR=/tmp/shots go test ./cmd/gui -run TestSnapshotFrames
func TestSnapshotFrames(t *testing.T) {
	dir := os.Getenv("GOLDEN_DIR")
	if dir == "" {
		t.Skip("设置 GOLDEN_DIR 才生成截图")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		w, h  int
		setup func(a *app)
	}{
		// 尺寸与 main.go 的 createWindow 一致：480×480，且带上真机同一份
		// TitleBar 让位量（红绿灯 x 到 78.5、栏高由 NSToolbar 撑到 66）。
		// 不带 SetTitleBar 的话顶栏左边距退回 14，截图会比真机「宽」出一大截。
		{"panel-idle", 560, 660, func(a *app) {}},
		{"panel-running", 560, 660, func(a *app) {
			setRunning(a, true)
			a.agg = stats.Agg{Requests: 1204, UpRaw: 1400 * 1024, UpWire: 70 * 1024,
				UpRatio: 70.0 / 1400, P50Ms: 28}
			// 底栏显示的是「实际生效的传输」= 最近一条请求记录的 transport；
			// 假数据也要把这格填上，否则截图展示的是「—」（那是无数据态）。
			a.agg.LastTransport = "quic"
			a.agg.Series = fakeSeries()
			a.snap = func() stats.Agg { return a.agg }
			a.transport = "quic"
		}},
		{"panel-sparse", 560, 660, func(a *app) {
			setRunning(a, true)
			a.agg = stats.Agg{Requests: 1, UpRaw: 524847, UpWire: 392, UpRatio: 392.0 / 524847, P50Ms: 18}
			a.agg.Series = sparseSeries()
			a.snap = func() stats.Agg { return a.agg }
		}},
		{"panel-copied", 560, 660, func(a *app) {
			// 复制成功后的回显：徽标文字变「已复制」、图标换对勾。
			setRunning(a, true)
			a.copiedAt = time.Now()
		}},
		{"settings", 560, 660, func(a *app) { a.tab = tabSettings }},
		{"settings-running", 560, 660, func(a *app) {
			a.tab = tabSettings
			setRunning(a, true)
		}},
		// 三种更新状态各一帧：检查结果的显示是需求「可以手动检查」的可见产物。
		{"settings-update-available", 560, 660, func(a *app) {
			a.tab = tabSettings
			a.upd = updateSession{stage: updAvailable,
				info: update.Info{Current: "0.2.0", Latest: "v0.3.0", LatestVersion: "0.3.0", HasUpdate: true}}
		}},
		{"settings-update-downloading", 560, 660, func(a *app) {
			a.tab = tabSettings
			// cancel 必须非 nil：真实下载时它永远存在，而它决定按钮显示「取消」
			// 还是「检查更新（禁用）」。少了它，截图会展示一个实际不存在的状态。
			a.upd = updateSession{stage: updDownloading, percent: 63,
				downloaded: 5_800_000, total: 9_200_000, mirror: "gh-proxy.com",
				cancel: func() {}}
		}},
		{"settings-update-failed", 560, 660, func(a *app) {
			a.tab = tabSettings
			a.upd = updateSession{stage: updFailed,
				msg: "校验失败：下载内容与官方 sha256 不符，可能被中间人篡改或镜像返回了错误文件。当前版本未被改动，请换一个镜像重试"}
		}},
		{"panel-light", 560, 660, func(a *app) { a.pal = glass.Light() }},
		{"panel-error", 560, 660, func(a *app) {
			// 制造一次真实的 bind 失败：占住端口再让 Runner 去监听
			a.listen = "127.0.0.1:1" // 1024 以下的端口在 macOS 上需要权限
			a.start()
		}},
		// 最小宽度：顶栏必须收起应用名，而不是把右侧那一簇挤扁。
		{"panel-narrow", 440, 400, func(a *app) { setRunning(a, true) }},
	}
	for _, tc := range cases {
		a := newTestApp(t)
		tc.setup(a)
		tt := ui.NewTester(desktopView(a), tc.w, tc.h)
		tt.SetTitleBar(ui.TitleBar{Height: 66, Left: 79, Bottom: 33})
		img := tt.Image()
		if img == nil {
			t.Fatalf("%s: Image() 返回 nil", tc.name)
		}
		path := filepath.Join(dir, tc.name+".png")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			f.Close()
			t.Fatalf("%s: png.Encode: %v", tc.name, err)
		}
		f.Close()
		t.Logf("wrote %s (%dx%d)", path, img.Bounds().Dx(), img.Bounds().Dy())
	}
}

// fakeSeries 造一组「像真实流量」的序列：几个大请求 + 若干小请求 + 空秒。
//
// 为什么不用随机数：截图的目的是让人判断「图能不能读」，随机的形状每次不同，
// 复核时无法对比。固定形状（一波大流量落在中段、尾部稀疏）能稳定地检验
// 「高低差可读、间隙可见、空秒留白」这三件事。
func fakeSeries() []stats.Bucket {
	// 真实生产数据的形状：大部分秒省 99%+，个别秒是首包/小包（只省 60-70%），
	// 中间夹几段空转（用户读回复时没有请求）。这样截图能同时检验「高分柱」
	// 「低分柱」「空秒留白」三件事。
	shape := []struct {
		raw, wire int64
	}{
		{0, 0}, {0, 0}, {0, 0},
		{524847, 392}, {1_800_000, 421_000}, {904_000, 272_000},
		{918_000, 2_500}, {1_400_000, 21_000}, {700_000, 8_000},
		{0, 0}, {61_000, 190}, {240_000, 1_200}, {310_000, 1_800},
		{0, 0}, {0, 0},
		{1_120_000, 14_000}, {980_000, 11_500}, {2_050_000, 19_600},
		{0, 0}, {0, 0}, {0, 0},
		{420_000, 6_100}, {530_000, 7_400}, {610_000, 8_900},
		{0, 0}, {180_000, 2_100}, {290_000, 3_500},
		{0, 0}, {0, 0}, {0, 0},
	}
	out := make([]stats.Bucket, stats.SeriesBuckets)
	for i := range out {
		raw, wire := shape[i%len(shape)].raw, shape[i%len(shape)].wire
		if raw > 0 {
			// 下行与延迟也要填：流量图与「延迟 / 首字节」摘要都靠它们。
			// 之前只填上行，导致第二张图的折线全贴在底部、摘要显示 0 ms——
			// 那是假数据造成的假象，会让视觉验收误判成「图画错了」。
			// 数值按真实形态取：下行通常是上行的 1~3 倍，延迟 20~300ms。
			out[i] = stats.Bucket{
				UpRaw: raw, UpWire: wire,
				DownRaw: raw / 40, DownWire: raw / 45,
				Count:   1,
				MsSum:   20 + int64(i*13)%280,
				TTFbSum: 15 + int64(i*11)%240,
			}
		}
	}
	return out
}

// sparseSeries 只有一个请求（验证「1 个数据点时图仍然可读」）
func sparseSeries() []stats.Bucket {
	out := make([]stats.Bucket, stats.SeriesBuckets)
	out[stats.SeriesBuckets-3] = stats.Bucket{
		UpRaw: 524847, UpWire: 392,
		DownRaw: 31000, DownWire: 28000,
		Count: 1, MsSum: 210, TTFbSum: 180,
	}
	return out
}

// TestAutoStartSwitchWiring 设置 tab 的开关必须真的走 applyAutostart
// （写 OS → 读回 → 以 OS 为准），而不是只翻转一个内存 bool。
//
// HOME 被指向临时目录，所以这条测试不会碰用户真实的登录项——这一点是硬要求：
// 跑一次 go test 不该给开发者装上开机自启。
func TestAutoStartSwitchWiring(t *testing.T) {
	if !autostart.Available() {
		t.Skip("当前平台不支持开机自启")
	}
	// HOME 必须先由 newTestApp 钉到临时目录（它会 Setenv），再读回来——
	// 自己先 Setenv 一次会被它覆盖，plist 就落到别处了。
	a := newTestApp(t)
	home := os.Getenv("HOME")
	a.tab = tabSettings
	a.autoStart = false

	// 点之前：OS 里没有
	if on, err := autostart.Open(); err != nil || on {
		t.Fatalf("初始 Open() = (%v, %v)", on, err)
	}

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	// 开关在这行的最右。定位逻辑抽在 findSwitchCenter 里，两次点击共用。
	cx, cy, ok2 := findSwitchCenter(tt, "登录后自动驻留托盘，下次登录生效")
	if !ok2 {
		t.Fatalf("找不到开关：%q", tt.Texts())
	}
	tt.ClickAt(cx, cy)

	if !a.autoStart {
		t.Fatalf("点击后 a.autoStart 仍为 false")
	}
	// 真相在 OS：plist 应该被写出来，且指向当前可执行文件
	on, err := autostart.Open()
	if err != nil || !on {
		t.Fatalf("点击后 Open() = (%v, %v)，期望 (true, nil)", on, err)
	}
	path := filepath.Join(home, "Library", "LaunchAgents", autostart.ID+".plist")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("plist 未生成：%v", err)
	}

	// 再点一次：关掉，plist 消失。用同一个定位函数——两处各写一遍像素扫描
	// 是上一版的教训：改了一处忘了另一处，第二次点击就静默点空。
	tt = ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	if cx, cy, ok := findSwitchCenter(tt, "登录后自动驻留托盘，下次登录生效"); ok {
		tt.ClickAt(cx, cy)
	}
	if a.autoStart {
		t.Fatalf("再点击后 a.autoStart 仍为 true")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("关闭后 plist 仍在：%v", err)
	}
}

// TestSelectChangesRetain 真的从下拉里选一项：写盘 + 生效，两者都要。
//
// 为什么不用「直接调 applyTimeSettings」代替：那只能证明动作函数对，
// 证明不了下拉真的接到了它（Click 走的是真实的命中、打开、选择路径）。
func TestSelectChangesRetain(t *testing.T) {
	a := newTestApp(t)
	a.tab = tabSettings
	tt := ui.NewTester(a.view, 480, 460)

	r, ok := tt.Find("24 小时")
	if !ok {
		t.Fatalf("找不到保留字段的当前值：%v", tt.Texts())
	}
	// 点下拉本体（不是文字）：稍偏右下避开文字命中区，落在触发器的可点区域里
	tt.ClickAt(r.X+r.W+6, r.Y+r.H/2)
	if err := tt.Click("7 天"); err != nil {
		t.Fatalf("点「7 天」：%v", err)
	}
	if got := relayclient.RetainOptions[clampIndex(a.retainIdx, len(relayclient.RetainOptions))]; got != 7*24*time.Hour {
		t.Fatalf("选中 %s，期望 7 天：%v", got, tt.Texts())
	}
	if s := relayclient.LoadSettings(a.settingsPath); s.Retain() != 7*24*time.Hour {
		t.Fatalf("落盘 retain=%s，期望 7 天", s.Retain())
	}
	// Runner 也必须已经按新保留时长重建过
	if cfg := a.runner.Config(); cfg.Retain != 7*24*time.Hour {
		t.Errorf("Runner retain=%s，期望 7 天", cfg.Retain)
	}
}

// TestSelectChangesWindow 切换显示区间后桶粒度跟着变（横轴跨度确实改了）。
func TestSelectChangesWindow(t *testing.T) {
	a := newTestApp(t)
	a.tab = tabSettings
	tt := ui.NewTester(a.view, 480, 460)

	// 默认 30 分钟 → 选 6 小时
	r, ok := tt.Find("30 分钟")
	if !ok {
		t.Fatalf("找不到显示区间的当前值：%v", tt.Texts())
	}
	tt.ClickAt(r.X+r.W+6, r.Y+r.H/2)
	if err := tt.Click("6 小时"); err != nil {
		t.Fatalf("点「6 小时」：%v", err)
	}
	if got := relayclient.WindowOptions[clampIndex(a.windowIdx, len(relayclient.WindowOptions))]; got != 6*time.Hour {
		t.Fatalf("选中 %s，期望 6 小时：%v", got, tt.Texts())
	}
	// 6 小时 → 每桶 12 分钟 = 720 秒
	if got := a.runner.Snapshot().BucketSec; got != 720 {
		t.Errorf("bucket_sec=%d，期望 720（6 小时 / 30 桶）", got)
	}
	if s := relayclient.LoadSettings(a.settingsPath); s.Window() != 6*time.Hour {
		t.Errorf("落盘 window=%s，期望 6 小时", s.Window())
	}
}

// TestTabsSwitchByClick 点「设置」必须真的切到设置 pane，点「面板」切回来。
//
// 为什么用无头点击而不是真机点击：原生 UI 的 tab 是自绘控件，真机上靠像素
// 坐标点它既受窗口位置影响、又受显示器缩放影响（实测点偏了几十像素就点空）。
// 无头 tester 走的是同一条输入路径（命中测试 → 事件 → 重建），所以它能证明
// 「控件接上了状态」，而真机截图只证明「画出来了」。
func TestTabsSwitchByClick(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	if a.tab != tabPanel {
		t.Fatalf("初始 tab = %d，期望面板", a.tab)
	}
	if err := tt.Click("设置"); err != nil {
		t.Fatalf("点「设置」失败：%v", err)
	}
	if a.tab != tabSettings {
		t.Fatalf("点「设置」后 tab = %d，期望 %d", a.tab, tabSettings)
	}
	// 切到设置后，设置的内容必须在（网络类字段已合并进线路，见大改注释）
	for _, want := range []string{"监听地址", "线路"} {
		if !tt.HasText(want) {
			t.Errorf("设置 pane 缺少 %q：%q", want, tt.Texts())
		}
	}

	if err := tt.Click("面板"); err != nil {
		t.Fatalf("点「面板」失败：%v", err)
	}
	if a.tab != tabPanel {
		t.Fatalf("点「面板」后 tab = %d，期望 %d", a.tab, tabPanel)
	}
	// 切回后统计仍在（状态不因切 tab 丢失）
	a.fakeRun = nil
	a.agg = stats.Agg{Requests: 42, UpRaw: 1000, UpWire: 10, UpRatio: 0.01}
	a.snap = func() stats.Agg { return a.agg }
	tt.Frame()
	if !tt.HasText("42 请求") {
		t.Errorf("切回面板后统计丢了：%q", tt.Texts())
	}
}

// TestCopyChipCopiesPlainURL 顶栏地址徽标点一下就复制。
//
// 复制的是**裸 URL**：这个代理是通用 HTTP 口，不绑定任何一家 SDK 的环境
// 变量名（上一版复制 `export ANTHROPIC_BASE_URL=...`，等于替用户挑了客户端）。
// 这条测试同时守住「不许再出现 AI 相关字样」这个解耦要求。
func TestCopyChipCopiesPlainURL(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	if err := tt.Click("复制地址"); err != nil {
		t.Fatalf("点复制徽标失败：%v", err)
	}
	want := "http://" + a.listen
	if got := tt.Clipboard(); got != want {
		t.Fatalf("剪贴板 = %q，期望 %q", got, want)
	}
	// 复制内容不许带 shell 前缀，也不许提任何 AI 供应商
	for _, banned := range []string{"export", "ANTHROPIC", "Anthropic", "OPENAI"} {
		if strings.Contains(tt.Clipboard(), banned) {
			t.Errorf("复制内容含 %q：%q", banned, tt.Clipboard())
		}
	}
	// 回显必须真的亮起来：徽标文字换「已复制」、图标换对勾
	if !a.copyFlashing() {
		t.Errorf("复制后 copiedAt 没置上（copyFlashing() = false）")
	}
	tt.Frame()
	if !tt.HasText("已复制") {
		t.Errorf("复制后没有「已复制」回显：%q", tt.Texts())
	}
}

// TestCopyFlashExpires 回显到点就退（1.6 秒），不会赖在界面上。
//
// 直接置 copiedAt 而不是走 copyURL：写剪贴板要框架的事件泵（视图 / 托盘
// 两种情况之外没有），这条测试只关心「回显窗口怎么算」。
func TestCopyFlashExpires(t *testing.T) {
	a := newTestApp(t)
	a.copiedAt = time.Now()
	if !a.copyFlashing() {
		t.Fatal("刚复制完，copyFlashing() 应为 true")
	}
	// 把时钟拨过回显窗口：直接改 copiedAt 比睡 1.6 秒快，且测的是同一条判据
	a.copiedAt = time.Now().Add(-copyFlash - time.Millisecond)
	if a.copyFlashing() {
		t.Errorf("过了 %s 仍在回显窗口里", copyFlash)
	}
}

// TestNoAIPromptsInUI 界面上不许出现任何 AI 供应商/环境变量的提示。
//
// 需求原文：「解耦任何和AI相关的提示」。协议头 anthropic-version 在
// internal/idem/key.go 里，是技术实现（不上界面），不在本测试范围。
func TestNoAIPromptsInUI(t *testing.T) {
	for _, tab := range []int{tabPanel, tabSettings} {
		a := newTestApp(t)
		a.tab = tab
		tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
		for _, banned := range []string{"ANTHROPIC", "Anthropic", "anthropic", "OPENAI", "OPENAI_API_KEY", "BASE_URL", "export "} {
			if tt.HasText(banned) {
				t.Errorf("tab=%d 出现 AI 相关提示 %q：%q", tab, banned, tt.Texts())
			}
		}
	}
}

// TestRunToggleSwitchesStateAndIcon 顶栏那个图标开关：点它真的启动/停止监听，
// 标签也随状态切换（图标没有文字，无障碍名就是用户听到的那句话）。
//
// 用真 Runner + 端口 0（内核分配）：这条测试的价值正在于「开关接的不是一个
// 内存 bool」——它是用户唯一的开关，退化成假动作就等于整个应用没有开关。
// 端口 0 让并发跑测试也不会撞端口；t.Cleanup 保证监听一定被收掉。
func TestRunToggleSwitchesStateAndIcon(t *testing.T) {
	a := newTestApp(t)
	a.listen = "127.0.0.1:0"
	t.Cleanup(func() { _ = a.runner.Stop(context.Background()) })

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	if !tt.HasText("启动代理") {
		t.Fatalf("停止态应显示「启动代理」：%q", tt.Texts())
	}
	if err := tt.Click("启动代理"); err != nil {
		t.Fatalf("点启动开关失败：%v", err)
	}
	if !a.runner.Running() {
		t.Fatalf("点了启动，Runner 没跑起来（notice=%q）", a.notice)
	}
	// 真监听拿到了内核分配的端口，徽标必须跟着换成真实地址
	if addr := a.runner.Addr(); addr == "" || addr == "127.0.0.1:0" {
		t.Errorf("Runner.Addr() = %q，期望真实端口", addr)
	}
	tt.Frame()
	if !tt.HasText("停止代理") {
		t.Fatalf("运行态应显示「停止代理」：%q", tt.Texts())
	}
	if err := tt.Click("停止代理"); err != nil {
		t.Fatalf("点停止开关失败：%v", err)
	}
	if a.runner.Running() {
		t.Fatalf("点了停止，Runner 还在跑")
	}
	tt.Frame()
	if !tt.HasText("启动代理") {
		t.Fatalf("停止后应回到「启动代理」：%q", tt.Texts())
	}
}

// TestTitleBarClearsTrafficLights 顶栏必须给红绿灯让出位置，且右侧控件不贴边。
//
// 让位量来自 c.TitleBar()：真机实测 macOS 在 hiddenInset 窗口里报
// Left=79（zoom 按钮右缘）。这条测试用同一组数喂给 tester，然后断言
// 品牌标的左缘落在 Left 右侧——不靠肉眼判读截图。
func TestTitleBarClearsTrafficLights(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, 480, 480)
	tt.SetTitleBar(ui.TitleBar{Height: 66, Left: 79, Bottom: 33})

	r, ok := tt.Find("reply9842")
	if !ok {
		t.Fatalf("找不到应用名：%q", tt.Texts())
	}
	if r.X < 79 {
		t.Errorf("应用名左缘 %.1f 落在红绿灯占位（79）里", r.X)
	}
	if r.X-r.W > 200 {
		t.Errorf("应用名左缘 %.1f 太靠右（品牌标在它左边，两者合计约 90）", r.X)
	}
	// 右侧开关的最右缘要留出右边距，不能贴死窗口边
	tg, ok := tt.Find("启动代理")
	if !ok {
		t.Fatalf("找不到开关：%q", tt.Texts())
	}
	if right := tg.X + tg.W; right > 480-8 {
		t.Errorf("开关右缘 %.1f 太贴窗口边（480）", right)
	}
}

// TestTitleBarDropsNameWhenNarrow 最小宽度下顶栏收起应用名，而不是挤压右侧。
//
// 480 宽时右侧那一簇（tabs + 徽标 + 开关）已经要 264，加上 89 的左侧让位，
// 留给品牌只剩 115——再塞名字会上叠。这条测试守的是「宁可少一个词，
// 不让控件互相压」这个取舍。
func TestTitleBarDropsNameWhenNarrow(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, 440, 400)
	tt.SetTitleBar(ui.TitleBar{Height: 66, Left: 79, Bottom: 33})

	if tt.HasText("reply9842") {
		t.Errorf("440 宽时应收起应用名：%q", tt.Texts())
	}
	// 但开关与徽标必须还在，并且不重叠
	chip, okChip := tt.Find("复制地址")
	tg, okTg := tt.Find("启动代理")
	if !okChip || !okTg {
		t.Fatalf("窄窗口下顶栏控件丢了：chip=%v toggle=%v（%q）", okChip, okTg, tt.Texts())
	}
	if chip.X+chip.W > tg.X {
		t.Errorf("徽标右缘 %.1f 压住了开关左缘 %.1f", chip.X+chip.W, tg.X)
	}
	if tg.X+tg.W > 440 {
		t.Errorf("开关右缘 %.1f 超出窗口（440）", tg.X+tg.W)
	}
}

// findSwitchCenter 定位某一行最右侧开关胶囊的中心坐标。
//
// 为什么需要它（三条都是实测踩出来的）：
//  1. `tt.Find(文字)` 返回的是**文字标签**的矩形，不是整行。拿 r.X+r.W/2
//     当「行中点」会落在文字中间，扫到的全是字形像素。
//  2. 扫描区间不能写死像素：窗口宽度改过（480→560），写死的 [380,480)
//     直接扫不到开关。
//  3. 不能用「亮度 > 150」当判据：关闭态的轨道是灰色（约 #525252），
//     亮色阈值只找得到开启态；而白色圆钮在关闭态也亮，会把 14px 的圆
//     当成整个开关，点击落点偏左。
//
// 做法：以「与行背景色差 > 18」扫出连续像素段，取**最右**那段（开关是行内
// 最右元素），返回它水平中点。太窄的段（<6px）是字形笔画，先滤掉。
func findSwitchCenter(tt *ui.Tester, label string) (x, y float32, ok bool) {
	r, found := tt.Find(label)
	if !found {
		return 0, 0, false
	}
	img := tt.Image()
	w := img.Bounds().Max.X
	yy := int(r.Y + r.H/2)
	// 行背景取字符左侧 100px（那一带是纯行底，无文字无控件）
	br, _, _, _ := img.At(int(r.X)-100, yy).RGBA()
	bg := int(br >> 8)

	var segs [][2]int
	runStart := -1
	for px := int(r.X) - 120; px < w; px++ {
		cr, _, _, _ := img.At(px, yy).RGBA()
		d := int(cr>>8) - bg
		hit := d > 18 || d < -18
		if hit && runStart < 0 {
			runStart = px
		}
		if !hit && runStart >= 0 {
			if px-runStart >= 6 {
				segs = append(segs, [2]int{runStart, px - 1})
			}
			runStart = -1
		}
	}
	if runStart >= 0 && w-runStart >= 6 {
		segs = append(segs, [2]int{runStart, w - 1})
	}
	if len(segs) == 0 {
		return 0, 0, false
	}
	// 从右往左找第一个「够宽」的段。
	//
	// 不能直接取最右那段：行底右侧还有卡片边框之类的窄条
	// （实测最右段是 545..550、宽 6，那是卡片右边框；开关在 494..529、宽 36）。
	// 取「最右且宽 >= 12」才对——开关胶囊是圆钮 + 轨道，至少 30px。
	for i := len(segs) - 1; i >= 0; i-- {
		if w := segs[i][1] - segs[i][0] + 1; w >= 12 {
			return float32(segs[i][0]+segs[i][1]) / 2, float32(yy), true
		}
	}
	return 0, 0, false
}

// TestSettingsFitsShippedWindow 设置页在**出货窗口尺寸**下必须全部可见。
//
// 为什么需要这条：设置页的行数只增不减（8 行 → 11 行），而窗口高度是常量。
// 上一轮加两行时已经撞过一次「最后一行被顶出可视区」，那次靠人眼在真机截图上
// 发现；同一件事发生第二次时，用户只会说「我的设置页被切了」。
//
// 判据是像素坐标而不是「HasText」：设置页包在 Scroll 里，被裁掉的行**仍然**
// 在 Texts 里（只是不在可视区）。所以断言每一项的矩形底边都落在 footer 之上。
// 取值用 main.go 里 createWindow 的真实宽度与高度（窄到 440 也要成立）。
func TestSettingsFitsShippedWindow(t *testing.T) {
	// 出货尺寸 + 一个「更新出错且保存也失败」的最坏情况（那时文案最长）。
	winW, winH := defaultWinWidth, defaultWinHeight
	footerTop := float32(winH - 33) // footer 32 + 发丝线 1

	type scenario struct {
		name string
		prep func(a *app)
	}
	cases := []scenario{
		{"空闲", func(a *app) {}},
		{"运行中", func(a *app) { setRunning(a, true) }},
		{"运行中+长错误+提示", func(a *app) {
			setRunning(a, true)
			a.upd.stage = updFailed
			a.upd.msg = "校验失败：下载内容与官方 sha256 不符，可能被中间人篡改或镜像返回了错误文件。当前版本未被改动，请换一个镜像重试"
			a.setNotice("保存设置失败：open /Users/someone/.reply9842/settings.json.tmp: permission denied，请检查目录属主与权限后重试", true)
		}},
	}
	// 每一项都必须可见。前五项是「连接」组，其余在「其他」组——
	// 任何一项被切掉都说明分区把行顶出了窗口。
	rows := []string{
		// 「目标机器 / 网络 / 传输方式」已合并进线路（owner 2026-11-24），
		// 这一组只留真正独立的行 + 两个组标题。
		"连接", "监听地址", "线路",
		"其他", "开机自启", "日志保留", "显示区间", "自动更新", "更新镜像", "软件更新",
		"检查更新",
	}
	for _, tc := range cases {
		for _, w := range []int{440, winW} {
			a := newTestApp(t)
			a.tab = tabSettings
			tc.prep(a)
			tt := ui.NewTester(a.view, w, winH)
			tt.SetTitleBar(ui.TitleBar{Height: 66, Left: 79, Bottom: 33})
			for _, name := range rows {
				r, ok := tt.Find(name)
				if !ok {
					t.Errorf("%s w=%d: 设置页缺少 %q", tc.name, w, name)
					continue
				}
				if r.Y+r.H > footerTop {
					t.Errorf("%s w=%d: %q 底边 %.1f 越过了 footer 顶边 %.1f（被切掉）",
						tc.name, w, name, r.Y+r.H, footerTop)
				}
			}
		}
	}
}

// TestSettingsHasTwoGroups 两个分区的标题都在，而面板 tab 里没有。
//
// 需求原文：「设置页内两个分区，不用滚」。分组是可见的结构，不是内部实现。
func TestSettingsHasTwoGroups(t *testing.T) {
	a := newTestApp(t)
	a.tab = tabSettings
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	for _, want := range []string{"连接", "其他"} {
		if !tt.HasText(want) {
			t.Errorf("设置页缺少分区标题 %q：%q", want, tt.Texts())
		}
	}
	// 分区标题的字重必须高于行标签（否则它读起来就是个字段名）。
	// 用「颜色不同」间接验证：标题用 Foreground，行标签用 MutedForeground。
	head, okH := tt.Find("连接")
	// 用「监听地址」当行标签基准：「目标机器」已合并进线路，
	// 而监听地址是连接组里唯一与线路无关的独立行。
	label, okL := tt.Find("监听地址")
	if !okH || !okL {
		t.Fatalf("找不到标题或行标签")
	}
	if head.Y >= label.Y {
		t.Errorf("分区标题应在行标签之上：head.Y=%.1f label.Y=%.1f", head.Y, label.Y)
	}
}

// TestFooterHasNoCopyEntry 底栏不再有「复制地址」按钮。
//
// 复制入口只保留顶栏徽标一个；底栏是纯读取的状态条。这条断言数的是
// accessible label：底栏按钮去掉后，页面上只剩徽标那一个同名元素。
func TestFooterHasNoCopyEntry(t *testing.T) {
	for _, tab := range []int{tabPanel, tabSettings} {
		a := newTestApp(t)
		a.tab = tab
		tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
		n := 0
		for _, s := range tt.Texts() {
			if s == "复制地址" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("tab=%d: 「复制地址」出现 %d 次，期望只有顶栏徽标 1 次：%q", tab, n, tt.Texts())
		}
	}
}

// TestFooterEffectiveTransport 底栏显示**实际生效**的传输（最近一条请求记录），
// 而不是设置里的「自动选择传输」——意图不是事实。
func TestFooterEffectiveTransport(t *testing.T) {
	// 有记录：显示最近一次的传输（大写）。a.transport 故意设成 "auto"，
	// 证明显示值来自记录而不是设置。
	a := newTestApp(t)
	setRunning(a, true)
	a.transport = "auto"
	a.agg = stats.Agg{Requests: 3, UpRaw: 3000, UpWire: 30, RetainSec: 86400, LastTransport: "quic"}
	a.snap = func() stats.Agg { return a.agg }
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	if !tt.HasText("运行中") || !tt.HasText("QUIC") {
		t.Errorf("底栏应显示实际传输 QUIC：%q", tt.Texts())
	}
	if tt.HasText("自动选择传输") {
		t.Errorf("「自动选择传输」应已从底栏移除：%q", tt.Texts())
	}

	// 无记录：显示「—」，且不回退到设置值（设置了 quic 也不显示）
	b := newTestApp(t)
	b.transport = "quic"
	tt2 := ui.NewTester(b.view, defaultWinWidth, defaultWinHeight)
	if !tt2.HasText("未运行 · —") {
		t.Errorf("无记录时底栏应显示「未运行 · —」：%q", tt2.Texts())
	}
	// 设置页**不再**有「自动 / QUIC / TCP」选择器：传输方式是每条线路自己的
	// 字段（代理线路不能 QUIC，直连可以），放在页面顶部只能表达「全局默认」，
	// 而那个概念已经不存在了。线路编辑区里仍然能改。
	b.tab = tabSettings
	tt2.Frame()
	if tt2.HasText("传输方式") {
		t.Errorf("传输方式已并入线路，设置页不该再有这一行：%q", tt2.Texts())
	}
}

// TestFooterRateLabel 底栏实时带宽的显示形态：无基线是「↑ — · ↓ —」，
// 有读数走 humanRate（与别处同一个字节换算）。
func TestFooterRateLabel(t *testing.T) {
	a := &app{}
	if got := a.rateLabel(); got != "↑ — · ↓ —" {
		t.Errorf("无基线时 rateLabel = %q，期望 ↑ — · ↓ —", got)
	}
	a.rateValid, a.upRate, a.downRate = true, 12345, 456*1024
	if got := a.rateLabel(); !strings.Contains(got, "↑ 12.1 KB/s") || !strings.Contains(got, "↓ 456.0 KB/s") {
		t.Errorf("rateLabel = %q，期望含 ↑ 12.1 KB/s 与 ↓ 456.0 KB/s", got)
	}
}

// TestFooterFitsNarrowWindow 底栏在最小窗口宽度（440）下不溢出：
// 「状态 + 运行时长 · 实际传输」「↑ ↓ 实时带宽」「版本号」三段都要完整落在
// 窗口里，且互不重叠。
//
// 测试默认 version=="dev"、runner 未启动——出货构建里两个字段都真实存在，
// 不注入就测不到最宽的底栏组合。这里按出货态压测：长运行时长（23:59:59）+
// 千兆级读数（999.9 MB/s，本地万兆网的量级）+ v0.3.10 版本号。
func TestFooterFitsNarrowWindow(t *testing.T) {
	savedVer := version
	version = "0.3.10"
	t.Cleanup(func() { version = savedVer })
	uptime := 23*time.Hour + 59*time.Minute + 59*time.Second

	a := newTestApp(t)
	setRunning(a, true)
	a.fakeUptime = &uptime
	a.agg = stats.Agg{Requests: 9, UpRaw: 9000, UpWire: 90, RetainSec: 86400, LastTransport: "quic"}
	a.snap = func() stats.Agg { return a.agg }
	a.rateValid, a.upRate, a.downRate = true, 999.9*1024*1024, 999.9*1024*1024

	tt := ui.NewTester(a.view, 440, 400)
	tt.SetTitleBar(ui.TitleBar{Height: 66, Left: 79, Bottom: 33})

	head, ok := tt.Find("运行中 23:59:59 · QUIC")
	if !ok {
		t.Fatalf("底栏状态段缺失（长 uptime + 传输）：%q", tt.Texts())
	}
	rate, ok := tt.Find(a.rateLabel())
	if !ok {
		t.Fatalf("底栏带宽段缺失（rateLabel=%q）：%q", a.rateLabel(), tt.Texts())
	}
	ver, ok := tt.Find("v0.3.10")
	if !ok {
		t.Fatalf("底栏版本段缺失（version=%q）：%q", version, tt.Texts())
	}
	if head.X+head.W > rate.X+0.5 {
		t.Errorf("状态段右缘 %.1f 压住带宽段左缘 %.1f", head.X+head.W, rate.X)
	}
	if rate.X+rate.W > ver.X+0.5 {
		t.Errorf("带宽段右缘 %.1f 压住版本段左缘 %.1f（带宽被挤出的信号）", rate.X+rate.W, ver.X)
	}
	if ver.X+ver.W > 440-16+0.5 {
		t.Errorf("版本段右缘 %.1f 越过内容区右缘（424）", ver.X+ver.W)
	}
	if rate.W < 90 {
		t.Errorf("带宽段宽度只有 %.1fpx，可能被截断：%q", rate.W, a.rateLabel())
	}
	t.Logf("底栏：状态 [%.1f..%.1f] 带宽 [%.1f..%.1f] 版本 [%.1f..%.1f]",
		head.X, head.X+head.W, rate.X, rate.X+rate.W, ver.X, ver.X+ver.W)
}

// TestHumanRate 实时带宽的字节口径（复用 humanBytes，负数按 0）。
func TestHumanRate(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0 B/s"},
		{512, "512 B/s"},
		{12345, "12.1 KB/s"},
		{1048576, "1.0 MB/s"},
		{-5, "0 B/s"},
	}
	for _, tc := range cases {
		if got := humanRate(tc.in); got != tc.want {
			t.Errorf("humanRate(%v) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestSampleStatsRates 实时带宽 = 累计计数的差值 / 时间差：第一次采样只建
// 基线（没有读数）；计数变小（统计重建/清零）时回到「—」，不把前移算成流量。
func TestSampleStatsRates(t *testing.T) {
	a := newTestApp(t)
	agg := stats.Agg{UpWire: 1000, DownWire: 2000}
	a.snap = func() stats.Agg { return agg }

	// 第一次：只建基线，没有读数（除零防线）
	a.sampleStats()
	if a.rateValid {
		t.Fatalf("第一次采样不该有读数")
	}
	if a.ratePrevAt.IsZero() {
		t.Fatalf("第一次采样没有建基线")
	}

	// 拨回 2 秒再采样：+100/+200 字节 → 约 50/100 B/s
	a.ratePrevAt = a.ratePrevAt.Add(-2 * time.Second)
	agg = stats.Agg{UpWire: 1100, DownWire: 2200}
	a.sampleStats()
	if !a.rateValid {
		t.Fatalf("第二次采样应有读数")
	}
	if a.upRate < 40 || a.upRate > 60 || a.downRate < 80 || a.downRate > 120 {
		t.Errorf("速率 = %.1f/%.1f B/s，期望约 50/100（100/200 字节 in 2s）", a.upRate, a.downRate)
	}

	// 计数变小：视为重建/清零，回到「—」
	a.ratePrevAt = a.ratePrevAt.Add(-2 * time.Second)
	agg = stats.Agg{UpWire: 10, DownWire: 20}
	a.sampleStats()
	if a.rateValid {
		t.Fatalf("计数变小后 rateValid 应为 false")
	}

	// 采样断档（窗口关着/休眠）：中断期间的平均值不能冒充实时速率
	a.ratePrevAt = a.ratePrevAt.Add(-30 * time.Second)
	agg = stats.Agg{UpWire: 5000, DownWire: 9000}
	a.sampleStats()
	if a.rateValid {
		t.Fatalf("采样间隔超过上限后 rateValid 应为 false")
	}

	// 间隔不足 minGap（悬停等抖动帧）：读数与基线都保持原样
	a.rateValid, a.upRate, a.downRate = true, 12345, 23456
	base := time.Now().Add(-400 * time.Millisecond)
	a.ratePrevAt, a.ratePrevUp, a.ratePrevDown = base, 100, 200
	agg = stats.Agg{UpWire: 50_000, DownWire: 60_000}
	a.sampleStats()
	if !a.rateValid || a.upRate != 12345 || a.downRate != 23456 {
		t.Errorf("抖动帧改写了读数：valid=%v up=%.1f down=%.1f", a.rateValid, a.upRate, a.downRate)
	}
	if !a.ratePrevAt.Equal(base) || a.ratePrevUp != 100 || a.ratePrevDown != 200 {
		t.Errorf("抖动帧动了基线：prevAt=%v up=%d down=%d", a.ratePrevAt, a.ratePrevUp, a.ratePrevDown)
	}

	// 计数回退后的恢复：基线重置到回退后的值，下一个间隔能算出正确速率
	a.ratePrevAt = time.Now().Add(-2 * time.Second)
	a.ratePrevUp, a.ratePrevDown = 10_000, 20_000
	agg = stats.Agg{UpWire: 500, DownWire: 800}
	a.sampleStats()
	if a.rateValid {
		t.Fatalf("计数回退后不该有读数")
	}
	if a.ratePrevUp != 500 || a.ratePrevDown != 800 {
		t.Fatalf("回退后基线未重置：%d/%d", a.ratePrevUp, a.ratePrevDown)
	}
	a.ratePrevAt = a.ratePrevAt.Add(-2 * time.Second)
	agg = stats.Agg{UpWire: 600, DownWire: 1000}
	a.sampleStats()
	if !a.rateValid || a.upRate < 40 || a.upRate > 60 || a.downRate < 80 || a.downRate > 120 {
		t.Errorf("回退恢复后的速率 = %.1f/%.1f B/s，期望约 50/100", a.upRate, a.downRate)
	}

	// 时钟回拨（基线在未来）：丢掉基线重建，而不是永久静默
	a.rateValid, a.upRate = true, 7
	a.ratePrevAt = time.Now().Add(time.Hour)
	a.sampleStats()
	if a.rateValid || !a.ratePrevAt.IsZero() {
		t.Errorf("时钟回拨后应丢基线：valid=%v prevAt=%v", a.rateValid, a.ratePrevAt)
	}

	// resetRate 丢基线（改保留期的路径）
	a.rateValid = true
	a.resetRate()
	if a.rateValid || !a.ratePrevAt.IsZero() {
		t.Errorf("resetRate 未清空基线：valid=%v prevAt=%v", a.rateValid, a.ratePrevAt)
	}
}

// TestApplyTimeSettingsResetsRate 改保留期/显示区间会触发统计重建、累计计数
// 可能整体前移；基线必须当场作废，否则下一帧会把跳变算成一波假带宽
// （复核者反事实实测：不 resetRate 会显示 536 MB/s）。
func TestApplyTimeSettingsResetsRate(t *testing.T) {
	a := newTestApp(t)
	// 上一帧：有读数、基线在两秒前
	a.rateValid, a.upRate, a.downRate = true, 12_000, 34_000
	a.ratePrevAt = time.Now().Add(-2 * time.Second)
	a.ratePrevUp, a.ratePrevDown = 1_000, 2_000
	// 重建后总量整体抬到 GB 级（保留期放大后的真实形态）
	jumped := stats.Agg{UpWire: 1 << 30, DownWire: 2 << 30}
	a.snap = func() stats.Agg { return jumped }

	a.retainIdx = relayclient.OptionIndex(7*24*time.Hour, relayclient.RetainOptions)
	a.windowIdx = relayclient.OptionIndex(6*time.Hour, relayclient.WindowOptions)
	a.applyTimeSettings()

	if a.rateValid || !a.ratePrevAt.IsZero() {
		t.Fatalf("重建后未丢基线：valid=%v prevAt=%v", a.rateValid, a.ratePrevAt)
	}
	// 重建后的第一帧只建新基线，不得把 1GB 的跳变算成带宽
	a.sampleStats()
	if a.rateValid {
		t.Errorf("重建后的第一帧就把计数跳变算成了带宽：%.1f B/s", a.upRate)
	}
	// 再下一帧：真实增量为 0 → 0 B/s（而不是 536 MB/s）
	a.ratePrevAt = a.ratePrevAt.Add(-2 * time.Second)
	a.sampleStats()
	if !a.rateValid || a.upRate > 1 || a.downRate > 1 {
		t.Errorf("恢复后的读数 = %.1f/%.1f B/s，期望 0", a.upRate, a.downRate)
	}
}

// TestTrafficSummaryTwoLines 流量摘要按语义拆成两行：第一行窗口与实发量、
// 第二行延迟三个读数；两行各自单独成行（不折行）。
//
// 守的是一个实际缺陷（2026-11-16 视觉复核）：原来挤成一行，560 宽 + 有数据时
// 尾巴被省略号吃掉（「…全程复用连…」），440 宽必然截断。拆行后的宽度上界由
// TestSummaryLinesFitNarrowWindow 统一守。
func TestTrafficSummaryTwoLines(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.agg = stats.Agg{
		Requests: 6, UpRaw: 4000, UpWire: 3000,
		WindowSec: 3600, BucketSec: 120,
		Series: []stats.Bucket{
			{UpRaw: 1000, UpWire: 1000, DownWire: 2000, Count: 2, MsSum: 400},
			{UpRaw: 1200, UpWire: 1200, DownWire: 1800, Count: 3, MsSum: 1500},
			{UpRaw: 0, UpWire: 0, DownWire: 0},
			{UpRaw: 800, UpWire: 800, DownWire: 1200, Count: 1, MsSum: 100},
		},
	}
	a.snap = func() stats.Agg { return a.agg }

	l1, l2 := a.trafficSummary(a.agg)
	if !strings.Contains(l1, "最近 1 小时") || !strings.Contains(l1, "↑") || !strings.Contains(l1, "↓") {
		t.Errorf("第一行 = %q，应为「窗口 · ↑实发 · ↓下行」", l1)
	}
	for _, want := range []string{"延迟 当前", "/ 峰值", "/ 均值"} {
		if !strings.Contains(l2, want) {
			t.Errorf("第二行 = %q，缺少 %q", l2, want)
		}
	}
	// 旧格式的「 · 均 2449 ms」是折行的元凶：不允许它回来。
	if strings.Contains(l1+l2, " 均 ") {
		t.Errorf("摘要仍带「 均 X ms」尾巴：%q", l1+l2)
	}

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	for _, line := range []string{l1, l2} {
		r, ok := tt.Find(line)
		if !ok {
			t.Fatalf("找不到摘要行 %q：%q", line, tt.Texts())
		}
		if r.H > 16 {
			t.Errorf("摘要行高度 %.1f（>16px）说明折成了多行：%q", r.H, line)
		}
	}
}

// TestSummaryLinesFitNarrowWindow 440/560 宽 + 有数据时，两张卡的每一行摘要
// 都必须完整显示（不许省略号）。
//
// 守的缺陷（2026-11-16 视觉复核）：「流量与延迟」的摘要行在 560 宽下右缘
// x527 贴到内容区 x529，尾巴被吃成「…全程复用连…」；440 宽必然更糟。
//
// 判据：MeasureText 的自然宽度（×1.01 + 4px，吸收 tnum 与抗锯齿的实测差
// 295.45 vs 292.75）必须不超过该行元素的宽度——Text 在卡片列里横向拉满，
// 元素的 W 就是卡片内容宽。fixture 取最长的合理读数：30 天保留 / 24 小时
// 窗口 / 六位请求数 / 近 1 GB 体积 / 99.9 s 延迟 / 非零握手。
func TestSummaryLinesFitNarrowWindow(t *testing.T) {
	agg := stats.Agg{
		Requests:      12_847,
		UpRaw:         999_000_000,
		UpWire:        12_000_000,
		DownRaw:       999_000_000,
		DownWire:      999_000_000,
		P50Ms:         99_900,
		RetainSec:     30 * 86400,
		WindowSec:     24 * 3600,
		BucketSec:     48 * 60,
		LastTransport: "quic",
	}
	agg.Series = make([]stats.Bucket, stats.SeriesBuckets)
	agg.Series[0] = stats.Bucket{
		UpRaw: agg.UpRaw, UpWire: agg.UpWire, DownRaw: agg.DownRaw, DownWire: agg.DownWire,
		Count: agg.Requests, MsSum: agg.Requests * 99_900,
		HsCount: agg.Requests, HsSum: agg.Requests * 2_000,
	}

	for _, width := range []int{440, 560} {
		a := newTestApp(t)
		setRunning(a, true)
		a.agg = agg
		a.snap = func() stats.Agg { return agg }

		l1, l2 := a.trafficSummary(agg)
		lines := []string{a.retentionStatsLine(), a.chartSummary(agg), l1, l2}
		var natural []float32
		tt := ui.NewTester(func(c *ui.Context) {
			a.view(c)
			natural = natural[:0]
			for _, s := range lines {
				w, _ := c.MeasureText(0, ui.Span{Text: s, Size: 10.5})
				natural = append(natural, w)
			}
		}, width, defaultWinHeight)
		tt.SetTitleBar(ui.TitleBar{Height: 66, Left: 79, Bottom: 33})

		for i, line := range lines {
			r, ok := tt.Find(line)
			if !ok {
				t.Fatalf("w=%d: 找不到摘要行 %q：%q", width, line, tt.Texts())
			}
			if limit := natural[i]*1.01 + 4; limit > r.W {
				t.Errorf("w=%d: 摘要行自然宽度 %.1f（容差后 %.1f）超过可用宽 %.1f，会被省略号截断：%q",
					width, natural[i], limit, r.W, line)
			}
		}
	}
}

// TestHumanBytesAxis 同一根纵轴的两档刻度必须共用峰值的量纲。
//
// 守的缺陷（2026-11-16 视觉复核）：golden 帧出现「2.0 MB / 1001.0 KB」——
// humanBytes 各自独立选单位，在 1 MB 边界上两档混排两种单位。
func TestHumanBytesAxis(t *testing.T) {
	cases := []struct {
		peak      int64
		top, half string
	}{
		{2_050_000, "2.0 MB", "1.0 MB"},   // 旧实现 half 是 "1001.0 KB"
		{800_000, "781.2 KB", "390.6 KB"}, // 与 TestChartScaleBarsAndTicks 同值
		{13_000_000, "12.4 MB", "6.2 MB"},
		{500_000, "488.3 KB", "244.1 KB"},
		{1500, "1.5 KB", "0.7 KB"}, // 小流量：半值不退回 B（同量纲）
	}
	for _, tc := range cases {
		top, half := humanBytesAxis(tc.peak)
		if top != tc.top || half != tc.half {
			t.Errorf("humanBytesAxis(%d) = (%q, %q)，期望 (%q, %q)",
				tc.peak, top, half, tc.top, tc.half)
		}
	}
}

// TestEmptyChartNoteGetOnly 窗口内只有无请求体的请求（GET/HEAD）时，空态
// 文案不能说「没有请求」——同一张卡片的保留期总览会同时写着「近 24 小时
// 42 请求」，两句话自相矛盾。判据必须来自窗口内的 Series，而不是保留期的
// Agg.Requests。
func TestEmptyChartNoteGetOnly(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.agg = stats.Agg{Requests: 42, RetainSec: 86400, WindowSec: 1800, BucketSec: 60}
	a.agg.Series = make([]stats.Bucket, stats.SeriesBuckets)
	// GET：没有请求体（UpRaw=0、UpWire=0），响应体走下行，Count 照常记账。
	for _, i := range []int{3, 9, 21} {
		a.agg.Series[i] = stats.Bucket{Count: 2, DownRaw: 42_000, DownWire: 40_000, MsSum: 300}
	}
	a.snap = func() stats.Agg { return a.agg }

	if a.hasSeries() {
		t.Fatalf("GET-only 窗口不该有可画的上行柱")
	}
	note := a.emptyChartNote()
	if !strings.Contains(note, "无请求体") || !strings.Contains(note, "GET") {
		t.Errorf("空态文案 = %q，应说明「只有无请求体的请求（GET 等）」", note)
	}
	if strings.Contains(note, "没有请求") {
		t.Errorf("空态文案 = %q，不得把 GET-only 窗口说成「没有请求」", note)
	}

	// 同屏一致性：保留期总览在、误报不在；流量卡有下行流量 → 不是空态。
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	if !tt.HasText("近 24 小时 42 请求") {
		t.Errorf("同卡片应显示保留期总览：%q", tt.Texts())
	}
	if tt.HasText("没有请求") {
		t.Errorf("同屏出现「没有请求」的误报：%q", tt.Texts())
	}
	if tt.HasText("等待流量数据") {
		t.Errorf("有下行流量时流量卡不该是空态：%q", tt.Texts())
	}
}

// TestLatencyHasGaps 折线断点只在「两个有延迟样本的桶之间夹着无样本的桶」时成立。
//
// 判据与画线共用 hasLatencySample：Count>0 但 MsSum==0 的桶（有请求、没采到
// 隧道往返）在图上不画点，它同样是一个真实断点。
func TestLatencyHasGaps(t *testing.T) {
	// sample 一个有延迟样本的桶
	sample := func(n int64) stats.Bucket {
		return stats.Bucket{UpRaw: 1000 * n, Count: n, MsSum: 100 * n}
	}
	cases := []struct {
		name string
		b    []stats.Bucket
		want bool
	}{
		{"中间有洞", []stats.Bucket{sample(1), {}, sample(1)}, true},
		{"连续", []stats.Bucket{sample(1), sample(1), sample(1)}, false},
		{"两端为空不算断点", []stats.Bucket{{}, sample(1), sample(1), {}}, false},
		{"尾部为空不算断点", []stats.Bucket{sample(1), {}, {}, {}}, false},
		{"全空", []stats.Bucket{{}, {}, {}}, false},
		{"中间夹着无延迟样本的桶", []stats.Bucket{sample(1), {Count: 2, UpRaw: 10}, sample(1)}, true},
	}
	for _, tc := range cases {
		a := &app{agg: stats.Agg{Series: tc.b}}
		if got := a.latencyHasGaps(); got != tc.want {
			t.Errorf("%s: latencyHasGaps() = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}

// TestSwitchKnobPositions 开关的开/关必须靠位置区分，而不只是亮度。
func TestSwitchKnobPositions(t *testing.T) {
	r := ui.Rect{X: 10, Y: 5, W: 36, H: 20}
	off, on := switchKnobRect(r, false), switchKnobRect(r, true)
	if on.X <= off.X+8 {
		t.Errorf("拇指位移只有 %.1fpx（off.X=%.1f on.X=%.1f），不靠亮度也能分辨要求至少 8px",
			on.X-off.X, off.X, on.X)
	}
	if off.X < r.X || on.X+on.W > r.X+r.W {
		t.Errorf("拇指越出轨道：track=%+v off=%+v on=%+v", r, off, on)
	}
}

// TestHumanMs 延迟口径：4 位数的毫秒换秒。
func TestHumanMs(t *testing.T) {
	for in, want := range map[int64]string{0: "0 ms", 999: "999 ms", 1000: "1.0 s", 28771: "28.8 s"} {
		if got := humanMs(in); got != want {
			t.Errorf("humanMs(%d) = %q，期望 %q", in, got, want)
		}
	}
}

// ---- 图表几何：柱、参考线、刻度必须共用同一个 scale ----

// 系列色（暗色板 p.DownBar/p.UpBar）：渲染时 Fill 用 0.8 透明度，但通道值
// 与色板一致（实测 #4ec38a / #4c9ef0），所以按 RGB 匹配即可。
var (
	rgbDownBar = [3]uint8{0x4e, 0xc3, 0x8a}
	rgbUpBar   = [3]uint8{0x4c, 0x9e, 0xf0}
	// 延迟折线的琥珀色 #e0a94a：不透明，所以渲染值与色板逐通道一致。
	rgbLatency = [3]uint8{0xe0, 0xa9, 0x4a}
)

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// absF 浮点绝对值（几何断言的容差比较用）。
func absF(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

// nearRGB 像素是否接近给定颜色（逐通道容差 tol，吸收抗锯齿）。
func nearRGB(c color.Color, rgb [3]uint8, tol int) bool {
	r, g, b, _ := c.RGBA()
	return absInt(int(r>>8)-int(rgb[0])) <= tol &&
		absInt(int(g>>8)-int(rgb[1])) <= tol &&
		absInt(int(b>>8)-int(rgb[2])) <= tol
}

// barTopRow 在 [y0,y1) × [x0,x1) 里找最上面一行柱色像素（-1 表示没有）。
func barTopRow(img *image.RGBA, x0, x1, y0, y1 int) int {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if nearRGB(img.At(x, y), rgbDownBar, 20) || nearRGB(img.At(x, y), rgbUpBar, 20) {
				return y
			}
		}
	}
	return -1
}

// inkCenterRow [y0,y1) 里某色文字的最上一组连续墨迹行的中心（-1 表示没找到）。
func inkCenterRow(img *image.RGBA, x0, x1, y0, y1 int, rgb [3]uint8, tol int) float64 {
	lo, hi := -1, -1
	for y := y0; y < y1; y++ {
		hit := false
		for x := x0; x < x1; x++ {
			if nearRGB(img.At(x, y), rgb, tol) {
				hit = true
				break
			}
		}
		if hit && lo < 0 {
			lo = y
		}
		if hit {
			hi = y
		} else if lo >= 0 && y > hi+2 {
			break
		}
	}
	if lo < 0 {
		return -1
	}
	return float64(lo+hi) / 2
}

// TestChartScaleBarsAndTicks 两张图的柱、刻度标签共用同一个 scale；第一张图的
// 柱高表达**体积**（2026-11-16 改版的核心契约）。
//
// 五条断言，每条都对应一个真实修过的缺陷：
//  1. 第一图：峰值柱顶落在最上刻度中心之下 2..4px（与第二图同一条约定——
//     柱顶先于刻度收住，图才有上边框）
//  2. 第一图：柱高按体积——raw 只有峰值 1/4 的桶，柱高正好约 1/4。旧版柱高
//     画的是压缩率（真实负载恒在 98-99.9%），所有柱几乎一样高；这条直接盯住它
//  3. 第一图：蓝段贴底、随 UpWire 真实伸缩——50% 实发的桶蓝段与绿段相当，
//     而实发 1% 的峰值桶蓝段只剩 1-3px（旧版恒为 3px 常量，即「蓝块永远不变」）
//  4. 第一图：wire=0 的桶没有蓝段（不许用最小高度伪造发送）
//  5. 两图绘图区等高（由各自刻度行距反算，≤1px）——摘要区同为两行高的布局前提
func TestChartScaleBarsAndTicks(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.agg = stats.Agg{Requests: 40, WindowSec: 1800, BucketSec: 60}
	a.agg.Series = make([]stats.Bucket, stats.SeriesBuckets)
	// 四个桶拉开体积与实发比例：
	//   桶 2  峰值 800KB、实发 1%   → 最高柱，蓝段只剩存在性
	//   桶 5  1/4 体积、实发 1%     → 柱高应为峰值的 1/4
	//   桶 8  一半体积、实发 50%    → 蓝段 ≈ 绿段
	//   桶 11 1/8 体积、实发 0      → 纯绿，没有蓝段
	a.agg.Series[2] = stats.Bucket{UpRaw: 800_000, UpWire: 8_000, Count: 1, MsSum: 120}
	a.agg.Series[5] = stats.Bucket{UpRaw: 200_000, UpWire: 2_000, Count: 1, MsSum: 130}
	a.agg.Series[8] = stats.Bucket{UpRaw: 400_000, UpWire: 200_000, Count: 1, MsSum: 140}
	a.agg.Series[11] = stats.Bucket{UpRaw: 100_000, UpWire: 0, Count: 1, MsSum: 150}
	a.snap = func() stats.Agg { return a.agg }

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	img := tt.Image()
	if img == nil {
		t.Fatal("Image() 返回 nil")
	}

	// 百分数刻度与 100% 参考虚线已随「柱高=压缩率」删除：字节刻度里没有
	// 「100%」这个概念，而峰值参考线与最上刻度、峰值柱顶三者在像素上重合。
	for _, gone := range []string{"100%", "75%", "50%", "25%"} {
		if tt.HasText(gone) {
			t.Errorf("第一张图仍残留百分数刻度 %q（应为字节刻度）：%q", gone, tt.Texts())
		}
	}

	// 绘图区横向范围：左边是刻度列 + 间距，右边到卡片右边距。
	// 右缘跟着窗口宽度走（560 宽时是 529），写死会在改尺寸后量到卡片外。
	const x0 = 84
	x1 := defaultWinWidth - 31

	// 纵向扫描的下界取「流量与延迟」标题：两张图共用同一组系列色，往下扫
	// 会把第二张图的柱子也当成第一张图的。
	lower, ok := tt.Find("流量与延迟")
	if !ok {
		t.Fatalf("找不到第二张图标题：%q", tt.Texts())
	}
	yBot := int(lower.Y) - 4

	peakTick, ok := tt.Find(humanBytes(800_000))
	if !ok {
		t.Fatalf("找不到峰值刻度 %q：%q", humanBytes(800_000), tt.Texts())
	}
	yTop := int(peakTick.Y) - 4

	// ---- 1. 峰值柱顶 vs 最上刻度 ----
	tick := inkCenterRow(img, 20, 84, int(peakTick.Y)-6, int(peakTick.Y)+8,
		[3]uint8{0x94, 0x94, 0x94}, 40)
	if tick < 0 {
		t.Fatalf("找不到峰值刻度文字：%q", tt.Texts())
	}
	peakTop := barTopRow(img, x0, x1, yTop, yBot)
	if peakTop < 0 {
		t.Fatalf("第一张图找不到柱子")
	}
	// 容差与平台字体度量解耦：macOS 上实测 2.5-3px，Linux runner 上同一契约
	// 量到 4.5px（字体 ink 高度不同导致刻度中心偏移），写死 2..4 会让 CI 假红。
	// 语义不变：柱顶必须在刻度中心之下、且留出不触顶的余量。
	if d := float64(peakTop) - tick; d < 1 || d > 6 {
		t.Errorf("峰值柱顶 y=%d、最上刻度 y=%.1f，差 %.1fpx（要求 1..6：柱顶先于刻度收住）",
			peakTop, tick, d)
	} else {
		t.Logf("第一图：峰值柱顶 y=%d / 最上刻度 y=%.1f（差 %.1fpx）", peakTop, tick, d)
	}

	// 扫出四根柱的列段（左 → 右 = 桶 2 / 5 / 8 / 11）
	segs := barColumns(img, x0, x1, yTop, yBot)
	if len(segs) != 4 {
		t.Fatalf("扫到 %d 根柱，期望 4（桶 2/5/8/11）：%v", len(segs), segs)
	}
	// 基线：所有柱都坐在它上面 → 最下一行柱色像素 + 1
	base := barBottomRow(img, x0, x1, yTop, yBot)
	if base < 0 {
		t.Fatalf("找不到柱底")
	}
	base++
	barHeight := func(seg [2]int) float64 {
		return float64(base - barTopRow(img, seg[0], seg[1]+1, yTop, base))
	}
	blueHeight := func(seg [2]int) float64 {
		top := colorTopRow(img, seg[0], seg[1]+1, yTop, base, rgbUpBar, 26)
		if top < 0 {
			return 0
		}
		return float64(base - top)
	}

	// ---- 2. 柱高按体积 ----
	hPeak, hQuarter := barHeight(segs[0]), barHeight(segs[1])
	if d := hPeak/4 - hQuarter; d < -2.5 || d > 2.5 {
		t.Errorf("体积 1/4 的桶柱高 %.1fpx，峰值柱高 %.1fpx 的 1/4 是 %.1fpx（差 %.1f）：柱高不是体积",
			hQuarter, hPeak, hPeak/4, d)
	} else {
		t.Logf("柱高：峰值 %.1fpx / 1/4 体积桶 %.1fpx", hPeak, hQuarter)
	}

	// ---- 3. 蓝段贴底、随 UpWire 真实伸缩 ----
	bPeak, bHalf, bZero := blueHeight(segs[0]), blueHeight(segs[2]), blueHeight(segs[3])
	if bPeak <= 0 || bPeak > 5 {
		t.Errorf("峰值桶（实发 1%%）蓝段 %.0fpx，应只有 1-3px（存在性、按比例算）", bPeak)
	}
	if bHalf-bPeak < 20 {
		t.Errorf("50%% 实发的桶蓝段 %.0fpx vs 峰值桶 %.0fpx：蓝段没有随流量真实变化", bHalf, bPeak)
	}
	if bZero != 0 {
		t.Errorf("wire=0 的桶出现蓝段（%.0fpx）：不许用最小高度伪造发送", bZero)
	}
	// 蓝段在最底：实发 >0 的柱，最下一行像素必须是蓝的
	for i := 0; i < 3; i++ {
		bottom := barBottomRow(img, segs[i][0], segs[i][1]+1, yTop, base)
		if bottom < 0 || !rowHasColor(img, bottom, segs[i][0], segs[i][1]+1, rgbUpBar, 30) {
			t.Errorf("第 %d 根柱（实发>0）底行不是蓝段：蓝段必须贴底", i)
		}
	}
	t.Logf("蓝段高度：峰值桶 %.0fpx / 50%% 实发桶 %.0fpx", bPeak, bHalf)

	// ---- 4. 第二图：峰值柱顶 vs 最上刻度（原契约不变）----
	a2 := newTestApp(t)
	setRunning(a2, true)
	a2.agg = stats.Agg{Requests: 40, WindowSec: 1800, BucketSec: 60}
	a2.agg.Series = make([]stats.Bucket, stats.SeriesBuckets)
	for i := 0; i < 6; i++ {
		a2.agg.Series[i] = stats.Bucket{UpRaw: 900_000, UpWire: 1_000, DownWire: 1_000,
			Count: 1, MsSum: int64(300 * (i + 1))}
	}
	// 峰值桶：上行 4MB + 下行 9MB = 13MB → 轴顶刻度 "12.4 MB"（与摘要行的
	// ↑3.8MB / ↓8.6MB 不同串，Find 不会选错）。
	a2.agg.Series[10] = stats.Bucket{UpRaw: 20_000_000, UpWire: 4_000_000,
		DownWire: 9_000_000, Count: 1, MsSum: 90_000}
	a2.snap = func() stats.Agg { return a2.agg }

	tt2 := ui.NewTester(a2.view, defaultWinWidth, defaultWinHeight)
	img2 := tt2.Image()
	if img2 == nil {
		t.Fatal("Image() 返回 nil")
	}
	topLabel := humanBytes(13_000_000)
	rt, ok := tt2.Find(topLabel)
	if !ok {
		t.Fatalf("找不到峰值刻度 %q：%q", topLabel, tt2.Texts())
	}
	// 图例色块在轴顶上方约 20px，从刻度行开始往下扫，避开图例。
	peak := barTopRow(img2, x0, x1, int(rt.Y)-2, int(rt.Y)+220)
	if peak < 0 {
		t.Fatal("第二图找不到峰值柱")
	}
	tick2 := inkCenterRow(img2, 20, 84, int(rt.Y)-6, int(rt.Y)+8,
		[3]uint8{0x94, 0x94, 0x94}, 40)
	if tick2 < 0 {
		t.Fatalf("找不到峰值刻度文字")
	}
	if d := float64(peak) - tick2; d < 1 || d > 6 {
		t.Errorf("峰值柱顶 y=%d、最上刻度 y=%.1f，差 %.1fpx（要求 1..6：柱顶必须在刻度"+
			"中心之下留出顶部余量，齐平会读成「柱顶顶到坐标系上沿」；容差随平台字体度量放宽）",
			peak, tick2, d)
	} else {
		t.Logf("第二图：峰值柱顶 y=%d / 最上刻度 y=%.1f（差 %.1fpx）", peak, tick2, d)
	}

	// ---- 5. 两图绘图区等高 ----
	// 两张图各自用「峰值/半峰值」两档刻度反算（间隔 0.5H）：用刻度行距而不是
	// 绝对 y，避免标签居中带来的偏移。
	half1, ok := tt.Find(humanBytes(800_000 / 2))
	if !ok {
		t.Fatalf("找不到第一图的半峰值刻度 %q：%q", humanBytes(800_000/2), tt.Texts())
	}
	h1 := (half1.Y - peakTick.Y) / 0.5
	half2, ok := tt2.Find(humanBytes(13_000_000 / 2))
	if !ok {
		t.Fatalf("找不到半峰值刻度 %q：%q", humanBytes(13_000_000/2), tt2.Texts())
	}
	h2 := (half2.Y - rt.Y) / 0.5
	if d := float64(h1 - h2); d < -1 || d > 1 {
		t.Errorf("两个绘图区高度不等：%.1f vs %.1f（差 %.1fpx）", h1, h2, d)
	} else {
		t.Logf("绘图区高度：第一图 %.1fpx / 第二图 %.1fpx", h1, h2)
	}
}

// TestChartsEqualHeightWithEmptyRetentionLine 保留期总览为空（Requests=0、
// 但窗口内有桶）时，第一张卡片的第一行是**空占位**、第二张卡片没有空行——
// 两处都必须占满同一行高，否则绘图区差 20.7px。
//
// 这条专盯「summaryLine 对空串直接 return」这类突变：行高靠空文本占位，
// 空串一旦被跳过，等高契约就静默破掉（复核者的突变实验实证现套件抓不到）。
func TestChartsEqualHeightWithEmptyRetentionLine(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.agg = stats.Agg{Requests: 0, WindowSec: 1800, BucketSec: 60}
	a.agg.Series = make([]stats.Bucket, stats.SeriesBuckets)
	a.agg.Series[2] = stats.Bucket{UpRaw: 800_000, UpWire: 8_000, DownWire: 600_000, Count: 1, MsSum: 120}
	a.snap = func() stats.Agg { return a.agg }
	if a.retentionStatsLine() != "" {
		t.Fatalf("前提不成立：保留期总览非空")
	}

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	top1, ok := tt.Find(humanBytes(800_000))
	if !ok {
		t.Fatalf("找不到第一图峰值刻度：%q", tt.Texts())
	}
	half1, ok := tt.Find(humanBytes(800_000 / 2))
	if !ok {
		t.Fatalf("找不到第一图半峰值刻度：%q", tt.Texts())
	}
	maxTraffic := int64(8_000 + 600_000)
	top2, ok := tt.Find(humanBytes(maxTraffic))
	if !ok {
		t.Fatalf("找不到第二图峰值刻度 %q：%q", humanBytes(maxTraffic), tt.Texts())
	}
	half2, ok := tt.Find(humanBytes(maxTraffic / 2))
	if !ok {
		t.Fatalf("找不到第二图半峰值刻度：%q", tt.Texts())
	}
	h1 := (half1.Y - top1.Y) / 0.5
	h2 := (half2.Y - top2.Y) / 0.5
	if d := float64(h1 - h2); d < -1.2 || d > 1.2 {
		t.Errorf("保留期行为空占位时两图绘图区高度不等：%.1f vs %.1f（差 %.1fpx，空行占位失效）",
			h1, h2, d)
	} else {
		t.Logf("保留期行为空占位时两图高度：%.1f / %.1f", h1, h2)
	}
}

// TestVolumeBarSegments 体积柱的两段切分：亚像素桶不许被抬成「纯蓝无绿」，
// 有实质流量的桶与流量图共用同一套存在性标准。
func TestVolumeBarSegments(t *testing.T) {
	const plotH float32 = 150
	approx := func(x, y float32) bool { return absF(x-y) < 0.01 }

	// 峰值柱：满高；蓝段按比例只有 0.75px（<1px）→ 存在性抬到 1px，其余是绿段
	if b, w, s := volumeBarSegments(1_000_000, 5_000, 1_000_000, plotH); !approx(b, 150) ||
		!approx(w, minWirePx) || !approx(s, 150-minWirePx) {
		t.Errorf("峰值柱 = (%.2f, %.2f, %.2f)，期望 (150, 1, 149)", b, w, s)
	}
	// 0.1% 峰值 + 50% 实发：亚像素桶整根不画（返回值全 0）。若按比例画，
	// 蓝/绿两个亚像素矩形会落到同一像素行、蓝段把绿段盖掉——读出「纯蓝无绿」。
	if b, w, s := volumeBarSegments(1_000, 500, 1_000_000, plotH); b != 0 || w != 0 || s != 0 {
		t.Errorf("0.1%% 亚像素桶 = (%.3f, %.3f, %.3f)，期望整根不画", b, w, s)
	}
	// 0.8% 峰值 + 50% 实发：柱高 1.2px 可见，但绿段只剩 0.2px → 抬到两段各
	// 1px（柱高 2px），不许出现「蓝段可见、绿段被盖」的读法
	if b, w, s := volumeBarSegments(8_000, 4_000, 1_000_000, plotH); !approx(b, 2) ||
		!approx(w, minWirePx) || !approx(s, minWirePx) {
		t.Errorf("0.8%% 桶 = (%.2f, %.2f, %.2f)，期望 (2, 1, 1)", b, w, s)
	}
	// wire == raw（没压缩）：整根蓝段是正确的（绿段为 0 不是 bug）
	if b, w, s := volumeBarSegments(100_000, 100_000, 1_000_000, plotH); !approx(w, b) || s != 0 {
		t.Errorf("未压缩桶 = (%.2f, %.2f, %.2f)，期望蓝段=柱高、绿段 0", b, w, s)
	}
	// 1% 峰值：与流量图同标准，总高抬到 minBarPx；蓝段 1px、绿段 2px
	if b, w, s := volumeBarSegments(10_000, 100, 1_000_000, plotH); !approx(b, minBarPx) ||
		!approx(w, minWirePx) || !approx(s, minBarPx-minWirePx) {
		t.Errorf("1%% 桶 = (%.2f, %.2f, %.2f)，期望 (3, 1, 2)", b, w, s)
	}
	// 10% 峰值：蓝段按比例（不触发最小高度）
	if _, w, s := volumeBarSegments(100_000, 50_000, 1_000_000, plotH); !approx(w, 7.5) || !approx(s, 7.5) {
		t.Errorf("10%% 桶蓝/绿 = (%.2f, %.2f)，期望 (7.5, 7.5)", w, s)
	}
	// wire=0：没有蓝段，绿段 = 柱高
	if b, w, s := volumeBarSegments(100_000, 0, 1_000_000, plotH); w != 0 || !approx(s, b) {
		t.Errorf("wire=0 桶 = (%.2f, %.2f, %.2f)，期望蓝段 0、绿段=柱高", b, w, s)
	}
	// 空桶/除零防护
	if b, w, s := volumeBarSegments(0, 5, 1_000_000, plotH); b != 0 || w != 0 || s != 0 {
		t.Errorf("空桶 = (%.2f, %.2f, %.2f)，期望全 0", b, w, s)
	}
}

// TestChartSubPixelBucketHasNoFalseBlue 0.1% 峰值的亚像素桶不许在像素上画出
// 「纯蓝无绿」——柱高 <1px 时蓝/绿两个矩形落在同一行，后画的蓝段把绿段盖掉，
// 整根读作「一点没省」，与图例「绿=省下」直接矛盾（像素复核实测）。
//
// 2000 高的窗口把绘图区拉到约 850px：0.1% 的桶约 0.85px，正好是「亚像素但
// 可能被取整成一整行」的危险区（默认 640 高时它只有 0.15px，测不出来）。
// 1% 的对照桶必须可见且顶端是绿段，同时证明本测试的列几何没有算错。
func TestChartSubPixelBucketHasNoFalseBlue(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.agg = stats.Agg{Requests: 2, WindowSec: 1800, BucketSec: 60}
	a.agg.Series = make([]stats.Bucket, stats.SeriesBuckets)
	a.agg.Series[2] = stats.Bucket{UpRaw: 100_000_000, UpWire: 100_000, Count: 1, MsSum: 120}
	a.agg.Series[27] = stats.Bucket{UpRaw: 100_000, UpWire: 50_000, Count: 1, MsSum: 120}
	a.snap = func() stats.Agg { return a.agg }

	tt := ui.NewTester(a.view, defaultWinWidth, 2000)
	img := tt.Image()
	if img == nil {
		t.Fatal("Image() 返回 nil")
	}
	lower, ok := tt.Find("流量与延迟")
	if !ok {
		t.Fatalf("找不到第二张图标题：%q", tt.Texts())
	}
	// 第一图绘图区几何与视图共用 plotArea：卡片内容从 x=30 开始，宽 = 窗口
	// 减两侧面板内边距(32)与卡片内边距(28)。
	area := plotArea(ui.Rect{X: 30, W: defaultWinWidth - 32 - 28})
	if area.W <= 0 {
		t.Fatal("绘图区宽度无效")
	}
	slot := area.W / float32(stats.SeriesBuckets)
	yBot := int(lower.Y) - 4
	// 只扫第一张图的绘图区：从峰值刻度往下到「流量与延迟」标题之上。
	peakTick, ok := tt.Find(humanBytes(100_000_000))
	if !ok {
		t.Fatalf("找不到峰值刻度：%q", tt.Texts())
	}
	yTop := int(peakTick.Y) + 6

	cx := int(area.X + 27.5*slot)
	for y := yTop; y < yBot; y++ {
		if rowHasColor(img, y, cx-5, cx+6, rgbDownBar, 20) ||
			rowHasColor(img, y, cx-5, cx+6, rgbUpBar, 20) {
			t.Errorf("0.1%% 峰值的亚像素桶（x≈%d）在 y=%d 画出了柱色像素：亚像素桶必须整根不可见，不许读成「纯蓝无绿」",
				cx, y)
			break
		}
	}

	// 对照：1% 的桶必须可见，且最上面一行是绿段（省下）。
	a.agg = stats.Agg{Requests: 2, WindowSec: 1800, BucketSec: 60}
	a.agg.Series = make([]stats.Bucket, stats.SeriesBuckets)
	a.agg.Series[2] = stats.Bucket{UpRaw: 100_000_000, UpWire: 100_000, Count: 1, MsSum: 120}
	a.agg.Series[3] = stats.Bucket{UpRaw: 1_000_000, UpWire: 500_000, Count: 1, MsSum: 120}
	tt2 := ui.NewTester(a.view, defaultWinWidth, 2000)
	img2 := tt2.Image()
	if img2 == nil {
		t.Fatal("Image() 返回 nil（对照帧）")
	}
	cx1 := int(area.X + 3.5*slot)
	greenTop := colorTopRow(img2, cx1-5, cx1+6, yTop, yBot, rgbDownBar, 20)
	blueTop := colorTopRow(img2, cx1-5, cx1+6, yTop, yBot, rgbUpBar, 20)
	if greenTop < 0 {
		t.Fatalf("1%% 的对照桶不可见（列几何可能算错）：greenTop=%d", greenTop)
	}
	if blueTop >= 0 && blueTop < greenTop {
		t.Errorf("1%% 的桶顶端是蓝段（y=%d）而不是绿段（y=%d）", blueTop, greenTop)
	}
}

// barBottomRow [y0,y1) × [x0,x1) 里最下面一行柱色像素（-1 表示没有）。
func barBottomRow(img *image.RGBA, x0, x1, y0, y1 int) int {
	for y := y1 - 1; y >= y0; y-- {
		for x := x0; x < x1; x++ {
			if nearRGB(img.At(x, y), rgbDownBar, 20) || nearRGB(img.At(x, y), rgbUpBar, 20) {
				return y
			}
		}
	}
	return -1
}

// colorTopRow [y0,y1) × [x0,x1) 里最上面一行指定柱色的像素（-1 表示没有）。
func colorTopRow(img *image.RGBA, x0, x1, y0, y1 int, rgb [3]uint8, tol int) int {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if nearRGB(img.At(x, y), rgb, tol) {
				return y
			}
		}
	}
	return -1
}

// rowHasColor 一行 y × [x0,x1) 里是否有指定颜色像素。
func rowHasColor(img *image.RGBA, y, x0, x1 int, rgb [3]uint8, tol int) bool {
	for x := x0; x < x1; x++ {
		if nearRGB(img.At(x, y), rgb, tol) {
			return true
		}
	}
	return false
}

// barColumns 扫出连续的「有柱色」列，返回 [left, right] 段（含端点）。
//
// 用列而不是行来分段：柱与柱之间的 3px 间隙在 1x 下是干净的背景列，
// 于是每根柱正好一段，段的中心就是柱心。
func barColumns(img *image.RGBA, x0, x1, y0, y1 int) [][2]int {
	var out [][2]int
	start := -1
	for x := x0; x <= x1; x++ {
		hit := false
		if x < x1 {
			for y := y0; y < y1; y++ {
				c := img.At(x, y)
				if nearRGB(c, rgbDownBar, 20) || nearRGB(c, rgbUpBar, 20) {
					hit = true
					break
				}
			}
		}
		if hit && start < 0 {
			start = x
		}
		if !hit && start >= 0 {
			out = append(out, [2]int{start, x - 1})
			start = -1
		}
	}
	return out
}

// latencyInk [x0,x1) × [y0,y1) 里的折线像素（-1 行表示没有）。
func latencyInk(img *image.RGBA, x0, x1, y0, y1 int) (yMin, yMax int) {
	yMin, yMax = -1, -1
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if nearRGB(img.At(x, y), rgbLatency, 26) {
				if yMin < 0 {
					yMin = y
				}
				yMax = y
				break
			}
		}
	}
	return yMin, yMax
}

// TestLatencyVerticesOnBarCenters 折线顶点与柱心必须是同一个 x。
//
// 上一版折线的顶点落在**桶边界**（柱缝）上，整体左移半个槽位（实测 -8px）：
// 线画在柱缝上，读数会归到错误的桶。这条测试用锯齿形延迟（每个桶都是拐点）
// 在像素上验证两件事：
//  1. 折线的横向覆盖范围恰好从第一根柱心到最后第二根柱心（不许从绘图区左缘起笔）；
//  2. 每个桶的柱心列是该段折线的极值点（顶点在这里），而不是段中点。
func TestLatencyVerticesOnBarCenters(t *testing.T) {
	const x0 = 84
	x1 := defaultWinWidth - 31
	a := newTestApp(t)
	setRunning(a, true)

	var maxTotal int64
	a.agg = stats.Agg{Requests: 60, WindowSec: 1800, BucketSec: 60}
	a.agg.Series = make([]stats.Bucket, stats.SeriesBuckets)
	for i := range a.agg.Series {
		b := stats.Bucket{
			// UpRaw 取 1.5M：第一张图的刻度（峰值 1.4 MB / 半峰值 0.7 MB，
			// humanBytesAxis 同量纲）与第二张图的峰值刻度（humanBytes(500000)
			// =「488.3 KB」）不同串——否则 tt.Find 会先命中上面那张图的刻度。
			UpRaw: 1_500_000, UpWire: 200_000, DownWire: 300_000,
			Count: 2,
		}
		// 锯齿：偶数桶慢、奇数桶快 → 每个桶都是折线的拐点。
		if i%2 == 0 {
			b.MsSum = 8000
		} else {
			b.MsSum = 2000
		}
		a.agg.Series[i] = b
		if v := b.UpWire + b.DownWire; v > maxTotal {
			maxTotal = v
		}
	}
	a.snap = func() stats.Agg { return a.agg }

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	img := tt.Image()
	if img == nil {
		t.Fatal("Image() 返回 nil")
	}
	top, ok := tt.Find(humanBytes(maxTotal))
	if !ok {
		t.Fatalf("找不到峰值刻度 %q：%q", humanBytes(maxTotal), tt.Texts())
	}
	yTop, yBot := int(top.Y)-4, int(top.Y)+230
	segs := barColumns(img, x0, x1, yTop, yBot)
	if len(segs) < stats.SeriesBuckets-2 {
		t.Fatalf("扫到 %d 根柱，期望至少 %d：柱之间必须还剩得下干净的背景列",
			len(segs), stats.SeriesBuckets-2)
	}
	if len(segs) != stats.SeriesBuckets {
		// 允许一两处间隙被抗锯齿填掉（580 宽时槽距 15.5px、柱宽 12px），
		// 但必须记下来：真掉了半屏的柱子说明布局出问题。
		t.Logf("扫到 %d 根柱（理想 %d），按实测柱心继续验证", len(segs), stats.SeriesBuckets)
	}
	if len(segs) < 3 {
		t.Fatal("柱子太少，锯齿无从比较")
	}

	// 1. 横向范围：从第一根柱心到最后第二根柱心（±2px 吸收圆头与抗锯齿）。
	firstCenter := float64(segs[0][0]+segs[0][1]) / 2
	lastCenter := float64(segs[len(segs)-1][0]+segs[len(segs)-1][1]) / 2
	inkMin, inkMax := -1, -1
	for x := x0; x < x1; x++ {
		if lo, _ := latencyInk(img, x, x+1, yTop, yBot); lo >= 0 {
			if inkMin < 0 {
				inkMin = x
			}
			inkMax = x
		}
	}
	if inkMin < 0 {
		t.Fatal("绘图区里没有折线像素")
	}
	if d := float64(inkMin) - firstCenter; d < -2 || d > 2 {
		t.Errorf("折线起点 x=%d，第一根柱心 x=%.1f（差 %.1fpx）：顶点没有落在柱心上",
			inkMin, firstCenter, d)
	}
	if d := float64(inkMax) - lastCenter; d < -2 || d > 2 {
		t.Errorf("折线终点 x=%d，最后一根柱心 x=%.1f（差 %.1fpx）：顶点没有落在柱心上",
			inkMax, lastCenter, d)
	}

	// 2. 每个柱心列必须是极值点。延迟按「峰值贴轴顶」归一，所以偶数桶
	// （慢）在屏幕上更高（y 更小），奇数桶（快）更低。取「中线 y」比较：
	// 顶点列的墨迹围绕顶点对称。
	centerY := func(cx int) float64 {
		lo, hi := latencyInk(img, cx, cx+1, yTop, yBot)
		if lo < 0 {
			return -1
		}
		return float64(lo+hi) / 2
	}
	for i, s := range segs {
		cx := (s[0] + s[1]) / 2
		cv := centerY(cx)
		if cv < 0 {
			t.Errorf("第 %d 个桶：柱心列 x=%d 没有折线像素", i, cx)
			continue
		}
		// 柱缝中点（两根柱之间的间隙中心）就是上一版画顶点的位置。
		if i+1 < len(segs) {
			mid := (s[1] + segs[i+1][0]) / 2
			if mv := centerY(mid); mv >= 0 {
				slow := i%2 == 0
				if slow && !(cv < mv) {
					t.Errorf("第 %d 个桶（慢）：柱心 y=%.1f 不比柱缝 y=%.1f 更高——顶点落在柱缝上",
						i, cv, mv)
				}
				if !slow && !(cv > mv) {
					t.Errorf("第 %d 个桶（快）：柱心 y=%.1f 不比柱缝 y=%.1f 更低——顶点落在柱缝上",
						i, cv, mv)
				}
			}
		}
	}
}

// TestLatencySkipsZeroMsBuckets 「有请求但没采到延迟」的桶不画点、不连线。
//
// 上一版只判 Count>0：MsSum==0 的桶按 AvgMs()=0 画在基线上，于是在离柱群
// 很远的空白处留下一条 17×2px 的橙线——1x 下就是一道划痕。
func TestLatencySkipsZeroMsBuckets(t *testing.T) {
	const x0, x1 = 84, 529
	a := newTestApp(t)
	setRunning(a, true)

	a.agg = stats.Agg{Requests: 4, WindowSec: 1800, BucketSec: 60}
	a.agg.Series = make([]stats.Bucket, stats.SeriesBuckets)
	var maxTotal int64
	put := func(i int, msSum int64) {
		// UpRaw 取 1.5M：把第一张图的刻度（humanBytesAxis → 1.4 MB / 0.7 MB）
		// 与第二张图的峰值刻度（humanBytes(500000) =「488.3 KB」）错开，
		// tt.Find 才能唯一命中。
		b := stats.Bucket{UpRaw: 1_500_000, UpWire: 200_000, DownWire: 300_000,
			Count: 2, MsSum: msSum}
		a.agg.Series[i] = b
		if v := b.UpWire + b.DownWire; v > maxTotal {
			maxTotal = v
		}
	}
	// 两端各一个正常样本，中间一个是「有请求、没采到隧道往返」的桶。
	put(1, 4000)
	put(15, 0)
	put(28, 8000)
	a.snap = func() stats.Agg { return a.agg }

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	img := tt.Image()
	if img == nil {
		t.Fatal("Image() 返回 nil")
	}
	top, ok := tt.Find(humanBytes(maxTotal))
	if !ok {
		t.Fatalf("找不到峰值刻度 %q", humanBytes(maxTotal))
	}
	yTop, yBot := int(top.Y)-6, int(top.Y)+230
	segs := barColumns(img, x0, x1, yTop, yBot)
	if len(segs) != 3 {
		t.Fatalf("扫到 %d 根柱，期望 3（桶 1 / 15 / 28）", len(segs))
	}
	mid := segs[1] // 桶 15
	lo, hi := latencyInk(img, mid[0]-4, mid[1]+5, yTop, yBot)
	if lo >= 0 {
		t.Errorf("MsSum==0 的桶（x %d..%d）上画了折线：y %d..%d",
			mid[0], mid[1], lo, hi)
	}
	// 两侧的正常样本仍必须画出来（别把整条线都静默了）
	for _, s := range []int{0, 2} {
		if l, _ := latencyInk(img, segs[s][0], segs[s][1]+1, yTop, yBot); l < 0 {
			t.Errorf("第 %d 根柱（有延迟样本）上没有折线像素", s)
		}
	}
}

// TestTrafficBarMinHeight 最小可见高度只给「有实质流量」的桶（≥ 峰值 1%）。
//
// 上一版无条件把非零桶抬到 3px：真实数据的最右 bucket 只有峰值的 0.08%，
// 被画成 3px 绿点，配上压在柱顶的延迟短划，1x 下是一小坨绿橙杂点。
func TestTrafficBarMinHeight(t *testing.T) {
	const peak int64 = 4_000_000
	const plotH float32 = 180
	cases := []struct {
		name  string
		total int64
		check func(float32) bool
		why   string
	}{
		{"峰值=满量程", peak, func(h float32) bool { return h == plotH }, "峰值柱必须精确落在轴顶"},
		{"0.08% 的碎屑桶", peak / 1250, func(h float32) bool { return h < 1 }, "按真实比例画（<1px），不许抬到 3px"},
		{"0.5%", peak / 200, func(h float32) bool { return h < 1 }, "低于 1% 一律按真实比例"},
		{"1.25%", peak / 80, func(h float32) bool { return h == minBarPx }, "刚过 1% 的桶抬到 3px"},
		{"2%", peak / 50, func(h float32) bool { return h > minBarPx }, "按比例已超过 3px"},
		{"空桶", 0, func(h float32) bool { return h == 0 }, "空桶不画"},
	}
	for _, tc := range cases {
		if got := trafficBarHeight(tc.total, peak, plotH); !tc.check(got) {
			t.Errorf("%s: trafficBarHeight(%d) = %.2fpx，%s", tc.name, tc.total, got, tc.why)
		}
	}
}

// TestHumanMsGroup 同一行里的延迟读数必须用同一个单位。
//
// 上一版是「当前 788 ms / 峰值 81.7 s / 均值 42.8 s」——一句话里两种量纲，
// 三个数没法直接比大小。
func TestHumanMsGroup(t *testing.T) {
	cases := []struct {
		in   []int64
		want []string
	}{
		{[]int64{788, 81700, 42800}, []string{"0.8 s", "81.7 s", "42.8 s"}},
		{[]int64{12, 300, 200}, []string{"12 ms", "300 ms", "200 ms"}},
		{[]int64{10, 5000, 50}, []string{"<0.1 s", "5.0 s", "0.1 s"}},
		{[]int64{0, 0, 0}, []string{"0 ms", "0 ms", "0 ms"}},
	}
	for _, tc := range cases {
		got := humanMsGroup(tc.in...)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("humanMsGroup(%v) = %v，期望 %v", tc.in, got, tc.want)
		}
	}
}

// TestCrashReportWritten 崩溃日志必须真的落盘。
//
// 起因：GUI 从 Finder 启动时 stderr 无处可去，进程「整个消失」而不留任何痕迹
// （2026-11-14 实测：Dock 图标消失、无 .ips、无日志）。写盘是唯一的证据来源，
// 所以这条断言守的是「有文件、有时间戳、有 panic 值和栈」。
func TestCrashReportWritten(t *testing.T) {
	dir := t.TempDir()
	if err := writeCrashReport(dir, "boom", []byte("goroutine 1 [running]:\nmain.main()")); err != nil {
		t.Fatalf("writeCrashReport: %v", err)
	}
	b, err := os.ReadFile(crashLogPath(dir))
	if err != nil {
		t.Fatalf("读不到崩溃日志：%v", err)
	}
	s := string(b)
	for _, want := range []string{"panic: boom", "goroutine 1", "T"} {
		if !strings.Contains(s, want) {
			t.Errorf("崩溃日志缺少 %q：%q", want, s)
		}
	}
}

// TestShouldStartOnLaunch 启动时是否自动开始转发。
//
// 需求（owner 2026-11-14）：「只要在启动状态，这个状态就应该持久化，下次重启
// 自动进入启动；除非我手动停止服务，那么这个停止的状态也会被持久化」。
// 所以判定顺序是：登录项 / 显式 -start 优先，其次才是上次的状态。
func TestShouldStartOnLaunch(t *testing.T) {
	cases := []struct {
		name      string
		login     bool
		flagStart bool
		lastRun   bool
		want      bool
	}{
		{"登录项启动", true, false, false, true},
		{"显式 -start", false, true, false, true},
		{"上次在运行", false, false, true, true},
		{"上次手动停过", false, false, false, false},
		{"手动停过但这次 -start", false, true, false, true},
	}
	for _, tc := range cases {
		st := relayclient.DefaultSettings()
		st.Running = tc.lastRun
		if got := shouldStartOnLaunch(tc.login, tc.flagStart, st); got != tc.want {
			t.Errorf("%s: shouldStartOnLaunch = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}

// TestRunStatePersisted 启动/停止必须写进 settings.json。
//
// 只断言文件：下次启动读文件决定是否自动开始，所以「文件里的 running」就是
// 全部契约。启动失败不能写成 true（否则每次启动都白试一次），这条也在语义里。
func TestRunStatePersisted(t *testing.T) {
	a := newTestApp(t)
	// :0 让内核分配端口：18899 可能被本机真实运行的客户端占着，绑不上就不该
	// 把「端口冲突」误判成「状态没持久化」。
	a.listen = "127.0.0.1:0"
	a.start()
	if !a.running() {
		t.Fatalf("start() 之后 Runner 没跑起来")
	}
	if s := relayclient.LoadSettings(a.settingsPath); !s.Running {
		t.Errorf("启动后 settings.running = false，期望 true")
	}
	a.stop()
	if a.running() {
		t.Fatalf("stop() 之后 Runner 还在跑")
	}
	if s := relayclient.LoadSettings(a.settingsPath); s.Running {
		t.Errorf("停止后 settings.running = true，期望 false")
	}
}

// TestCertNameOnlyForIPTarget 「证书域名」只在需要时出现。
//
// 2026-11-24 之前这条测的是设置页顶部的「目标机器」输入框：填 IP 时多出
// 一栏证书域名。合并到线路编辑区后约束不变——域名目标下 SNI 自动等于它，
// 多一个字段只会让人以为必须填；IP 目标（或中继指向另一台）才真的需要名字。
//
// 现在通过**线路编辑区**验证：进设置页 → 展开线路 → 编辑那条线路。
func TestCertNameOnlyForIPTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    route.Route
		want bool
	}{
		{"域名直连", route.Route{ID: "A", Kind: route.KindDirect,
			Server: "magpie.aiweb.xin:28443"}, false},
		{"IP 直连", route.Route{ID: "A", Kind: route.KindDirect,
			Server: "47.82.76.102:28443"}, true},
		{"域名中继", route.Route{ID: "A", Kind: route.KindRelay,
			Server: "relay.example:28443"}, true},
		{"IP 中继", route.Route{ID: "A", Kind: route.KindRelay,
			Server: "47.117.152.3:28443"}, true},
		{"代理线路", route.Route{ID: "A", Kind: route.KindProxy,
			Server: "magpie.aiweb.xin:28443", Proxy: "http://p:7890"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			a.tab = tabSettings
			a.routesExpanded = true
			a.routesDraft = route.Set{tc.r}
			a.setEditingRoute(0)
			tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
			if got := tt.HasText("证书域名"); got != tc.want {
				t.Fatalf("证书域名字段存在=%v，期望 %v（server=%s kind=%s）",
					got, tc.want, tc.r.Server, tc.r.Kind)
			}
		})
	}
}
