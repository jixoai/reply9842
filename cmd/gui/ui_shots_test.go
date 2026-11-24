//go:build darwin

// ui_shots_test.go 生成「设置页几个关键状态」的截图，供视觉验收与官网素材。
//
// 为什么带 darwin tag：截图依赖真实渲染（字体、抗锯齿），Linux runner 上出图
// 不可比；更实际的原因是它用到 site_shots_test.go 里的 demoRoutes/demoSnaps，
// 而那个文件本身是 darwin-only。CI 的 `go vet ./...` 是全平台的，把这条断言
// 放在跨平台文件里会让 Linux/Windows 的 vet 直接失败（v0.4.0 发布时踩过一次）。
package main

import (
	"errors"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/jixoai/reply9842/internal/relayclient"
	"github.com/jixoai/reply9842/internal/route"
	"github.com/jixoai/reply9842/internal/stats"
)

func shot(t *testing.T, a *app, path string) {
	t.Helper()
	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	img := tt.Image()
	if img == nil {
		t.Fatal("渲染为空")
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// 合并后的设置页（折叠态）：应该只剩 监听地址 / 线路。
//
// appliedRoutes 必须与 routesDraft 一致，否则这张图会带着「有改动未生效」+
// 「立即应用」——那是另一个状态（shot-routes-dirty 的职责）。视觉验收时
// 它就因为折叠态多出这枚胶囊而质疑夹具（0/0/1 的期望对不上）。
func TestShotMergedCollapsed(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.tab = tabSettings
	a.routesDraft = demoRoutes()
	a.routeSnaps = demoSnaps()
	a.appliedRoutes = a.routesDraft.Normalize() // 干净态：没有未应用改动
	shot(t, a, "/tmp/shot-merged-collapsed.png")
}

// 草稿态：点「＋代理」后应有的表单 + 测试连接 + 取消/添加
func TestShotPendingDraft(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.tab = tabSettings
	a.routesExpanded = true
	a.routesDraft = demoRoutes()
	a.routeSnaps = demoSnaps()
	a.beginAddRoute(route.KindProxy)
	shot(t, a, "/tmp/shot-pending-draft.png")
}

// 未应用态：运行中改了线路，列表下方应出现「有改动未生效」+ 立即应用
func TestShotRoutesDirty(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.tab = tabSettings
	a.routesExpanded = true
	a.routesDraft = demoRoutes()
	a.routeSnaps = demoSnaps()
	// 模拟「已应用的配置」与当前不同
	// 已应用的配置里「备用中继」是启用的，当前 draft 里是停用的 → dirty
	on := true
	a.appliedRoutes = route.Set{}
	for _, r := range demoRoutes() {
		r.Enabled = &on
		a.appliedRoutes = append(a.appliedRoutes, r)
	}
	a.appliedRoutes = a.appliedRoutes.Normalize()
	shot(t, a, "/tmp/shot-routes-dirty.png")
}

// 草稿态的每个控件都必须完整落在 footer 之上。
//
// 视觉复验 N1：早期草稿动作行被视口折线切掉 4px（底部圆角都没画完，读起来
// 像按钮坏了）。这条把它变成可断言的几何约束，以后加字段/加提示时若把内容
// 推到折线上会立刻红。
func TestDraftControlsFitAboveFooter(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.tab = tabSettings
	a.routesExpanded = true
	a.routesDraft = demoRoutes()
	a.routeSnaps = demoSnaps()
	a.beginAddRoute(route.KindProxy)

	tt := ui.NewTester(a.view, defaultWinWidth, defaultWinHeight)
	// footer 顶边：与 site_shots_test.go 的 footerTop 同一基准
	const footerTop = 607
	for _, label := range []string{
		"新建代理线路", "名称", "服务器", "代理地址", "测试连接", "取消", "添加",
	} {
		r, ok := tt.Find(label)
		if !ok {
			t.Errorf("草稿态找不到 %q", label)
			continue
		}
		if r.Y+r.H > footerTop {
			t.Errorf("%q 底边 %.0f 越过 footer 顶边 %d（会被折线切断）", label, r.Y+r.H, footerTop)
		}
	}
}

// 编辑已有线路的字段区（改 server/SNI/源地址）。
func TestShotRouteEditing(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.tab = tabSettings
	a.routesExpanded = true
	a.routesDraft = demoRoutes()
	a.routeSnaps = demoSnaps()
	a.appliedRoutes = a.routesDraft.Normalize()
	// 编辑「备用中继」：它的字段最全（服务器/证书域名/源地址）
	for i := range a.routesDraft {
		if a.routesDraft[i].Kind == route.KindRelay {
			a.setEditingRoute(i)
		}
	}
	shot(t, a, "/tmp/shot-route-editing.png")
}

// 测试连接成功的状态（注入探测结果，不真拨号）。
func TestShotProbeSuccess(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.tab = tabSettings
	a.routesExpanded = true
	a.routesDraft = demoRoutes()
	a.routeSnaps = demoSnaps()
	a.appliedRoutes = a.routesDraft.Normalize()
	a.beginAddRoute(route.KindProxy)
	a.pendingRoute.Proxy = "http://127.0.0.1:7890"
	a.pendingProbe = &relayclient.ProbeResult{
		HandshakeMS: 46, RTTMS: 12, Transport: "tcp", Dcz2: true,
	}
	shot(t, a, "/tmp/shot-probe-success.png")
}

// 测试连接失败的状态：错误信息要点出该改哪一项。
func TestShotProbeFailure(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.tab = tabSettings
	a.routesExpanded = true
	a.routesDraft = demoRoutes()
	a.routeSnaps = demoSnaps()
	a.appliedRoutes = a.routesDraft.Normalize()
	a.beginAddRoute(route.KindRelay)
	a.pendingRoute.Server = "47.117.152.3:28443"
	a.pendingProbeErr = errors.New(
		"tls: failed to verify certificate: x509: cannot validate certificate for 47.117.152.3 " +
			"because it doesn't contain any IP SANs\n→ 这条线路指向 IP 但证书只签了域名：请在「证书域名」里填证书上的域名")
	shot(t, a, "/tmp/shot-probe-failure.png")
}

// 面板 tab（首页）：确认整体风格没被这次改动带偏。
func TestShotPanelTab(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.tab = tabPanel
	a.routesDraft = demoRoutes()
	a.appliedRoutes = a.routesDraft.Normalize()
	snap := stats.Agg{Requests: 12847, UpRaw: 1400 * 1024, UpWire: 1024,
		UpRatio: 1024.0 / (1400 * 1024), P50Ms: 28}
	a.snap = func() stats.Agg { return snap }
	shot(t, a, "/tmp/shot-panel.png")
}

// 面板 + 多线路分布（owner 2026-11-25 第 1 条：线条应按线路区分）。
func TestShotPanelRouteMix(t *testing.T) {
	a := newTestApp(t)
	setRunning(a, true)
	a.tab = tabPanel
	a.routesDraft = demoRoutes()
	a.appliedRoutes = a.routesDraft.Normalize()

	// 造一组有真实形状的窗口数据：三条线路按不同比例承担上行，
	// 并给几个桶填延迟，让两张图都有内容。
	series := make([]stats.Bucket, 12)
	now := time.Now()
	for i := range series {
		series[i].At = now.Add(time.Duration(i-11) * time.Minute)
		hk := int64(42000 + i*3200)
		sh := int64(13000 + i*900)
		px := int64(4000 + i*200)
		series[i].UpRaw = (hk + sh + px) * 3
		series[i].UpWire = hk + sh + px
		series[i].DownWire = (hk + sh + px) / 2
		series[i].Count = 6
		series[i].MsSum = 6 * int64(900+i*80)
		series[i].ByRoute = map[string]*stats.RouteBucket{
			"香港":  {UpWire: hk, Count: 4, MsSum: 4 * int64(700+i*60)},
			"上海":  {UpWire: sh, Count: 1, MsSum: int64(1800 + i*120)},
			"代理甲": {UpWire: px, Count: 1, MsSum: int64(2400 + i*150)},
		}
	}
	snap := stats.Agg{
		Requests: 72, UpRaw: 2100000, UpWire: 700000,
		UpRatio: 700000.0 / 2100000, P50Ms: 880, P90Ms: 2600,
		Series: series, WindowSec: 720, BucketSec: 60, RetainSec: 86400,
	}
	a.snap = func() stats.Agg { return snap }
	shot(t, a, "/tmp/shot-panel-routemix.png")
}
