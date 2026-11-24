//go:build darwin

// ui_shots_test.go 生成「设置页几个关键状态」的截图，供视觉验收与官网素材。
//
// 为什么带 darwin tag：截图依赖真实渲染（字体、抗锯齿），Linux runner 上出图
// 不可比；更实际的原因是它用到 site_shots_test.go 里的 demoRoutes/demoSnaps，
// 而那个文件本身是 darwin-only。CI 的 `go vet ./...` 是全平台的，把这条断言
// 放在跨平台文件里会让 Linux/Windows 的 vet 直接失败（v0.4.0 发布时踩过一次）。
package main

import (
	"image/png"
	"os"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/jixoai/reply9842/internal/route"
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
