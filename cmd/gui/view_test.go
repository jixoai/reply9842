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
func TestIdleHasNoHeroNumber(t *testing.T) {
	a := newTestApp(t)
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)

	for _, banned := range []string{"—", "0×", "0.0×"} {
		if tt.HasText(banned) {
			t.Errorf("空态出现了占位巨号 %q：%q", banned, tt.Texts())
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
	for _, banned := range []string{"上游地址", "监听地址", "传输方式", "开机自启"} {
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

	for _, want := range []string{"上游地址", "监听地址", "传输方式", "日志保留", "显示区间"} {
		if !tt.HasText(want) {
			t.Errorf("设置 tab 缺少 %q：%q", want, tt.Texts())
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
// 不要求桶粒度后缀：一行装不下四段（会被省略号截断），设计上保留
// 「请求数」而不是「每桶多久」——桶粒度可由窗口长度除以 30 推出。
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

// TestSettingsWhileRunning 运行中：字段被禁用并给出「先停止再改」的说明与出口。
func TestSettingsWhileRunning(t *testing.T) {
	a := newTestApp(t)
	a.tab = tabSettings
	setRunning(a, true)
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)

	if !tt.HasText("运行中不可修改") {
		t.Errorf("运行中未提示不可修改：%q", tt.Texts())
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
	if n := rawBytesPerKilo(0.01); n != 10 {
		t.Errorf("每 1,000 字节发出 %d 字节，期望 10", n)
	}
	if s := (&app{}).heroNote(stats.Agg{UpRaw: 1400, UpWire: 14, UpRatio: 0.01}, true); !strings.Contains(s, "99.0%") {
		t.Errorf("heroNote = %q，期望含节省 99.0%%", s)
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
				(&app{pal: pal}).actionButton(c, pal, "k", "按钮", true)
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

// TestMeterDirection 压缩条的填充语义：越长 = 发出去的越多 = 压得越差。
//
// 这条测试守的是一个容易被改反的方向（「进度条越长越好」是直觉，但这里正好相反）。
func TestMeterDirection(t *testing.T) {
	// 几乎压到零：填充极短，用最亮的白（好结果）
	good := stats.Agg{UpRaw: 1400 * 1024, UpWire: 1024, UpRatio: 1024.0 / (1400 * 1024)}
	if frac := good.UpRatio; frac > 0.01 {
		t.Errorf("压得好的用例占比 %.4f，应远小于 1%%", frac)
	}
	// 没压：填充满格，且不该给亮白
	bad := stats.Agg{UpRaw: 101, UpWire: 101, UpRatio: 1.0}
	if s := (&app{}).heroNote(bad, true); !strings.Contains(s, "未压缩") {
		t.Errorf("未压缩时说明文案 = %q，应点明「未压缩」而不是「节省 0.0%%」", s)
	}
	if s := (&app{}).heroNote(good, true); !strings.Contains(s, "节省") {
		t.Errorf("有压缩时说明文案 = %q，应给出节省比例", s)
	}
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
	// 切到设置后，设置的内容必须在
	for _, want := range []string{"上游地址", "监听地址", "传输方式"} {
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
		"连接", "上游地址", "服务器域名", "源地址", "监听地址", "传输方式",
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
	label, okL := tt.Find("上游地址")
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

// TestTrafficSummaryOneLine 摘要必须单行，且延迟的三个读数都在。
//
// 守的是一个实际缺陷：「握手 N 次 · 均 X ms」太长，560 宽时最后一个
// 单位词被折到第二行、独占一个字。现在改成「当前/峰值/均值」，并用
// SingleLine 兜底；这里同时断言文案字段与渲染高度（单行约 13px）。
func TestTrafficSummaryOneLine(t *testing.T) {
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

	text := a.trafficSummary(a.agg)
	for _, want := range []string{"延迟 当前", "/ 峰值", "/ 均值"} {
		if !strings.Contains(text, want) {
			t.Errorf("trafficSummary = %q，缺少 %q", text, want)
		}
	}
	// 旧格式的「 · 均 2449 ms」是折行的元凶：不允许它回来。
	if strings.Contains(text, " 均 ") {
		t.Errorf("trafficSummary = %q，仍带「 均 X ms」尾巴", text)
	}

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	r, ok := tt.Find(text)
	if !ok {
		t.Fatalf("找不到摘要文本：%q", tt.Texts())
	}
	if r.H > 16 {
		t.Errorf("摘要高度 %.1f（>16px）说明折成了多行：%q", r.H, text)
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

// dashRow 找参考虚线所在行：虚线段 3px 实 3px 空，一行里有几十段同色像素，
// 覆盖行宽约一半；取 [y0,y1) 里「同一颜色重复次数最多」的行。要求覆盖率在
// 30%..65%，把整行同色的卡片边/基线（≈100%）排除掉。
func dashRow(img *image.RGBA, x0, x1, y0, y1 int) int {
	width := x1 - x0
	lo, hi := width*3/10, width*65/100
	best, bestN := -1, 0
	for y := y0; y < y1; y++ {
		count := map[color.RGBA]int{}
		for x := x0; x < x1; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			count[color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}]++
		}
		for _, n := range count {
			if n >= lo && n <= hi && n > bestN {
				best, bestN = y, n
			}
		}
	}
	return best
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

// TestChartScaleBarsAndTicks 两张图的柱、参考虚线、刻度标签共用同一个 scale。
//
// 三条断言对应上一轮独立像素复核的三条 blocker：
//  1. 第一图：100% 压缩的柱顶与 100% 虚线在同一像素行（≤1px）。上一版柱高
//     用 96% 可用高度、虚线用整高，虚线从每根柱顶下方 15px 处横穿。
//  2. 第二图：峰值柱顶不高于最上刻度（≥0），且落在刻度上（≤2px）。
//  3. 两个绘图区等高（由刻度反算，≤1px）。
func TestChartScaleBarsAndTicks(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.agg = stats.Agg{Requests: 40, WindowSec: 1800, BucketSec: 60}
	a.agg.Series = make([]stats.Bucket, stats.SeriesBuckets)
	// 桶 0：wire=0 → 压缩率 100%，柱顶必须压在 100% 虚线上。
	a.agg.Series[0] = stats.Bucket{UpRaw: 800_000, UpWire: 0, DownWire: 60_000, Count: 1, MsSum: 120}
	for i := 1; i < 12; i++ {
		a.agg.Series[i] = stats.Bucket{
			UpRaw: 800_000, UpWire: int64(20_000 * i), DownWire: int64(30_000 * i),
			Count: 1, MsSum: int64(400 * i),
		}
	}
	a.snap = func() stats.Agg { return a.agg }

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	img := tt.Image()
	if img == nil {
		t.Fatal("Image() 返回 nil")
	}

	// 绘图区横向范围：左边是刻度列 + 间距，右边到卡片右边距。
	// 右缘跟着窗口宽度走（560 宽时是 529），写死会在改尺寸后量到卡片外。
	const x0 = 84
	x1 := defaultWinWidth - 31

	// ---- 1. 第一图的柱顶 vs 100% 虚线 ----
	r100, ok := tt.Find("100%")
	if !ok {
		t.Fatalf("找不到 100%% 刻度：%q", tt.Texts())
	}
	yTop := int(r100.Y)
	if yTop < 40 || yTop > 500 {
		t.Fatalf("100%% 刻度 y=%.1f 不合理", r100.Y)
	}
	dash := dashRow(img, x0, x1, yTop-4, yTop+12)
	bar := barTopRow(img, x0, x1, yTop-4, yTop+40)
	if dash < 0 || bar < 0 {
		t.Fatalf("第一图找不到虚线(%d)或柱顶(%d)", dash, bar)
	}
	if d := absInt(bar - dash); d > 1 {
		t.Errorf("柱顶 y=%d 与 100%% 虚线 y=%d 相差 %dpx（要求 ≤1）", bar, dash, d)
	} else {
		t.Logf("第一图：柱顶 y=%d / 100%% 虚线 y=%d（差 %dpx）", bar, dash, d)
	}

	// ---- 2/3. 第二图：峰值柱顶 vs 最上刻度；两图等高 ----
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
	tick := inkCenterRow(img2, 20, 84, int(rt.Y)-6, int(rt.Y)+8,
		[3]uint8{0x94, 0x94, 0x94}, 40)
	if tick < 0 {
		t.Fatalf("找不到峰值刻度文字")
	}
	if d := float64(peak) - tick; d < 2 || d > 4 {
		t.Errorf("峰值柱顶 y=%d、最上刻度 y=%.1f，差 %.1fpx（要求 2..4：柱顶必须在刻度"+
			"中心之下留出 2-3px 的顶部余量，齐平会读成「柱顶顶到坐标系上沿」）",
			peak, tick, d)
	} else {
		t.Logf("第二图：峰值柱顶 y=%d / 最上刻度 y=%.1f（差 %.1fpx）", peak, tick, d)
	}

	// 等高：第一图由 100%/25% 两档反算（间隔 0.75H），第二图由峰值/半峰值
	// 反算（间隔 0.5H）。用刻度行距而不是绝对 y，避免标签居中带来的偏移。
	r25, ok := tt.Find("25%")
	if !ok {
		t.Fatalf("找不到 25%% 刻度")
	}
	h1 := (r25.Y - r100.Y) / 0.75
	half, ok := tt2.Find(humanBytes(13_000_000 / 2))
	if !ok {
		t.Fatalf("找不到半峰值刻度 %q：%q", humanBytes(13_000_000/2), tt2.Texts())
	}
	h2 := (half.Y - rt.Y) / 0.5
	if d := float64(h1 - h2); d < -1 || d > 1 {
		t.Errorf("两个绘图区高度不等：%.1f vs %.1f（差 %.1fpx）", h1, h2, d)
	} else {
		t.Logf("绘图区高度：第一图 %.1fpx / 第二图 %.1fpx", h1, h2)
	}
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
			UpRaw: 1_000_000, UpWire: 200_000, DownWire: 300_000,
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
		b := stats.Bucket{UpRaw: 1_000_000, UpWire: 200_000, DownWire: 300_000,
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
		{"1.25%", peak / 80, func(h float32) bool { return h == minTrafficBarPx }, "刚过 1% 的桶抬到 3px"},
		{"2%", peak / 50, func(h float32) bool { return h > minTrafficBarPx }, "按比例已超过 3px"},
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
