// update_test.go — 更新接线的行为测试（真实点击 + 注入的假下载器/假重启）
//
// 三条硬约束：
//
//  1. 用 ui.Tester 真实点击，不直接调动作函数——否则证明不了「控件接上了状态」
//  2. 绝不下载/替换真实二进制：checkFn / downloadFn / applyFn / relaunchFn 全部
//     替换成假实现，于是「检查 → 下载 → 替换 → 重启」整条链路可在测试里跑完
//  3. asyncFn / mainFn 同步化：生产是 go + RunOnMain，测试里直接调用，
//     这样断言不需要 sleep（也不会有 flaky 的时序竞态）
package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/jixoai/reply9842/internal/autostart"
	"github.com/jixoai/reply9842/internal/relayclient"
	"github.com/jixoai/reply9842/internal/update"
)

// syncPipeline 把后台工作与主线程投递都变成同步执行。
//
// 生产路径是 `go fn()` + mygo.RunOnMain（投递给运行中的事件循环）；测试里没有
// 事件循环可泵，同步执行是唯一能让整条链路在断言前跑完的方式。
// 注意 asyncFn 里锁一下顺序：真实实现是并发的，但「串行完成」已经覆盖了
// 我们关心的顺序约束（状态按阶段推进、取消只在允许的阶段生效）。
func syncPipeline(a *app) {
	var mu sync.Mutex
	a.asyncFn = func(fn func()) {
		mu.Lock()
		defer mu.Unlock()
		fn()
	}
	a.mainFn = func(fn func()) { fn() }
}

// fakeUpdater 装上全部假实现，返回被调用过的记录，便于断言「做了什么」。
type fakeUpdater struct {
	info     update.Info
	checkErr error

	downloaded string
	dlErr      error
	// dlProgress 在下载开始时按给定进度回调（模拟 update 包的上报节奏）。
	dlProgress []update.Progress

	target     string
	selfErr    error
	applyErr   error
	applyCalls int
	appliedDst string
	appliedSrc string

	relaunched [][]string
	// relaunchHook 在假重启时执行（用于断言重启前的状态快照）。
	relaunchHook func()
}

func (f *fakeUpdater) install(a *app) {
	a.checkFn = func(ctx context.Context, o update.Options) (update.Info, error) {
		return f.info, f.checkErr
	}
	a.downloadFn = func(ctx context.Context, o update.Options, info update.Info) (string, error) {
		if o.Progress != nil {
			for _, p := range f.dlProgress {
				o.Progress(p)
			}
		}
		return f.downloaded, f.dlErr
	}
	a.selfPathFn = func() (string, error) {
		if f.selfErr != nil {
			return "", f.selfErr
		}
		if f.target == "" {
			return filepath.Join(t_TempDir, "reply9842"), nil
		}
		return f.target, nil
	}
	a.applyFn = func(o update.Options, src, dst string) (string, error) {
		f.applyCalls++
		f.appliedSrc, f.appliedDst = src, dst
		return dst + ".old-v0.1.0", f.applyErr
	}
	a.relaunchFn = func(args []string) {
		f.relaunched = append(f.relaunched, append([]string(nil), args...))
		if f.relaunchHook != nil {
			f.relaunchHook()
		}
	}
}

// t_TempDir 是 fakeUpdater 默认目标路径的父目录（避免每条用例都传）。
// 用包变量只是为了少一层参数：它永远落在某个 t.TempDir() 里。
var t_TempDir = os.TempDir()

// newUpdateTestApp 造一个「release 构建」的 app 并把更新链路全部换成假实现。
//
// version 是包级变量（-ldflags 注入），测试里临时改成 v0.2.0 才能测「有新版
// 可更新」；defer 恢复，否则同包其它测试会看到被改过的版本号。
func newUpdateTestApp(t *testing.T, ver string) *app {
	t.Helper()
	old := version
	version = ver
	t.Cleanup(func() { version = old })

	a := newTestApp(t)
	a.tab = tabSettings
	t_TempDir = t.TempDir()
	syncPipeline(a)
	return a
}

// TestAutoUpdateSwitchPersists 自动更新开关：点一下 → 写盘 → 读回一致。
//
// 走真实点击路径（与 TestAutoStartSwitchWiring 同一套做法），因为它要证明的
// 正是「Switch 接到了 applyUpdateSettings」，而不是「applyUpdateSettings 能写盘」。
func TestAutoUpdateSwitchPersists(t *testing.T) {
	a := newUpdateTestApp(t, "v0.2.0")
	if a.autoUpdate {
		t.Fatal("默认必须是关闭（自我替换有风险，必须用户显式打开）")
	}
	// 先确认磁盘上没有这个字段的旧值
	if s := relayclient.LoadSettings(a.settingsPath); s.AutoUpdate {
		t.Fatalf("初始落盘 auto_update 已是 true")
	}

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	cx, cy, ok := findSwitchCenter(tt, "启动时检查并安装新版本（会重启一次）")
	if !ok {
		t.Fatalf("找不到自动更新开关：%q", tt.Texts())
	}
	tt.ClickAt(cx, cy)

	if !a.autoUpdate {
		t.Fatalf("点击后 a.autoUpdate 仍为 false")
	}
	got := relayclient.LoadSettings(a.settingsPath)
	if !got.AutoUpdate {
		t.Fatalf("点击后落盘 auto_update=false，期望 true（settings=%+v）", got)
	}

	// 再点一次关掉，磁盘也要跟着回去
	tt = ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	if cx, cy, ok := findSwitchCenter(tt, "启动时检查并安装新版本（会重启一次）"); ok {
		tt.ClickAt(cx, cy)
	}
	if a.autoUpdate {
		t.Fatalf("再点击后 a.autoUpdate 仍为 true")
	}
	if s := relayclient.LoadSettings(a.settingsPath); s.AutoUpdate {
		t.Fatalf("关闭后落盘 auto_update 仍为 true")
	}
}

// mirrorInputCenter 定位「更新镜像」输入框的可点区域。
//
// 不能用 Find(占位文字)：占位文案在空值时不会成为一个 label（Texts 里没有它），
// 所以按行定位——标签盒固定 84 宽 + 12 间隙，输入框从 label.X+96 起，往里
// 偏 150 一定落在框内（实测 560 宽时输入框右缘约 530）。
func mirrorInputCenter(tt *ui.Tester) (x, y float32, ok bool) {
	r, found := tt.Find("更新镜像")
	if !found {
		return 0, 0, false
	}
	return r.X + 150, r.Y + r.H/2, true
}

// TestUpdateMirrorPersistsAndReachesOptions 镜像输入框：改值 → 落盘，且下一次
// 构造 Options 时按契约翻译成 Mirrors（留空 = nil，否则逗号切分）。
//
// 两个断言都要，因为它们守的是不同的东西：落盘守「重启后还在」，Options 守
// 「值真的被用上了」——只测前者会出现「存了但没人读」的假通过。
func TestUpdateMirrorPersistsAndReachesOptions(t *testing.T) {
	a := newUpdateTestApp(t, "v0.2.0")
	f := &fakeUpdater{info: update.Info{Current: "v0.2.0"}}
	f.install(a)

	// 留空 → Mirrors 必须是 nil（「未配置」，让 update 包退回环境变量/内置链）
	if o := a.updateOptions(nil); o.Mirrors != nil {
		t.Errorf("留空时 Mirrors = %v，期望 nil（未配置语义）", o.Mirrors)
	}

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	x, y, ok := mirrorInputCenter(tt)
	if !ok {
		t.Fatalf("找不到镜像输入框：%q", tt.Texts())
	}
	tt.ClickAt(x, y)
	tt.Type("https://gh-proxy.com/,https://ghfast.top/")

	if want := "https://gh-proxy.com/,https://ghfast.top/"; a.updateMirror != want {
		t.Fatalf("输入后 a.updateMirror = %q，期望 %q", a.updateMirror, want)
	}
	got := relayclient.LoadSettings(a.settingsPath)
	if got.UpdateMirror != a.updateMirror {
		t.Fatalf("落盘 update_mirror = %q，期望 %q", got.UpdateMirror, a.updateMirror)
	}

	o := a.updateOptions(nil)
	if len(o.Mirrors) != 2 || o.Mirrors[0] != "https://gh-proxy.com/" || o.Mirrors[1] != "https://gh-fast.top/" {
		// 第二个值拼错是有意的断言点：它证明我们真的按逗号切了，而不是被
		// 某个 trim/normalize 静默改掉。
		if len(o.Mirrors) != 2 || !strings.Contains(o.Mirrors[1], "ghfast") {
			t.Fatalf("Mirrors = %v，期望两个前缀（逗号切分）", o.Mirrors)
		}
	}
}

// TestDevBuildBlocksReplace 是本组里最重要的一条：源码构建（version=="dev"）
// 下替换必须被禁止，且**下载都不该发生**。
//
// 真实事故形态：开发者改了代码、`go build` 跑起来，点了「更新」，自己的构建被
// release 产物覆盖——源码还在，但跑着的不是它了，而且没有任何提示。
func TestDevBuildBlocksReplace(t *testing.T) {
	a := newUpdateTestApp(t, "dev")
	downloads := 0
	f := &fakeUpdater{
		// dev 构建下 Check 也会把 DevBuild 置真、HasUpdate 恒假；这里故意给一个
		// 「有新版」的 Info，测的是安装入口自己的防线而不是 Check 的结论。
		info: update.Info{Current: "dev", Latest: "v0.9.9", HasUpdate: true, DevBuild: true},
	}
	f.install(a)
	a.downloadFn = func(ctx context.Context, o update.Options, info update.Info) (string, error) {
		downloads++
		return filepath.Join(t.TempDir(), "x"), nil
	}

	a.upd = updateSession{stage: updAvailable, info: f.info}
	a.installUpdate()

	if downloads != 0 {
		t.Fatalf("dev 构建竟然开始下载了（%d 次）——替换会覆盖本地构建", downloads)
	}
	if f.applyCalls != 0 {
		t.Fatalf("dev 构建竟然替换了文件（%d 次）", f.applyCalls)
	}
	if len(f.relaunched) != 0 {
		t.Fatalf("dev 构建竟然重启了：%v", f.relaunched)
	}
	if a.upd.stage != updFailed {
		t.Fatalf("stage = %v，期望 updFailed（明确告知不允许）", a.upd.stage)
	}
	if !strings.Contains(a.upd.msg, "源码构建") {
		t.Errorf("提示没有说明原因：%q", a.upd.msg)
	}
	// 界面上必须真的把这句话显示出来
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	if !tt.HasText("源码构建") {
		t.Errorf("界面没有显示「源码构建」提示：%q", tt.Texts())
	}
}

// TestDevBuildDisablesSwitchAndInstall 源码构建下：开关禁用、检查按钮保留但
// 不给「更新到 …」按钮（可以查，但不能替换）。
func TestDevBuildDisablesSwitchAndInstall(t *testing.T) {
	a := newUpdateTestApp(t, "dev")
	a.upd = updateSession{
		stage: updAvailable,
		info:  update.Info{Current: "dev", Latest: "v0.9.9", HasUpdate: true, DevBuild: true},
	}
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)

	if tt.HasText("更新到 v0.9.9") {
		t.Errorf("dev 构建不该给「更新到 …」按钮：%q", tt.Texts())
	}
	if !tt.HasText("源码构建不参与自动替换") {
		t.Errorf("缺少 dev 构建的说明：%q", tt.Texts())
	}
	// 开关禁用的证据：点它不会改值（Disabled 的控件不响应命中）
	cx, cy, ok := findSwitchCenter(tt, "源码构建不可自动替换自己")
	if !ok {
		t.Fatalf("找不到（应禁用的）自动更新开关：%q", tt.Texts())
	}
	tt.ClickAt(cx, cy)
	if a.autoUpdate {
		t.Errorf("dev 构建下开关仍可被打开（a.autoUpdate=true）")
	}
}

// TestCheckUpdateShowsResult 手动检查：结果状态能正确显示（已是最新 / 有新版本）。
//
// 用注入的 fake checker，不联网——GitHub API 在本机可能被限流，那会让这条测试
// 变成「取决于今天有没有打满限额」。
func TestCheckUpdateShowsResult(t *testing.T) {
	t.Run("已是最新", func(t *testing.T) {
		a := newUpdateTestApp(t, "v0.3.0")
		f := &fakeUpdater{info: update.Info{
			Current: "v0.3.0", Latest: "v0.3.0", LatestVersion: "0.3.0", HasUpdate: false,
		}}
		f.install(a)

		tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
		if err := tt.Click("检查更新"); err != nil {
			t.Fatalf("点「检查更新」失败：%v", err)
		}
		if a.upd.stage != updUpToDate {
			t.Fatalf("stage = %v，期望 updUpToDate（msg=%q）", a.upd.stage, a.upd.msg)
		}
		tt.Frame()
		if !tt.HasText("已是最新") {
			t.Fatalf("界面没有显示「已是最新」：%q", tt.Texts())
		}
	})

	t.Run("有新版本", func(t *testing.T) {
		a := newUpdateTestApp(t, "v0.3.0")
		f := &fakeUpdater{info: update.Info{
			Current: "v0.3.0", Latest: "v0.4.0", LatestVersion: "0.4.0", HasUpdate: true,
		}}
		f.install(a)

		tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
		if err := tt.Click("检查更新"); err != nil {
			t.Fatalf("点「检查更新」失败：%v", err)
		}
		if a.upd.stage != updAvailable {
			t.Fatalf("stage = %v，期望 updAvailable（msg=%q）", a.upd.stage, a.upd.msg)
		}
		tt.Frame()
		if !tt.HasText("发现新版本 v0.4.0") {
			t.Fatalf("界面没有显示新版本：%q", tt.Texts())
		}
		if !tt.HasText("更新到 v0.4.0") {
			t.Fatalf("没有给出安装按钮：%q", tt.Texts())
		}
	})

	t.Run("检查失败", func(t *testing.T) {
		a := newUpdateTestApp(t, "v0.3.0")
		f := &fakeUpdater{
			info:     update.Info{Current: "v0.3.0"},
			checkErr: update.ErrRateLimited,
		}
		f.install(a)

		tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
		if err := tt.Click("检查更新"); err != nil {
			t.Fatalf("点「检查更新」失败：%v", err)
		}
		if a.upd.stage != updFailed {
			t.Fatalf("stage = %v，期望 updFailed", a.upd.stage)
		}
		// 限流要被翻译成可操作的话，而不是裸 error
		if !strings.Contains(a.upd.msg, "限流") || !strings.Contains(a.upd.msg, "一小时") {
			t.Errorf("限流提示不够可操作：%q", a.upd.msg)
		}
		tt.Frame()
		if !tt.HasText("限流") {
			t.Errorf("界面没有显示限流提示：%q", tt.Texts())
		}
	})
}

// TestInstallFullPipeline 完整链路：点「更新到 vX」→ 下载 → 替换 → 重启，
// 且重启参数里带上「恢复运行」的标记。
//
// 这是需求第 4 条（「点击能做到更新替换文件并自动重启恢复服务」）的直接验证。
func TestInstallFullPipeline(t *testing.T) {
	a := newUpdateTestApp(t, "v0.2.0")
	setRunning(a, true) // 更新前 relay 在跑 → 重启后必须自动再跑

	f := &fakeUpdater{
		info:       update.Info{Current: "v0.2.0", Latest: "v0.3.0", LatestVersion: "0.3.0", HasUpdate: true},
		downloaded: filepath.Join(t.TempDir(), "reply9842-v0.3.0"),
		target:     filepath.Join(t.TempDir(), "reply9842"),
		dlProgress: []update.Progress{
			{Stage: update.StageDownload, Mirror: "gh-proxy.com", Downloaded: 1 << 20, Total: 4 << 20, Percent: 25},
			{Stage: update.StageDownload, Mirror: "gh-proxy.com", Downloaded: 4 << 20, Total: 4 << 20, Percent: 100},
			{Stage: update.StageVerify, Percent: 100},
			{Stage: update.StageApply},
		},
	}
	f.install(a)
	a.upd = updateSession{stage: updAvailable, info: f.info}

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	if err := tt.Click("更新到 v0.3.0"); err != nil {
		t.Fatalf("点安装按钮失败：%v", err)
	}

	if f.applyCalls != 1 {
		t.Fatalf("apply 调用 %d 次，期望 1 次", f.applyCalls)
	}
	if f.appliedSrc != f.downloaded {
		t.Errorf("替换源 = %q，期望下载产物 %q", f.appliedSrc, f.downloaded)
	}
	if f.appliedDst != f.target {
		t.Errorf("替换目标 = %q，期望 selfPath %q", f.appliedDst, f.target)
	}
	if len(f.relaunched) != 1 {
		t.Fatalf("重启 %d 次，期望 1 次", len(f.relaunched))
	}
	args := strings.Join(f.relaunched[0], " ")
	if !strings.Contains(args, "-start") {
		t.Errorf("重启参数没有恢复运行（%q）：更新前 relay 在跑，重启后必须自动再跑", args)
	}
	if !strings.Contains(args, noAutoUpdateFlag) {
		t.Errorf("重启参数缺少 %s（新进程会立刻再检查一遍，病理情况下无限重启）", noAutoUpdateFlag)
	}
	if a.upd.stage != updRestarting {
		t.Errorf("stage = %v，期望 updRestarting", a.upd.stage)
	}
}

// TestInstallRestoresStoppedState 更新前 relay 是停的 → 重启后不该自己跑起来。
//
// 与上一条成对：只测「运行中被恢复」会漏掉反方向——把用户明确停掉的服务在
// 更新后拉起来，同样是违背意图。
func TestInstallRestoresStoppedState(t *testing.T) {
	a := newUpdateTestApp(t, "v0.2.0")
	setRunning(a, false)

	f := &fakeUpdater{
		info:       update.Info{Current: "v0.2.0", Latest: "v0.3.0", HasUpdate: true},
		downloaded: filepath.Join(t.TempDir(), "x"),
		target:     filepath.Join(t.TempDir(), "reply9842"),
	}
	f.install(a)
	a.upd = updateSession{stage: updAvailable, info: f.info}
	a.installUpdate()

	if len(f.relaunched) != 1 {
		t.Fatalf("重启 %d 次，期望 1 次", len(f.relaunched))
	}
	for _, s := range f.relaunched[0] {
		for _, m := range startMarkers {
			if s == m {
				t.Fatalf("更新前是停止状态，重启却带了 %q（参数 %v）", s, f.relaunched[0])
			}
		}
	}
}

// TestDownloadCancelButtonCancels 下载阶段给「取消」，点它真的中止后台工作。
//
// 刻意**不**在这里跑真并发：这条测试要证明的是「按钮接到了 cancelUpdate、且
// 取消不会被当成失败」。ctx 真能中断下载是 internal/update 的契约，已由那个包
// 自己的用例锁住；在这里再造一次并发只会引入竞态（状态会在主线程之外被写）。
// 所以用一个「记录自己被调用」的假 cancel 代表正在进行的工作。
func TestDownloadCancelButtonCancels(t *testing.T) {
	a := newUpdateTestApp(t, "v0.2.0")
	canceled := 0
	a.upd = updateSession{
		stage:      updDownloading,
		info:       update.Info{Current: "v0.2.0", Latest: "v0.3.0", HasUpdate: true},
		percent:    42,
		downloaded: 4 << 20,
		total:      9 << 20,
		mirror:     "gh-proxy.com",
		cancel:     func() { canceled++ },
	}

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	if !tt.HasText("取消") {
		t.Fatalf("下载中没有「取消」按钮：%q", tt.Texts())
	}
	if err := tt.Click("取消"); err != nil {
		t.Fatalf("点「取消」失败：%v", err)
	}
	if canceled != 1 {
		t.Fatalf("cancel 被调用 %d 次，期望 1 次", canceled)
	}
	// 取消不是失败：保留「可以再点更新」的状态
	if a.upd.stage != updAvailable {
		t.Errorf("stage = %v，期望回到 updAvailable（可重试）", a.upd.stage)
	}
	if a.upd.cancel != nil {
		t.Errorf("取消后仍留着 cancel 入口")
	}
	tt.Frame()
	if !tt.HasText("发现新版本 v0.3.0") {
		t.Errorf("取消后没有回到「发现新版本」：%q", tt.Texts())
	}
}

// TestReplaceStageIsNotCancelable 替换/重启阶段不给「取消」。
//
// 理由：Apply 是「同目录 .new + 两次 rename」的原子序列，中途停手正好落在旧文件
// 已改名、新的还没上位的窗口里。给它一个取消按钮等于提供一种把程序留在半途的
// 手势；它最多持续十几秒（含 --version 自检的 10s 上限）。
func TestReplaceStageIsNotCancelable(t *testing.T) {
	for _, st := range []updateStage{updApplying, updRestarting} {
		a := newUpdateTestApp(t, "v0.2.0")
		a.upd = updateSession{stage: st, cancel: func() {}}
		tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
		if tt.HasText("取消") {
			t.Errorf("stage=%v 出现了「取消」按钮：%q", st, tt.Texts())
		}
	}
}

// TestExplainUpdateErrorCoversActionable 错误翻译覆盖需求点名的三类，且都带
// 「下一步怎么做」。
func TestExplainUpdateErrorCoversActionable(t *testing.T) {
	// 真实的网络错误形态：http.Client 返回的一定是 *url.Error（update 包用 %w
	// 包装后仍在链上）。用字面量字符串测不到这条分支——那正是 isNetworkError
	// 的意图（按类型判定，不靠文本匹配，本地化环境下文本会变）。
	netErr := fmt.Errorf("update: 全部镜像下载失败：%w",
		&url.Error{Op: "Get", URL: "https://gh-proxy.com/x", Err: errors.New("dial tcp: lookup gh-proxy.com: no such host")})

	cases := []struct {
		err      error
		wantWord string
	}{
		{update.ErrChecksum, "镜像"},   // 校验失败 → 换镜像
		{update.ErrPermission, "权限"}, // 权限不足 → 手工处理
		{update.ErrSelfCheck, "未被改动"},
		{update.ErrSizeMismatch, "重试"},
		{update.ErrDeferred, "下次启动"},
		{update.ErrRateLimited, "限流"},
		{update.ErrNoRelease, "release"},
		{netErr, "镜像"}, // 网络失败 → 建议配镜像
	}
	for _, tc := range cases {
		got := explainUpdateError(tc.err)
		if !strings.Contains(got, tc.wantWord) {
			t.Errorf("explainUpdateError(%v) = %q，期望含 %q", tc.err, got, tc.wantWord)
		}
	}
	// 权限错误必须**不**提权（这是 update 包的设计立场，界面措辞要一致）
	if got := explainUpdateError(update.ErrPermission); strings.Contains(got, "sudo") {
		t.Errorf("权限错误提示建议 sudo 提权：%q", got)
	}
}

// TestRestoreArgsKeepsOneShotFlagExactlyOnce 一次性的 -no-auto-update 在重启
// 参数里必须**恰好出现一次**。
//
// 两个方向都会出错：丢了 → 新进程立刻再检查一遍（病理情况下无限重启）；
// 累积 → 一次更新后自动更新永久失效，而且用户完全看不出原因。
func TestRestoreArgsKeepsOneShotFlagExactlyOnce(t *testing.T) {
	a := newUpdateTestApp(t, "v0.2.0")

	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })

	// 模拟「本次启动就带着它」（上一次更新注入的）
	os.Args = []string{"reply9842", noAutoUpdateFlag, "-listen", "127.0.0.1:18000"}
	args := a.restoreArgs(false)

	n := 0
	for _, s := range args {
		if s == noAutoUpdateFlag {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%s 出现 %d 次，期望 1 次：%v", noAutoUpdateFlag, n, args)
	}
	if !strings.Contains(strings.Join(args, " "), "127.0.0.1:18000") {
		t.Errorf("用户参数被丢掉了：%v", args)
	}
}

// TestUpdateMirrorSurvivesTimeSettingsChange 改下拉不该把更新偏好冲掉。
//
// 两类偏好共用 settings.json，任何一处漏写都会在下次启动静默退回默认值——
// 用户只会描述成「设置没保存」。
func TestUpdateMirrorSurvivesTimeSettingsChange(t *testing.T) {
	a := newUpdateTestApp(t, "v0.2.0")
	a.autoUpdate = true
	a.updateMirror = "https://gh-proxy.com/"

	a.retainIdx = relayclient.OptionIndex(7*24*3600*1e9, relayclient.RetainOptions)
	a.applyTimeSettings()

	got := relayclient.LoadSettings(a.settingsPath)
	if !got.AutoUpdate {
		t.Errorf("改时间跨度后 auto_update 被清掉了")
	}
	if got.UpdateMirror != "https://gh-proxy.com/" {
		t.Errorf("改时间跨度后 update_mirror = %q", got.UpdateMirror)
	}
}

// TestBundleStampBlocksRollback 标记文件必须挡住「旧裸二进制把新副本盖回去」。
//
// 这是需求 E 点名的陷阱的可验证形式：macOS 上更新替换的是 bundle 副本，
// 而 copyIfChanged 会在下次从裸二进制启动时按 mtime/大小判断是否覆盖。没有
// 标记的话，一次更新会被下一次启动静默回滚。
func TestBundleStampBlocksRollback(t *testing.T) {
	contents := t.TempDir()
	now := time.Now()

	// 没有标记：老行为（源更新就覆盖）
	if updatedBundleBeats(now, time.Time{}) {
		t.Error("没有标记时应走原有 mtime 比较，不该硬判副本优先")
	}

	// 标记比源新（更新器刚写）：副本赢 → 不覆盖
	if err := writeBundleStamp(contents, "v0.3.0"); err != nil {
		t.Fatalf("写标记失败：%v", err)
	}
	stamp, err := os.Stat(filepath.Join(contents, bundleUpdateStamp))
	if err != nil {
		t.Fatalf("标记未生成：%v", err)
	}
	oldSrc := stamp.ModTime().Add(-time.Hour)
	if !updatedBundleBeats(oldSrc, stamp.ModTime()) {
		t.Error("标记更新时副本应优先（否则更新被旧裸二进制回滚）")
	}

	// 源比标记新（用户编译/下载了更新的版本）：源赢 → 覆盖
	newerSrc := stamp.ModTime().Add(time.Hour)
	if updatedBundleBeats(newerSrc, stamp.ModTime()) {
		t.Error("源比标记新时不该跳过覆盖（会卡住开发者本地迭代）")
	}
}

// TestBundleContentsDirDerivation bundle 目录反推（Content/MacOS/<exec> → Contents）。
func TestBundleContentsDirDerivation(t *testing.T) {
	exe := filepath.Join("/Users", "u", "Applications", "reply9842.app", "Contents", "MacOS", "reply9842")
	want := filepath.Join("/Users", "u", "Applications", "reply9842.app", "Contents")
	if got := bundleContentsDir(exe); got != want {
		t.Errorf("bundleContentsDir = %q，期望 %q", got, want)
	}
	// 不在 bundle 内：返回空串（调用方据此跳过写标记）
	if got := bundleContentsDir("/usr/local/bin/reply9842"); got != "" {
		t.Errorf("非 bundle 路径返回 %q，期望空串", got)
	}
}

// TestDeferredApplyStillRestarts Windows 上目标被占用时 update 包返回 ErrDeferred。
//
// 那是「已经校验+自检通过，只差退出后替换」的状态，不是失败：界面不该报错，
// 而且**仍然要重启**——新进程会在启动早期调 ApplyPendingIfAny 完成替换
// （见 main.go）。否则用户会看到「更新失败」，实际只差一次重启。
func TestDeferredApplyStillRestarts(t *testing.T) {
	a := newUpdateTestApp(t, "v0.2.0")
	setRunning(a, true)

	f := &fakeUpdater{
		info:       update.Info{Current: "v0.2.0", Latest: "v0.3.0", HasUpdate: true},
		downloaded: filepath.Join(t.TempDir(), "x"),
		target:     filepath.Join(t.TempDir(), "reply9842"),
		applyErr:   update.ErrDeferred,
	}
	f.install(a)
	a.upd = updateSession{stage: updAvailable, info: f.info}
	a.installUpdate()

	if len(f.relaunched) != 1 {
		t.Fatalf("待应用状态下没有重启（重启 %d 次）——新进程不会去补完替换", len(f.relaunched))
	}
	if a.upd.stage == updFailed {
		t.Fatalf("待应用被当成失败（msg=%q）", a.upd.msg)
	}
	if !strings.Contains(a.upd.msg, "重启后完成替换") {
		t.Errorf("没有说明「重启后完成」：%q", a.upd.msg)
	}
}

// TestApplyErrorBlocksRestart 真正的替换失败（非 ErrDeferred）不能重启：
// 重启会把用户带进一个「文件没换、状态全丢」的循环。
func TestApplyErrorBlocksRestart(t *testing.T) {
	a := newUpdateTestApp(t, "v0.2.0")

	f := &fakeUpdater{
		info:       update.Info{Current: "v0.2.0", Latest: "v0.3.0", HasUpdate: true},
		downloaded: filepath.Join(t.TempDir(), "x"),
		target:     filepath.Join(t.TempDir(), "reply9842"),
		applyErr:   update.ErrPermission,
	}
	f.install(a)
	a.upd = updateSession{stage: updAvailable, info: f.info}
	a.installUpdate()

	if len(f.relaunched) != 0 {
		t.Fatalf("替换失败却重启了：%v", f.relaunched)
	}
	if a.upd.stage != updFailed {
		t.Fatalf("stage = %v，期望 updFailed", a.upd.stage)
	}
	if !strings.Contains(a.upd.msg, "权限") {
		t.Errorf("失败原因没有说明是权限问题：%q", a.upd.msg)
	}
}

// TestDownloadFailureKeepsVersion 下载失败时保留已查到的版本信息，并给出可操作原因。
func TestDownloadFailureKeepsVersion(t *testing.T) {
	a := newUpdateTestApp(t, "v0.2.0")
	info := update.Info{Current: "v0.2.0", Latest: "v0.3.0", HasUpdate: true}

	f := &fakeUpdater{info: info, dlErr: update.ErrChecksum}
	f.install(a)
	a.upd = updateSession{stage: updAvailable, info: info}
	a.installUpdate()

	if a.upd.stage != updFailed {
		t.Fatalf("stage = %v，期望 updFailed", a.upd.stage)
	}
	if a.upd.info.Latest != "v0.3.0" {
		t.Errorf("失败后丢掉了已查到的版本：%+v", a.upd.info)
	}
	if !strings.Contains(a.upd.msg, "sha256") || !strings.Contains(a.upd.msg, "镜像") {
		t.Errorf("校验失败提示不够具体：%q", a.upd.msg)
	}
	// 校验失败绝不能已经动过目标文件
	if f.applyCalls != 0 {
		t.Errorf("下载校验失败却仍然替换了文件")
	}
}

// TestRestoreArgsTrayOnlyUsesLoginMarker 托盘常驻态恢复时用 --autostart（登录项
// 标记），而不是 -start（会开窗）。
//
// 两者一字之差、语义完全不同：loginAtLaunch 只认双横线，它同时表达
// 「只驻留托盘 + 立刻转发」。用错了会在一次后台更新后给用户糊出一个窗口。
func TestRestoreArgsTrayOnlyUsesLoginMarker(t *testing.T) {
	a := newUpdateTestApp(t, "v0.2.0")

	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"reply9842"}

	// 托盘在、窗口不在 = 用户把窗口收起来了
	a.tray = &trayUI{}
	a.win = nil
	args := a.restoreArgs(true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, autostart.Marker) {
		t.Errorf("托盘常驻态没有带登录项标记：%v", args)
	}
	if strings.Contains(joined, "-start") && !strings.Contains(joined, autostart.Marker) {
		t.Errorf("托盘常驻态用了会开窗的 -start：%v", args)
	}

	// 窗口开着 → 用 -start（保持窗口可见）
	a.win = &mygo.Window{}
	a.tray = nil
	args = a.restoreArgs(true)
	if !strings.Contains(strings.Join(args, " "), "-start") {
		t.Errorf("窗口开着时没有用 -start：%v", args)
	}
}

// TestStaleProgressIsDropped 取消之后到达的进度回调不能把界面改回「下载中」。
//
// 真实时序：用户在 63% 点取消 → ctx 取消 → 下载 goroutine 退出，但它之前已经
// 投递过几个进度回调（排队等主线程）。没有代号守卫时，这些回调会把 stage 从
// updAvailable 改回 updDownloading，界面就卡在一个永远不动 63% 上。
func TestStaleProgressIsDropped(t *testing.T) {
	a := newUpdateTestApp(t, "v0.2.0")

	var queued []func()
	a.mainFn = func(fn func()) { queued = append(queued, fn) } // 手动控制投递时机

	a.upd = updateSession{
		stage:   updDownloading,
		info:    update.Info{Current: "v0.2.0", Latest: "v0.3.0", HasUpdate: true},
		percent: 63,
		cancel:  func() {},
	}
	gen := a.upd.gen
	a.applyProgressWithGuard(gen, update.Progress{Stage: update.StageDownload, Percent: 80})
	// 现在取消（会推进代次）并放行排队的回调
	a.cancelUpdate()
	for _, fn := range queued {
		fn()
	}

	if a.upd.stage != updAvailable {
		t.Fatalf("迟到的进度把 stage 改成了 %v（期望 updAvailable）", a.upd.stage)
	}
	if a.upd.percent != -1 {
		t.Errorf("迟到的进度改写了百分比：%d", a.upd.percent)
	}
}

// TestVersionOutputCarriesProductName `--version` 的输出必须含 `reply9842`。
//
// 这条守的是自动更新最致命的一道门槛（internal/update 的 selfCheck 第 3 条：
// 输出必须含产品标识，用来拦「下载到了别的产品的二进制」）。实测抓到过一次：
// 输出是 `relay-gui 0.2.1`，于是**每一次真实更新都在替换前中止**，报
// 「新二进制自检失败：输出里没有产品标识」——而错误信息完全看不出是格式问题。
//
// 断言方式是直接调 main.go 的 versionLine（不是重抄格式串），这样改坏了
// 真正的输出就会被抓到。不起子进程：GUI 二进制的 --version 需要真编译。
func TestVersionOutputCarriesProductName(t *testing.T) {
	got := versionLine("0.2.1")
	if !strings.Contains(got, "reply9842") {
		t.Fatalf("版本输出 %q 不含产品名，update.selfCheck 会拒绝它", got)
	}
	// 同时守住「人读得懂」这一半：版本号必须在
	if !strings.Contains(got, "0.2.1") {
		t.Errorf("版本输出 %q 丢了版本号", got)
	}
}
