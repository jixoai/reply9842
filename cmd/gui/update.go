// update.go — 设置页里的「自动更新」接线层（UI 状态 + 流程编排 + 重启恢复）
//
// 正交意图（3 个，2026-11-13 需求：「自动更新在设置里是个开关，可以关闭」
// 「可以手动检查」「可以配置 Mirror」「点击能做到更新替换文件并自动重启恢复服务」）：
//
//  1. 把 internal/update 的四步（检查 / 下载 / 替换 / 重启）串成一条带 UI 反馈的流程
//  2. 自我替换是这个应用里唯一会动自己二进制的动作 → dev 构建一律禁止，开关默认关
//  3. 重启必须恢复更新前的运行状态，且不能撞上单实例锁
//
// 本文件**不修改** internal/update：那个包里已经有全部安全防线（sha256 强制校验、
// 自检、同目录 .new + rename、旧版本备份），这里只负责决定「什么时候调、怎么显示」。
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/jixoai/reply9842/internal/autostart"
	"github.com/jixoai/reply9842/internal/glass"
	"github.com/jixoai/reply9842/internal/update"
)

// updateKind 是本产品的产物形态。填错会让更新下载另一个可执行文件并覆盖自己
// （update.go 的 Options.Kind 注释里点名的不可恢复事故），所以写成常量。
const updateKind = update.KindGUI

// ---- 状态机 ----

// updateStage 更新流程的阶段。刻意只有一个维度（而不是「是否检查过 × 是否在跑」）：
// 设置页只有一行位置，阶段决定那一行的文案、按钮与颜色，多一个维度就要自己拼状态。
type updateStage int

const (
	updIdle updateStage = iota // 还没检查过
	updChecking
	updAvailable // 查到新版本，等用户点「更新」
	updUpToDate  // 已是最新
	updDownloading
	updVerifying
	updApplying
	updRestarting
	updCanceled // 用户主动中止（不是失败：不给红色，也不清空已查到的版本）
	updFailed
)

// busy 报告这一阶段是否已有一项进行中的工作（决定按钮是「取消」还是动作）。
func (s updateStage) busy() bool {
	switch s {
	case updChecking, updDownloading, updVerifying, updApplying, updRestarting:
		return true
	}
	return false
}

// cancelable 报告这一阶段能否被用户中断。
//
// 排除 apply/restart 是刻意的：Apply 已经是「同目录 .new + 两次 rename」的原子
// 序列，中途停下正好会落在最危险的那个窗口（旧文件已改名、新的还没上位）。
// 它最多持续十几秒（含 --version 自检的 10s 上限），不给取消比给一个会把程序
// 留在半途的取消按钮安全。
func (s updateStage) cancelable() bool {
	return s == updChecking || s == updDownloading || s == updVerifying
}

// updateSession 一次更新会话可观察的状态。
//
// 所有字段只在主线程读写（与 app 的其余字段同一约束）；后台 goroutine 只持有
// 自己的局部变量，结果经 app.runMain 投递回来。
type updateSession struct {
	stage updateStage
	// info 是最近一次成功检查的结果（失败时保留上一次，便于界面继续显示当前版本）。
	//
	// 不需要额外的「查过没有」布尔：stage 的零值 updIdle 就是那个语义，
	// 而 updAvailable/updUpToDate 只有在一次成功检查之后才会出现。
	info update.Info

	// 下载进度（Percent 为 -1 表示服务端没给总长）。
	percent    int
	downloaded int64
	total      int64
	mirror     string

	// msg 是失败原因（已翻译成人话 + 可操作建议）。
	msg string

	// cancel 取消当前后台工作（用户点「取消」或退出应用时调用）。
	cancel context.CancelFunc

	// gen 是会话语号，每次开始检查/下载时 +1。
	//
	// 为什么需要：后台工作结束（用户取消 / 失败）之后，已经排队但还没被主线程
	// 执行的进度回调仍会到达。没有代号时它们会把 stage 从「已取消」改回
	// 「下载中」，界面卡在一个永远不动的百分比上（下载早已停止）。
	// 与 relayclient 里 searchSeq/loadSeq 同一套做法：用序号丢弃过期投递。
	gen uint64
}

// nextGen 开启新会话并返回它的代号。
func (s *updateSession) nextGen() uint64 {
	s.gen++
	return s.gen
}

// ---- 动作（可注入，供测试替换）----
//
// 这些字段在测试里被替换成假实现，生产路径永远是 nil（走真实实现）。
// 与 a.snap / a.fakeRun 同一套做法，理由也一样：不注入就只能真下载真替换，
// 那会毁掉跑测试的这台机器。

// runAsync 启动后台工作。生产是 `go fn()`；测试替换成同步执行，于是「点击 →
// 检查 → 下载 → 替换 → 重启」整条链路可以在一条线程上断言。
func (a *app) runAsync(fn func()) {
	if a.asyncFn != nil {
		a.asyncFn(fn)
		return
	}
	go fn()
}

// runMain 把 fn 投递到主线程。生产走 mygo.RunOnMain（它同时会唤醒事件循环）；
// 测试替换成直接调用。
func (a *app) runMain(fn func()) {
	if a.mainFn != nil {
		a.mainFn(fn)
		return
	}
	mygo.RunOnMain(fn)
}

func (a *app) checkRemote(ctx context.Context, o update.Options) (update.Info, error) {
	if a.checkFn != nil {
		return a.checkFn(ctx, o)
	}
	u, err := update.New(o)
	if err != nil {
		return update.Info{}, err
	}
	return u.Check(ctx)
}

func (a *app) downloadArtifact(ctx context.Context, o update.Options, info update.Info) (string, error) {
	if a.downloadFn != nil {
		return a.downloadFn(ctx, o, info)
	}
	u, err := update.New(o)
	if err != nil {
		return "", err
	}
	return u.Download(ctx, info, nil)
}

func (a *app) applyArtifact(o update.Options, downloaded, target string) (string, error) {
	if a.applyFn != nil {
		return a.applyFn(o, downloaded, target)
	}
	u, err := update.New(o)
	if err != nil {
		return "", err
	}
	// BackupTag 用当前版本号，备份文件名读起来是 reply9842.old-0.2.1 —— 比
	// mtime 时间戳更容易判断「退回到哪个版本」。
	return u.Apply(downloaded, target, update.ApplyOptions{
		BackupTag: strings.TrimPrefix(strings.TrimSpace(version), "v"),
	})
}

func (a *app) executablePath() (string, error) {
	if a.selfPathFn != nil {
		return a.selfPathFn()
	}
	return update.SelfPath()
}

// relaunch 重启到新版本。
//
// # 为什么用 mygo.App.Relaunch 而不是 update.Restart
//
// 这是本文件里最重要的一个取舍，理由是单实例锁的**时序**（一手读 mygo v0.9.0）：
//
//	update.Restart 的契约是「spawn 之后调用方自己退出」，它只给了一个
//	  BeforeSpawn 钩子用于释放锁。而 mygo 的锁没有导出释放 API ——
//	  releaseSingleInstanceLock 是包内函数，只挂在 App.OnQuit 上
//	  （single_instance.go:101）。于是 BeforeSpawn 里能做的只有 App.Quit()，
//	  那是投递到主线程的异步动作：spawn 立刻发生，旧进程还没退，新进程连上
//	  socket 把 argv 转发给旧进程后自己退出 —— 正好是 update/restart.go 文件头
//	  描述的那个「假重启」。
//
//	mygo.App.Relaunch 把顺序编码在 finish() 里：
//	  app.go:318  relaunchNow() // after OnQuit, which releases the single instance lock
//	  即 onQuit（含 releaseSingleInstanceLock）→ 再 spawn。
//
// 它还替我们处理了两件平台细节：macOS 走 `open -n <bundle>`（直接 exec 的子进程
// 会留在本实例的 coalition 里，父进程退出时被 loginwindow 一并 SIGTERM，见
// mygo updater.go 的 openBundle 注释），`mygo dev` 下走开发重启协议。
//
// 所以这里只调用它，并把「恢复运行状态」编码进 argv（见 restoreArgs）。
func (a *app) relaunch(args []string) {
	if a.relaunchFn != nil {
		a.relaunchFn(args)
		return
	}
	mygo.App.Relaunch(args...)
}

// ---- 选项组装 ----

// updateOptions 组装一次更新会话的配置。
//
// 镜像的优先级完全交给 update 包（显式 > REPLY9842_MIRROR > DefaultMirrors），
// 这里只负责按契约翻译设置页的值：
//
//	留空   → Options.Mirrors = nil  （未配置：用环境变量或内置默认链）
//	非空   → 逗号切分后的显式列表   （用户说了算，哪怕要 `direct` 直连）
//
// 不在保存设置时校验合法性：用户可能边打字边被写盘（"https://g" 不是合法前缀），
// 那不该让整个设置保存失败。真正的解析在 update.New 里，错误会显示在状态行。
func (a *app) updateOptions(progress update.ProgressFunc) update.Options {
	var mirrors []string
	if raw := strings.TrimSpace(a.updateMirror); raw != "" {
		mirrors = strings.Split(raw, ",")
	}
	return update.Options{
		Kind:           updateKind,
		CurrentVersion: version,
		Mirrors:        mirrors,
		Progress:       progress,
	}
}

// updateBlockedReason 返回「为什么现在不允许自动替换」，空串表示允许。
//
// dev 构建一律禁止：Makefile 用 git describe 注入版本、`go build` 直接跑时是
// "dev"，两者都会被 update.IsDevBuild 判成本地构建。此时替换会把开发者正在改
// 的构建覆盖成 release 产物 —— 那是不可逆的（源码还在，但跑着的不是它了）。
func updateBlockedReason() string {
	if update.IsDevBuild(version) {
		v := strings.TrimSpace(version)
		if v == "" {
			v = "dev"
		}
		return "当前是源码构建（" + v + "），不能自动替换自己；请用编译产物或下载正式版"
	}
	return ""
}

// ---- 流程 ----

// checkUpdate 手动检查更新（设置页按钮）。
func (a *app) checkUpdate() {
	a.beginCheck(false)
}

// beginCheck 开始一次版本检查。auto 为真表示这是「自动更新」触发的，查到新版本
// 后直接进入安装（用户已经通过开关表达了同意）。
func (a *app) beginCheck(auto bool) {
	if a.upd.stage.busy() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	// 保留上一次的 info：界面在「正在检查」期间仍然显示上次查到的版本。
	prev := a.upd.info
	a.upd = updateSession{stage: updChecking, percent: -1, cancel: cancel, info: prev}
	gen := a.upd.nextGen()
	// 选项在主线程组装：它会读 a.updateMirror，而那个字段只在主线程写。
	opts := a.updateOptions(nil)
	a.invalidate()

	a.runAsync(func() {
		info, err := a.checkRemote(ctx, opts)
		a.runMain(func() {
			// 会话已经换了（用户取消后又点了一次 / 应用已退出）：丢弃这次结果。
			if a.upd.gen != gen {
				return
			}
			a.releaseCancel(cancel)
			if errors.Is(err, context.Canceled) {
				a.markCanceled()
				return
			}
			if err != nil {
				// 检查失败也保留 Current/Artifact：界面能说「当前 v0.2.1，检查失败：…」。
				a.upd.info = info
				a.upd.stage, a.upd.msg = updFailed, explainUpdateError(err)
				a.invalidate()
				return
			}
			a.upd.info = info
			a.upd.msg = ""
			if info.HasUpdate {
				a.upd.stage = updAvailable
			} else {
				a.upd.stage = updUpToDate
			}
			a.invalidate()
			if auto && info.HasUpdate {
				a.installUpdate()
			}
		})
	})
}

// installUpdate 执行「下载 → 校验 → 替换 → 重启」。
//
// 阶段安排的理由：下载与替换都在后台 goroutine 里串行完成，只在边界处投递状态。
// 替换阶段包含 `--version` 自检（最长 10s），放到主线程会让界面卡死。
//
// 下载期间**不停止 relay**：服务没必要为了一次下载中断。停止发生在重启的退出
// 序列里（OnBeforeQuit），那时新进程才需要端口。
func (a *app) installUpdate() {
	if a.upd.stage.busy() || !a.upd.info.HasUpdate {
		return
	}
	if reason := updateBlockedReason(); reason != "" {
		a.upd.stage, a.upd.msg = updFailed, reason
		a.invalidate()
		return
	}

	info := a.upd.info
	// 更新前是否在跑：重启后要恢复（需求「自动重启恢复服务」）。
	restoreRelay := a.running()

	a.upd.stage, a.upd.percent, a.upd.cancel = updDownloading, -1, nil
	a.upd.downloaded, a.upd.total, a.upd.mirror = 0, 0, ""
	a.upd.msg = ""
	gen := a.upd.nextGen()
	// cancel 必须在设置 gen 之后才写进会话：nil 检查（cancelUpdate）与代号
	// 守卫共同保证「取消一次只终止一次会话」。
	ctx, cancel := context.WithCancel(context.Background())
	a.upd.cancel = cancel
	opts := a.updateOptions(func(pg update.Progress) {
		// 进度回调在下载 goroutine 里同步触发（update 包已按 256KB/100ms 节流），
		// 这里只投递，不在回调里碰任何界面状态。投递可能迟到，所以带上代号。
		a.runMain(func() { a.applyProgressWithGuard(gen, pg) })
	})
	a.invalidate()

	// fail 是这条流水线的唯一失败出口：取消 → 回退到「可重试」而不是「失败」。
	fail := func(err error) {
		a.runMain(func() {
			if a.upd.gen != gen {
				return
			}
			a.releaseCancel(cancel)
			if errors.Is(err, context.Canceled) {
				a.markCanceled()
				return
			}
			a.upd.stage, a.upd.percent = updFailed, -1
			a.upd.msg = explainUpdateError(err)
			a.invalidate()
		})
	}

	a.runAsync(func() {
		downloaded, err := a.downloadArtifact(ctx, opts, info)
		if err != nil {
			fail(err)
			return
		}
		// 目标路径用 update.SelfPath 的语义而不是 os.Args[0]：macOS 上进程跑在
		// bundle 副本里，它就是「下次启动会被执行的那个文件」（见 update.SelfPath
		// 的注释与 bundle_darwin.go）。
		target, err := a.executablePath()
		if err != nil {
			fail(err)
			return
		}
		_, err = a.applyArtifact(opts, downloaded, target)
		// ErrDeferred 不是失败：Windows 上目标被占用时，update 包已经把校验+自检
		// 通过的 .new 放在目标旁边并写了标记，等下一次启动完成替换。此时**仍然
		// 重启**——新进程启动早期会调 ApplyPendingIfAny 完成它（见 main.go），
		// 所以用户体验仍是「点一下就好了」，而不是「下次手动打开才生效」。
		deferred := errors.Is(err, update.ErrDeferred)
		if err != nil && !deferred {
			fail(err)
			return
		}
		// 替换成功才写 macOS 标记：它要挡的是「下次从旧裸二进制启动把副本盖回去」。
		if !deferred {
			markBundleUpdated(target)
		}

		a.runMain(func() {
			if a.upd.gen != gen {
				return
			}
			a.releaseCancel(cancel)
			a.upd.stage, a.upd.percent = updRestarting, 100
			a.upd.msg = ""
			if deferred {
				a.upd.msg = "目标文件被占用，重启后完成替换"
			}
			a.invalidate()
			a.relaunch(a.restoreArgs(restoreRelay))
		})
	})
}

// cancelUpdate 用户点「取消」：中止下载/校验（替换阶段不可中断——那一步要么完成
// 要么回滚，中途停手才是最危险的状态）。
func (a *app) cancelUpdate() {
	if a.upd.cancel == nil {
		return
	}
	a.upd.cancel()
	a.markCanceled()
}

// releaseCancel 释放 ctx 并清掉取消入口（后台工作已经结束，再点「取消」无意义）。
func (a *app) releaseCancel(cancel context.CancelFunc) {
	if a.upd.cancel != nil {
		a.upd.cancel = nil
	}
	cancel()
}

// markCanceled 回到「可以重新开始」的状态。
//
// 取消不是错误：用户主动停手，不该看到红色错误行。若已经查到新版本，就退回
// 「发现新版本」等用户再点；否则回到初始态。
func (a *app) markCanceled() {
	a.upd.nextGen() // 作废已排队但还没执行的进度回调（见 updateSession.gen）
	a.upd.cancel = nil
	a.upd.percent, a.upd.downloaded, a.upd.total = -1, 0, 0
	if a.upd.info.HasUpdate {
		a.upd.stage = updAvailable
	} else {
		a.upd.stage = updCanceled
	}
	a.upd.msg = "已取消"
	a.invalidate()
}

// applyProgressWithGuard 只接受当前会话的进度（过期的直接丢弃）。
//
// 判据是代号而不是 stage：stage 在取消后可能恰好又回到「下载中」（用户重新点
// 了更新），那时旧会话的迟到回调会污染新会话的百分比。
func (a *app) applyProgressWithGuard(gen uint64, pg update.Progress) {
	if a.upd.gen != gen {
		return
	}
	a.applyProgress(pg)
}

// applyProgress 把 update.Progress 映射到界面状态。
func (a *app) applyProgress(pg update.Progress) {
	switch pg.Stage {
	case update.StageCheck:
		a.upd.stage = updChecking
	case update.StageDownload:
		a.upd.stage = updDownloading
		a.upd.percent = pg.Percent
		if pg.Downloaded > 0 {
			a.upd.downloaded = pg.Downloaded
		}
		if pg.Total > 0 {
			a.upd.total = pg.Total
		}
		if pg.Mirror != "" {
			a.upd.mirror = pg.Mirror
		}
	case update.StageVerify:
		a.upd.stage = updVerifying
		a.upd.percent = pg.Percent
	case update.StageApply:
		a.upd.stage = updApplying
	case update.StageRestart:
		a.upd.stage = updRestarting
	}
	a.invalidate()
}

// ---- 重启与状态恢复 ----

// startMarkers 是「启动后立即转发」的几个 argv 形态（-start 与它的旧别名）。
var startMarkers = []string{"-start", "--start", "-autostart", "--autostart"}

// restoreArgs 计算「更新后重启」时传给新实例的参数。
//
// 三件事：
//
//  1. 保留当前参数（-listen / -server 这类是用户意图，丢掉等于把用户改的配置
//     还原成默认值）。用 update.RestartArgsFrom 而不是手抄 os.Args：
//     它已经处理了 `--flag value` 的成对语义。
//  2. 注入 -no-auto-update：新进程是「更新后重启」起来的，让它立刻又查一遍
//     只会把检查次数翻倍（且病理情况下无限重启）。它是一次性的，下一次重启
//     会把它摘掉（第 1 步的 exclude）。
//  3. 若更新前 relay 在跑，补一个启动标记 —— 新进程起来就自动恢复转发，这是
//     需求里「自动重启恢复服务」的那一半。托盘常驻态（窗口已收起）用
//     --autostart：它表达「不弹窗 + 继续转发」，否则一次后台更新会给用户糊一个
//     本来不存在的窗口。
func (a *app) restoreArgs(restoreRelay bool) []string {
	args := update.RestartArgsFrom(noAutoUpdateFlag)
	hasStart := false
	for _, s := range args {
		for _, m := range startMarkers {
			if s == m || strings.HasPrefix(s, m+"=") {
				hasStart = true
			}
		}
	}
	if restoreRelay && !hasStart {
		if a.win == nil && a.tray != nil {
			// --autostart（双横线）在 main.go 里是**登录项标记**：loginAtLaunch 认它，
			// 效果正是「只驻留托盘 + 立刻开始转发 + 不弹窗」。这里复用它而不是新造
			// 一个 flag，因为要的语义与登录启动完全一致。注意它与单横线的
			// -autostart（= -start，会开窗）不是同一件事，别改成单横线。
			args = append(args, autostart.Marker)
		} else {
			args = append(args, "-start")
		}
	}
	return append(args, noAutoUpdateFlag)
}

// noAutoUpdateFlag 是「本次启动不检查更新」的一次性 flag。
//
// 两条约束：
//
//  1. 更新自己重启时必须**注入**它（否则新进程立刻又检查一遍，病理情况下
//     ——比如替换没真正生效——会变成无限重启）。
//  2. 之后任何一次重启都要**摘掉**它（用户手工重启、或下一次真正的更新重启
//     都该恢复正常的自动检查），所以 restoreArgs 把它列进 exclude。
const noAutoUpdateFlag = "-no-auto-update"

// maybeAutoUpdate 启动时的自动检查（开关关闭时什么都不做）。
func (a *app) maybeAutoUpdate() {
	if a.skipAutoUpdate || !a.autoUpdate {
		return
	}
	if reason := updateBlockedReason(); reason != "" {
		// dev 构建：开关在界面上是禁用的，理论上不会走到这里；但 settings.json
		// 可能被手改，或者用户在 release 版打开开关后换了源码构建。记一行日志
		// 而不是弹提示——这不是错误，只是一个被忽略的偏好。
		a.logs.Write([]byte("自动更新已跳过：" + reason + "\n"))
		return
	}
	a.logs.Write([]byte("自动更新：开始检查最新版本\n"))
	a.beginCheck(true)
}

// cancelUpdateQuietly 取消后台更新，但不改界面状态（退出流程里不需要刷新界面）。
func (a *app) cancelUpdateQuietly() {
	if a.upd.cancel != nil {
		cancel := a.upd.cancel
		a.upd.cancel = nil
		cancel()
	}
}

// bundleUpdateStamp 是「bundle 副本由更新器写入过」的标记文件名（仅 macOS）。
//
// 位置在 Contents/ 而不是 Contents/MacOS/：MacOS 目录里只应放可执行文件。
const bundleUpdateStamp = ".reply9842-updated"

// markBundleUpdated 在 macOS bundle 场景下记录「副本已被更新器替换过」。
//
// 要挡的是 bundle_darwin.go 里 copyIfChanged 的回滚路径：用户下次双击的是下载
// 目录里那个**旧版裸二进制**，它按「大小不同 / 源更新」判断就会把新副本覆盖回
// 旧版，一次更新白做。标记带 mtime，copyIfChanged 据此跳过覆盖（见
// updatedBundleBeats）。
func markBundleUpdated(target string) {
	if runtime.GOOS != "darwin" || !update.InAppBundle(target) {
		return
	}
	// 写失败不阻塞更新：更新本身已经成功，标记只是「防止下次被旧二进制盖回去」的
	// 加固。写不进去时退回 update/bundle.go 里记录的残余风险。
	_ = writeBundleStamp(bundleContentsDir(target), version)
}

// writeBundleStamp 写标记文件（contents 为空表示不在 bundle 内，什么都不做）。
//
// 返回 error 只为让测试能断言「真的写了」。
func writeBundleStamp(contents, ver string) error {
	if contents == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(contents, bundleUpdateStamp), []byte(ver+"\n"), 0o644)
}

// selfPathOrEmpty 是 update.SelfPath 的「失败返回空串」包装。
//
// 启动早期调用它只是为了问「有没有待应用的更新」：拿不到自身路径就没有待应用
// 更新可言（那种环境下也不可能有更新的东西），不该因此让 GUI 起不来。
func selfPathOrEmpty() string {
	p, err := update.SelfPath()
	if err != nil {
		return ""
	}
	return p
}

// spawnSelfRestarted 完成一次「已替换但内存里仍是旧映像」的重启。
//
// 这一步发生在**取单实例锁之前**，所以可以朴素地 spawn：没有锁要释放，
// 旧进程紧接着 return 退出。带 -no-auto-update：新映像刚装好，不必再查一次。
//
// 用 update.Restart 而不是 mygo.App.Relaunch：后者的语义是「退出事件循环之后
// 再拉起」，而此刻事件循环还没开始（App.Run 都没调过）。update.Restart 是
// 「spawn 完立刻返回」，正合这个阶段。
func spawnSelfRestarted() error {
	target, err := update.SelfPath()
	if err != nil {
		return err
	}
	args := append(update.RestartArgsFrom(noAutoUpdateFlag), noAutoUpdateFlag)
	return update.Restart(target, args, update.RestartOptions{
		// 脱离控制终端：这个进程可能是从 launchd / 快捷方式拉起来的，
		// 旧进程退出不该把新进程带走（见 update/restart_unix.go）。
		Detach: true,
	})
}

// bundleContentsDir 从 Contents/MacOS/<exec> 反推 Contents 目录。
func bundleContentsDir(exe string) string {
	macos := filepath.Dir(exe)
	if filepath.Base(macos) != "MacOS" {
		return ""
	}
	return filepath.Dir(macos)
}

// updatedBundleBeats 判断 bundle 副本是否应当压过（不被覆盖）源二进制。
//
// 语义：**更新器写过的副本**优先，除非源文件比那次更新还新。
//
//   - stampMod 为零值（没有标记）→ false，走原来的 mtime/size 比较
//   - 源文件的 mtime 晚于标记 → false，让源覆盖副本
//     （开发者 go build 出更版、或用户下载了更新的版本，必须尊重）
//   - 否则 → true，副本留着
//
// 独立成函数是为了能在任意平台上单测：真正的 copyIfChanged 只在 darwin 编译。
func updatedBundleBeats(srcMod, stampMod time.Time) bool {
	if stampMod.IsZero() {
		return false
	}
	return !srcMod.After(stampMod)
}

// ---- 错误翻译（可操作，不是裸 error）----

// explainUpdateError 把 update 包的哨兵错误翻译成「用户此刻该做什么」。
//
// 需求明确要求区分三类并给出不同动作：
//
//	网络失败  → 建议配镜像
//	校验失败  → 说明可能被劫持/镜像返回了错误内容
//	权限不足  → 说明要手工处理，且说明本程序不会提权
//
// 无法识别的错误保留原始串（宁可糙，不可错）。
func explainUpdateError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "已取消"
	case errors.Is(err, update.ErrRateLimited):
		return "GitHub 接口已限流（未登录 60 次/小时），请一小时后再试"
	case errors.Is(err, update.ErrNoRelease):
		return "仓库还没有正式 release，暂时无法更新"
	case errors.Is(err, update.ErrChecksum):
		return "校验失败：下载内容与官方 sha256 不符，可能被中间人篡改或镜像返回了错误文件。当前版本未被改动，请换一个镜像重试"
	case errors.Is(err, update.ErrSizeMismatch):
		return "下载不完整（收到的大小与声明不符）。重试即可，已下载的部分会续传"
	case errors.Is(err, update.ErrSelfCheck):
		return "下载的产物无法运行（架构不符或已损坏），已放弃替换，当前版本未被改动"
	case errors.Is(err, update.ErrPermission):
		return "没有写入权限，当前版本未被改动。本程序不会自行提权：请把程序放到你的用户目录（如 ~/Applications）后再更新"
	case errors.Is(err, update.ErrDeferred):
		return "文件被系统占用，更新已准备好，下次启动时替换生效"
	case errors.Is(err, update.ErrNotExecutable):
		return "目标文件不可执行，当前版本未被改动。请检查安装目录"
	case isNetworkError(err):
		return "网络失败：检查网络连接，或在设置里配置「更新镜像」加速。原始错误：" + err.Error()
	}
	return err.Error()
}

// isNetworkError 识别「换镜像/重试能救」的错误。
//
// 判定依据是类型而不是文本：update 包用 %w 包装，*url.Error 与 net.Error
// 会原样留在链上，比匹配 "timeout"/"no such host" 之类的字符串可靠
// （那些串在非英文 locale 下会变）。
func isNetworkError(err error) bool {
	var (
		urlErr *url.Error
		netErr net.Error
		opErr  *net.OpError
	)
	return errors.As(err, &urlErr) || errors.As(err, &netErr) || errors.As(err, &opErr)
}

// ---- 设置页的三行 ----

// autoUpdateRow 自动更新开关。
//
// 默认关闭（settings.json 的 AutoUpdate 零值），dev 构建下禁用并说明原因：
// 让开关看起来可用、点下去却什么都不做，比灰掉它更让人困惑。
func (a *app) autoUpdateRow(c *ui.Context, p *glass.Palette) {
	dev := update.IsDevBuild(version)
	ui.Row(c).Padding(6, 0).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Width(84).Shrink(0).Children(func() {
			ui.Text(c, "自动更新").FontSize(11).TextColor(p.MutedForeground).SingleLine()
		})

		hint := "启动时检查并安装新版本（会重启一次）"
		if dev {
			hint = "源码构建不可自动替换自己"
		}
		ui.Text(c, hint).FontSize(10.5).TextColor(p.Tertiary).MaxLines(1).Grow(1)
		sw := ui.Switch(c, &a.autoUpdate).Disabled(dev)
		if !dev {
			// OnChange 在绑定值变化后、重绘前触发：写盘失败会在这里被看到。
			sw.OnChange(a.applyUpdateSettings)
		}
	})
}

// mirrorRow 更新镜像输入框。
//
// 留空的语义与 update 包一致（未配置 → 环境变量 → 内置加速链），所以占位文案
// 必须说清「留空＝自动」，而不是列出内置列表（那只会在版本升级后过期）。
func (a *app) mirrorRow(c *ui.Context, p *glass.Palette) {
	ui.Row(c).Padding(6, 0).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Width(84).Shrink(0).Children(func() {
			ui.Text(c, "更新镜像").FontSize(11).TextColor(p.MutedForeground).SingleLine()
		})
		ui.Box(c).Grow(1).Children(func() {
			ui.TextInput(c, &a.updateMirror).
				Placeholder("留空＝自动（内置加速链）；多个用英文逗号分隔").
				FontSize(11).Height(26).Radius(9).
				OnChange(a.applyUpdateSettings)
		})
	})
}

// updateRow 检查/更新按钮 + 状态行。
//
// 一个按钮承担三种动作，因为设置页只有一行空位：
//
//	空闲/已是最新  → 「检查更新」
//	发现新版本    → 「更新到 v0.3.0」（主按钮，这是用户此刻唯一想做的事）
//	进行中        → 「取消」（下载可以用 ctx 中断；替换阶段不可中断，按钮不出现）
//
// 状态文字与按钮同排：进度放在按钮右边，视线不需要在两处之间跳。
func (a *app) updateRow(c *ui.Context, p *glass.Palette) {
	dev := update.IsDevBuild(version)
	s := a.upd

	ui.Row(c).Padding(6, 0).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Width(84).Shrink(0).Children(func() {
			// 标签与按钮刻意不同字：两处都写「检查更新」时，点击测试与读屏
			// 都会先命中那个不可点的标签。
			ui.Text(c, "软件更新").FontSize(11).TextColor(p.MutedForeground).SingleLine()
		})

		switch {
		case s.stage.cancelable() && s.cancel != nil:
			if a.actionButton(c, p, "update-cancel", "取消", false).Clicked() {
				a.cancelUpdate()
			}
		case s.stage == updAvailable && !dev:
			if a.actionButton(c, p, "update-install", "更新到 "+s.info.Latest, true).Clicked() {
				a.installUpdate()
			}
		default:
			btn := a.actionButton(c, p, "update-check", "检查更新", false)
			if s.stage.busy() {
				btn = btn.Disabled(true)
			}
			if btn.Clicked() {
				a.checkUpdate()
			}
		}

		// 检查中给一个转圈：它的耗时不可预测（API 2-15s），没有动的指示会让人
		// 以为按钮没接上。其余阶段都用文字进度，转圈会让数字读起来更慢。
		if s.stage == updChecking {
			ui.Spinner(c).Size(11, 11).Shrink(0).TextColor(p.MutedForeground)
		}

		text, color := a.updateStatusText(p)
		ui.Text(c, text).FontSize(10.5).TextColor(color).MaxLines(2).Grow(1)
	})
}

// updateStatusText 状态行的文案与颜色（唯一一处，避免按钮与提示各说一套）。
func (a *app) updateStatusText(p *glass.Palette) (string, ui.Color) {
	s := a.upd
	dev := update.IsDevBuild(version)

	switch s.stage {
	case updChecking:
		return "正在检查最新版本…", p.MutedForeground
	case updDownloading:
		if s.percent >= 0 {
			return clampLine(fmt.Sprintf("正在下载 %d%% · %s / %s · %s",
				s.percent, humanBytes(s.downloaded), humanBytes(s.total), s.mirror)), p.MutedForeground
		}
		return clampLine(fmt.Sprintf("正在下载 %s · %s", humanBytes(s.downloaded), s.mirror)), p.MutedForeground
	case updVerifying:
		return "正在校验下载内容…", p.MutedForeground
	case updApplying:
		return "正在替换程序文件（旧版本会保留一份备份）…", p.MutedForeground
	case updRestarting:
		return "正在重启…", p.Success
	case updFailed:
		return s.msg, p.Danger
	case updCanceled:
		return "已取消（可重新检查）", p.Tertiary
	case updUpToDate:
		return "已是最新（当前 " + displayVersion(s.info.Current) + "）", p.Tertiary
	case updAvailable:
		if dev {
			// dev 构建也把「正式版走到哪了」告诉开发者，但不给按钮。
			return "源码构建不参与自动替换；最新正式版是 " + s.info.Latest, p.Warning
		}
		return "发现新版本 " + s.info.Latest + "（当前 " + displayVersion(s.info.Current) + "）", p.Success
	}
	if dev {
		return "源码构建（" + strings.TrimSpace(version) + "）：可检查，但不能替换自己", p.Tertiary
	}
	if v := displayVersion(version); v != "" {
		return "当前 " + v, p.Tertiary
	}
	return "当前版本未知", p.Tertiary
}

// clampLine 把过长的状态行压到一行内（镜像名 + 字节数很容易超宽）。
func clampLine(s string) string {
	const max = 72
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// applyUpdateSettings 把自动更新开关与镜像写盘，并让状态行跟着刷新。
//
// 镜像只在**下一次**构造 Updater 时生效（update.New 会解析一次并在会话内固定），
// 所以这里不改动进行中的会话——用户改了镜像却看到「当前这次下载仍走旧镜像」，
// 比悄悄换掉中途的下载更可预期。
func (a *app) applyUpdateSettings() {
	if err := a.saveSettings(); err != nil {
		a.setNotice("保存设置失败："+err.Error(), true)
	}
	a.invalidate()
}
