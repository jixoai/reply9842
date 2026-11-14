// view.go — 界面绘制（mygo native UI 的模型：view 是状态的纯函数）
//
// 布局（2026-11-13 第二轮反馈后重排）：
//
//	titlebar  品牌标 + 应用名 + [面板｜设置] tabs + 复制地址徽标 + 启动/停止图标开关
//	面板 tab  上行压缩图（吃掉剩余高度）· 启动失败说明（仅失败时）· 统计摘要
//	设置 tab  上游 / 监听 / 传输方式 / 开机自启
//	footer    运行态 + 传输方式 + 上游 + 版本号（版本号只在这里出现）
//
// 为什么面板不再有状态行：它只有两件事——「连接状态」和「启动/停止」——两件都
// 已经被顶栏接管（图标开关的形状与颜色就是状态，点它就是那个动作）。一行 38px
// 的卡片只为了重复顶栏已有的信息，却占着首屏最贵的位置。
//
// 空态的两条硬规则：
//   - 不摆 68px 巨号：空态没有数字，摆「—」是一根读作「坏掉的进度条」的粗横条，
//     摆「0×」又会被读成「压缩率是 0」
//   - 三格统计整条不出现：空卡占 25% 面积却零信息量，空态的信息由 hero 的一句话承担
package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/jixoai/reply9842/internal/autostart"
	"github.com/jixoai/reply9842/internal/glass"
	"github.com/jixoai/reply9842/internal/netif"
	"github.com/jixoai/reply9842/internal/relayclient"
	"github.com/jixoai/reply9842/internal/stats"
)

// tab 索引，与 titleBar 里 ui.Tabs 的顺序一致
const (
	tabPanel = iota
	tabSettings
)

// view 每一帧重建界面
func (a *app) view(c *ui.Context) {
	// 每秒刷一次统计：After 是「到点再画一帧」，不占 CPU。
	// 托盘的菜单文字由同一条心跳驱动（见 trayUI.tick），两处口径因此不会漂移。
	c.After(time.Second)
	a.agg = a.snapshot()

	// 整个窗口套上 solid-glass 色板：控件（按钮/输入框/Select/滚动条）
	// 从这里取 Surface/Border/Accent，不必逐个重画
	c.SetTheme(a.pal.Theme())

	p := a.pal
	// 根节点透明，让 Vibrancy 的桌面材质透上来（设计稿的窗口背景本就半透明）
	c.Root().Background(ui.Transparent)

	a.pal.Page(c).Children(func() {
		a.titleBar(c, p)
		// 内容区吃掉剩余高度，footer 钉在窗口底部。
		ui.Column(c).Grow(1).Children(func() {
			// 面板 tab 用 Column(Grow) 而不是 Scroll：图表要占满窗口剩下的高度
			// （用户把窗口拉大时，该变高的是图，不是空白）。设置 tab 用 Scroll——
			// 它的内容高度固定，窗口太矮时宁可滚也不能把「开机自启」切掉。
			if a.tab == tabSettings {
				ui.Scroll(c).Grow(1).Children(func() {
					// 底边距与左右同为 16：卡片的大扩散阴影会往下吃掉 8-10px，
					// 10 的底边距在截图里会读成「卡片贴着 footer，被裁了一点」。
					ui.Column(c).Padding(16, 16, 16).Gap(10).Children(func() {
						a.settingsPane(c, p)
						a.noticeLine(c, p)
					})
				})
			} else {
				ui.Column(c).Grow(1).Padding(16, 16, 16).Gap(10).Children(func() {
					a.panelPane(c, p)
					a.noticeLine(c, p)
				})
			}
		})
		a.footer(c, p)
	})
}

// ---- titlebar ----

// titleBarHeight 自绘标题栏的高度（DIPs）。
//
// 48 是「红绿灯纵向居中」这个约束反解出来的，不是手感：红绿灯按钮实测 14 高，
// 窗口选项里把它们的左上角放在 y=17（见 main.go 的 TrafficLightPosition），
// 于是灯心在 24 —— 正好是 48 的一半。灯与这一栏的中线共线，视觉上才读作
// 「一条 bar 上的一排控件」，而不是「一排灯飘在一块深色板的上半部」。
//
// 之前这里写的是 max(bar.Height, 44)，而 macOS 在带 NSToolbar 的 hiddenInset
// 窗口里报的 bar.Height 是 66：bar 被撑到 66 高，灯却仍在 y≈19-33，于是灯
// 相对这一栏偏上 7pt（真机截图实测），这正是「和 window-controls 搭不上」的原因。
const titleBarHeight float32 = 48

// titleBarHeightFor 取这一栏的高度：别的平台（Windows 的标题按钮填满
// bar.Bottom）报的控件区比 48 高时，宁可加高也不让控件压在内容上。
func titleBarHeightFor(bar ui.TitleBar) float32 {
	if bar.Bottom+4 > titleBarHeight {
		return bar.Bottom + 4
	}
	return titleBarHeight
}

// titleBar 自绘标题栏：品牌 + [面板｜设置] + 复制地址徽标 + 启动/停止开关。
//
// 拖拽交给 DragWindow（双击最大化的行为也由它负责）。
//
// 两侧都要给窗口控件让位：macOS 的红绿灯在左（bar.Left），Windows 的最小化/
// 最大化/关闭在右（bar.Right）——只让左边的话，Windows 上最右侧那个开关会整块
// 被压在关闭按钮底下，既点不到也看不见。
//
// 版本号不在这里：它和 footer 各显示一次时，用户会以为是两个不同的东西。
func (a *app) titleBar(c *ui.Context, p *glass.Palette) {
	bar := c.TitleBar()
	height := titleBarHeightFor(bar)
	w, _ := c.Size()

	// 让位量：灯/按钮的实际占位 + 一段呼吸。控件区为 0（无边框、全屏）时
	// 退回 14，那是这一栏自己的内边距。
	left, right := float32(14), float32(14)
	if bar.Left > 0 {
		left = bar.Left + 10
	}
	if bar.Right > 0 {
		right = bar.Right + 12
	}

	// 窄窗口下让出应用名：480 宽时右侧那一簇（tabs 100 + 徽标 120 + 开关 26
	// 加两道 9 的间隙）要 264，加上左侧 89 的让位与 12 的右边距，留给品牌
	// 只剩 115——240 加上名字（69）就顶上了。468 是「名字右边还能留 20 空隙」
	// 的下限；比它窄就把名字收起来，留下三条横杠的品牌标：Dock 与菜单栏
	// 已经指明了这是什么应用，而两项都在的代价是元素互相挤压。
	showName := w >= 468

	ui.Row(c).Height(height).FillWidth().
		Padding(0, right, 0, left).
		Gap(8).AlignItems(ui.Center).
		Background(p.PageDim).
		DragWindow().
		Children(func() {
			ui.Row(c).Gap(7).AlignItems(ui.Center).Shrink(0).Children(func() {
				a.brandMark(c, p)
				if showName {
					ui.Text(c, "reply9842").FontSize(12.5).FontWeight(590).
						LetterSpacing(0.06).TextColor(p.Foreground).SingleLine()
				}
			})

			ui.Box(c).Grow(1)

			// tabs 在右上角（用户明确要求），紧挨复制徽标左侧。
			// 用 Segmented 而不是 ui.Tabs：Tabs 的选中态是一条下划线，
			// 在这条 48px 的深色条上几乎看不见；分段控件的选中块与玻璃
			// 材质语言一致（「一片薄玻璃贴在厚玻璃上」）。
			ui.Segmented(c, &a.tab, "面板", "设置").
				FontSize(11.5).Height(26).Shrink(0)

			a.hostChip(c, p)
			a.runToggle(c, p)
		})

	// 底边发丝线：与 body 之间一道分界，让 titlebar 读作一条独立 chrome
	ui.Box(c).Height(1).FillWidth().Background(p.Hairline)
}

// brandMark 品牌标：三条圆角横杠（设计稿的 mark）。
//
// 宽度收在 15 上是为了和 12.5 的行高对齐：横杠的落差 6.2 正好落在字身中线
// 两侧，因此品牌标与「reply9842」在视觉上共用一条中线。
func (a *app) brandMark(c *ui.Context, p *glass.Palette) {
	ui.Box(c).Size(15, 15).Shrink(0).Draw(func(pt *ui.Painter, r ui.Rect) {
		bars := []struct {
			y, w  float32
			alpha float32
		}{{3.2, 10.5, 0.92}, {6.3, 10.5, 0.60}, {9.4, 4.4, 0.95}}
		for _, b := range bars {
			pt.Fill(ui.Rect{X: r.X + 2.2, Y: r.Y + b.y, W: b.w, H: 2.1},
				p.Foreground.Alpha(b.alpha), 1.05)
		}
	})
}

// hostChip 可点击复制的地址（shadcn 的 copy 组件那一套）。
//
// 交互契约：
//
//	默认   地址（mono，可截断）+ 复制图标（图标在 inline-end）
//	悬停   底色抬亮一档、文字提到正文色 —— 整块都是可点区域
//	点击   复制纯 URL 到剪贴板，文字变「已复制」、图标变对勾，1.6 秒后复原
//
// 底栏不再放第二个复制入口：同一个动作在一屏里出现两次会稀释主次，
// 顶栏这枚徽标已经覆盖了「核对地址 + 复制」。回显也只在这里做，口径唯一。
// 为什么显示时去掉 scheme：这一栏只有 480 宽，`http://` 要吃掉约 40px，
// 而复制出去的内容仍是完整 URL（footer 的提示与这里显示同源，见 localURL）。
// 显示上截断用 TruncateMiddle：`127.0.0.…:18899` 保住端口那一半——
// 端口才是用户要核对的那几个数字。
func (a *app) hostChip(c *ui.Context, p *glass.Palette) {
	copied := a.copyFlashing()
	bg, fg, icon := ui.RGBA(255, 255, 255, 0.04), p.MutedForeground, iconCopy
	if copied {
		bg, fg, icon = p.Success.Alpha(0.12), p.Success, iconCheck
	}

	// 窄到 500 以下只留图标：中文字宽随平台字体浮动（Linux 的 fallback 比
	// macOS 宽几个像素），只按 480 算的话右侧那一簇会在 Linux 上反推出窗口边
	// （实测 475.5 > 480-8）。地址文字是便利，开关是主操作——空间紧时先让便利。
	w, _ := c.Size()
	showText := w >= 500

	chip := ui.ButtonBase(c.Key("copy-host")).Shrink(0).Height(22).
		Padding(0, 7).Gap(5).Radius(7).
		AlignItems(ui.Center).Cursor(ui.CursorPointer).
		Label("复制地址").
		InsetShadow(0, 0, 0, 1, ui.RGBA(255, 255, 255, 0.045)).
		Shadow(0, 0, 0, 1, ui.RGBA(255, 255, 255, 0.03))
	switch {
	case chip.Pressed():
		chip = chip.Background(ui.RGBA(255, 255, 255, 0.12))
	case chip.Hovered():
		chip = chip.Background(ui.RGBA(255, 255, 255, 0.085))
	default:
		chip = chip.Background(bg)
	}
	// 悬停时文字也提亮：面积这么小的徽标，只换底色不足以说「这里能点」。
	text := fg
	if chip.Hovered() && !copied {
		text = p.Foreground
	}
	chip.Children(func() {
		// 文字在前、图标在后（icon 落在 inline-end）：图标是「点一下能复制」
		// 的提示，跟在地址末尾读起来像它的一个后缀动作，放在行首则像另一个按钮。
		if showText {
			label := a.hostText()
			if copied {
				label = "已复制"
			}
			ui.Text(c, label).FontSize(10).Font("monospace").
				TextColor(text).SingleLine().TruncateMiddle().MaxWidth(104)
		}
		ui.Icon(c, icon).Size(10, 10).Shrink(0).TextColor(fg)
	})
	if chip.Clicked() {
		a.copyURL(c)
	}
}

// hostText 徽标上显示的地址（去掉 scheme，端口是用户真正要核对的东西）
func (a *app) hostText() string {
	return strings.TrimPrefix(a.localURL(), "http://")
}

// runToggle 启动/停止：顶栏右侧的图标开关。
//
// 它不是「切换按钮」而是一个动作按钮，只是图标与配色随运行状态变：
// 停止时是实心三角（播放 = 开始），运行时是实心方块（停止 = 结束）并且
// 整块染上成功色——用户扫一眼窗口右上角就知道现在到底跑没跑，不必去读字。
//
// 三态底色在这里按帧选择，而不是交给 Theme 的 faces 机制：这个按钮的面色
// 依赖运行状态，要走 faces 就必须让 Theme 随状态变化，那会把 Segmented、
// 输入框一起染色。直接读 Hovered/Pressed 是框架给自绘控件的正道（读它会让
// 悬停变化重建一次视图，代价与一秒一次的心跳同量级）。
func (a *app) runToggle(c *ui.Context, p *glass.Palette) {
	running := a.running()
	key, label := "run-start", "启动代理"
	icon, iconColor := iconPlay, p.Foreground
	rest, hover, pressed := p.Control, p.Hover, p.Pressed
	if running {
		key, label = "run-stop", "停止代理"
		icon, iconColor = iconStop, p.Success
		rest, hover, pressed = p.Success.Alpha(0.16), p.Success.Alpha(0.26), p.Success.Alpha(0.34)
	}

	btn := ui.ButtonBase(c.Key(key)).Size(26, 26).Radius(9).
		Shrink(0).Cursor(ui.CursorPointer).
		Label(label).Tooltip(label).
		InsetShadow(0, 1, 0, 0, p.ControlRim).
		InsetShadow(0, 0, 0, 1, ui.RGBA(255, 255, 255, 0.045)).
		Shadow(0, 0, 0, 1, ui.RGBA(255, 255, 255, 0.05)).
		Shadow(0, 1, 2, 0, ui.RGBA(0, 0, 0, 0.28))
	switch {
	case btn.Pressed():
		btn = btn.Background(pressed)
	case btn.Hovered():
		btn = btn.Background(hover)
	default:
		btn = btn.Background(rest)
	}
	btn.Children(func() {
		ui.Icon(c, icon).Size(11, 11).TextColor(iconColor)
	})
	if btn.Clicked() {
		a.toggle()
	}
}

// 图标（24×24 view box，lucide 的几何）。
//
// 都是实心/描边混排的两组：动作图标（播放、停止）用实心块，因为 11px 下
// 描边三角会糊成三条线；辅助图标（复制、对勾）用描边，因为它们在 10px 上要
// 与 mono 文字的笔画重量相当，实心会变成两个黑点。
var (
	iconPlay  = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><path d="M7 5.4c0-1.1 1.2-1.8 2.2-1.2l10.9 6.6c.9.6.9 1.9 0 2.5L9.2 19.8C8.2 20.4 7 19.7 7 18.6z" fill="currentColor"/></svg>`))
	iconStop  = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><rect x="6.5" y="6.5" width="11" height="11" rx="2.6" fill="currentColor"/></svg>`))
	iconCopy  = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><rect width="13" height="13" x="9" y="9" rx="2.5"/><path d="M5 15.5A2.5 2.5 0 0 1 2.5 13V5A2.5 2.5 0 0 1 5 2.5h8A2.5 2.5 0 0 1 15.5 5"/></svg>`))
	iconCheck = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.8" stroke-linecap="round" stroke-linejoin="round"><path d="M20 6.5 9.5 17 4 11.5"/></svg>`))
)

// ---- 面板 tab ----

// panelPane 「面板」tab 的全部内容。
//
// 没有状态行：连接状态与主操作都已经被顶栏接管（见 runToggle），这里只剩
// 「效果如何」——图占满剩余高度，失败时在上面补一句原因。
func (a *app) panelPane(c *ui.Context, p *glass.Palette) {
	a.failureLine(c, p)
	// 两张图的分工（owner 2026-11-13 要求）：
	//   上：压缩率——「省了多少」，本产品的卖点
	//   下：流量与延迟——「现在顺不顺」，压缩率图完全看不见的维度
	//
	// 高度分配（owner 2026-11-14 定）：两张卡各 Grow(1)，绘图区等高。
	//
	// 上一版是「主图 grow + 副图定高 156」：截图实测绘图区 228 vs 154，
	// 双序列（柱 + 折线）的图信息更多却矮 74px，而上面单序列图多出来的
	// 高度只是柱子上方的空白。两张图各自回答一个问题，没有哪张该独占
	// 剩余空间；等高之后两张图的柱底、刻度在同一屏上直接对齐。代价是
	// 窗口拉高时两张一起变高、主图不再独占增量——这是有意的取舍：本面板
	// 不设「主图」，两图是并列的两个问题（省了多少 / 现在顺不顺）。
	a.chartCard(c, p)
	a.trafficCard(c, p)
}

// failureLine 启动失败那一句（仅失败时出现）。
//
// 状态行的另一半职责就是它：开关只能表达「没跑起来」，说不出为什么。
// 这一行把它补上，并且在开关旁边就能看到——用户点完开关，视线还在右上角，
// 这句出现在正文第一行，不会被当成装饰。
func (a *app) failureLine(c *ui.Context, p *glass.Palette) {
	if a.running() || a.lastErr() == nil {
		return
	}
	glass.Surface(ui.Row(c).Padding(8, 12).AlignItems(ui.Center).Gap(8),
		p, glass.Panel()).Children(func() {
		ui.Box(c).Size(8, 8).Radius(4).Shrink(0).Background(p.Danger).
			Shadow(0, 0, 0, 1, ui.RGBA(0, 0, 0, 0.35))
		ui.Text(c, "启动失败").FontSize(12).FontWeight(600).
			TextColor(p.Danger).SingleLine().Shrink(0)
		ui.Text(c, humanError(a.lastErr())).FontSize(11).
			TextColor(p.MutedForeground).MaxLines(1).Grow(1)
	})
}

// liveBadge 「实时 / 已暂停」胶囊（内嵌 4px 圆点）。
//
// 圆点不带同色发光：solid-glass 的规矩是「光影在边缘」，雾状光晕会把 8px 的
// 状态点晕成 20px 的一团（上一轮真实截图里，错误态的红晕半径约 20px）。
// 要强调就给一圈极淡的外描边。
func (a *app) liveBadge(c *ui.Context, p *glass.Palette, running bool) {
	// 圆点用 p.DownBar 而不是 p.Success：同一条标题行里已经有数据绿
	// （图例「省下」），两种相近的绿（#54bf5c vs #4ec38a）并排会被读成
	// 「同一色的两种深浅」；「实时」表达的就是「数据在流」，用系列绿更准。
	dot, text := p.DownBar, "实时"
	if !running {
		dot, text = p.Tertiary, "已暂停"
	}
	ui.Row(c).Padding(2.5, 7, 2.5, 5).Gap(5).AlignItems(ui.Center).
		Background(ui.RGBA(255, 255, 255, 0.045)).
		InsetShadow(0, 0, 0, 1, ui.RGBA(255, 255, 255, 0.05)).
		Radius(999).Shrink(0).
		Children(func() {
			// 6×6：与图例色块同一尺寸（4px 时它在 11px 的文字旁显得像颗尘点）。
			ui.Box(c).Size(6, 6).Radius(3).Background(dot).Shrink(0)
			ui.Text(c, text).FontSize(11).LetterSpacing(0.2).
				TextColor(p.Tertiary).SingleLine()
		})
}

// chartCard 上行体积对比图。这是这一屏的主角。
//
// ## 纵轴为什么是「省下的比例」而不是体积
//
// 真实数据算过两种轴，都不行：
//
//	线性轴   12.5 MB → 139 KB（省 98.9%）时，实发段只有柱高的 1.1%，
//	         一根 200px 的柱子亮 2px —— 产品最强的场景在图上等于零。
//	对数轴   把「省 99.9%」和「省 70%」压到视觉上差不多，
//	         反而看不出比例差异。
//
// 用户要看的是「提升了多少」，不是「数据有多大」。所以柱高直接就是
// **压缩率**：满高 = 省 100%，零高 = 一点没省。绝对体积交给图下方那行文字。
// 这样「今天压得好不好」一眼可读，不需要理解任何刻度换算。
//
// ## 单列分层
//
//	每个时间桶一根柱子，柱内两段：
//	  亮段（p.Foreground）= 省下的部分
//	  暗段（p.Tertiary）  = 实际发出去的部分，贴底
//
// 暗段按「占原始体积的比例」切，且至少有 minWirePx 高——压缩 99.9% 时它本来
// 只有 0.2px，那会让整根柱子读成「纯亮色」，反而弱化了「还有一点点发出去」
// 这个事实。
func (a *app) chartCard(c *ui.Context, p *glass.Palette) {
	g := a.agg
	running := a.running()
	hasData := a.hasSeries()

	// 卡片永远吃掉剩余高度。
	//
	// 空态也吃（2026-11-13 第二轮）：状态行搬去顶栏之后，面板上空出了那 38px，
	// 若空卡仍按内容高度收着，下面就是 300px 纯空——那不是「留白」，是「没画完」。
	// 让卡片撑满，空态那句引导在卡内垂直居中，读作「一张空的图，等待数据」；
	// 这才是空态该有的形状（shadcn 的 Empty 也是占满容器再居中）。
	glass.Surface(ui.Column(c).Padding(12, 14, 12).Gap(8).Grow(1), p, glass.Card).Children(func() {
		// 头部：标题 + 图例 + live 徽标。
		// 图例只在有柱子时出现——空态下「■省下 ■实发」在解释一个不存在的东西。
		ui.Row(c).AlignItems(ui.Center).Gap(10).Children(func() {
			glass.Heading(c, p, "上行体积对比")
			ui.Box(c).Grow(1)
			if hasData {
				a.legend(c, p)
			}
			a.liveBadge(c, p, running)
		})

		if !hasData {
			// 空态不画坐标系：一副空坐标轴会被读成「图表坏了」，
			// 而「启动代理后…」本身就是准确的说明。
			//
			// 引导放在绘图区（而不是卡片正中）的高度一半处：标题在上、引导居中、
			// 下面留给图的位置——将来出现第一根柱子时，那句引导消失的位置正好是
			// 柱子长出来的地方，视线不会跳。
			ui.Box(c).Grow(1).MinHeight(52).FillWidth().
				AlignItems(ui.Center).Justify(ui.Center).Children(func() {
				ui.Text(c, a.emptyChartNote()).FontSize(11.5).
					TextColor(p.MutedForeground).SingleLine()
			})
			return
		}

		a.chartPlot(c, p)
		ui.Text(c, a.chartSummary(g)).FontSize(10.5).
			FontFeatures("tnum").TextColor(p.MutedForeground).SingleLine()
	})
}

// chartMinHeight 绘图区的最小高度（DIPs）。
//
// 图上柱高表达压缩率，太矮时 5% 的差异只剩 2-3px、读不出来。110 是
// 「25% 刻度间隔 27px」的下限：再矮，四档刻度就会与柱顶互相挤压。
const chartMinHeight float32 = 110

// chartAxisWidth / chartAxisGap 两张图共用的左侧刻度列与间距。
//
// 两张图上下叠在同一张卡片列里，绘图区必须共用同一组左右 gutter，否则
// 横轴的刻度、柱子的落点会错位（上一版一张 28、一张 46，x 轴差 18px，
// 叠在一起一眼就能看出没对齐）。46 是两张图里较宽的那个刻度文案
// （"411.1 KB"）量出来的宽度，两处统一取它。
const (
	chartAxisWidth float32 = 46
	chartAxisGap   float32 = 6
)

// ---- 绘图区的纵向比例尺（两张图共用同一个换算入口）----

// plotTopInset / plotBottomInset 绘图区上下留给刻度文字的内缩（DIPs）。
//
// 刻度标签以刻度值为中心，最上/最下两个标签各有约 3px 伸到刻度值之外；
// 8/6 的内缩让它们既不越出绘图区（不被裁），也不贴到卡片边缘。
//
// 顶部比底部多 2：绘图区顶边紧挨着图例（卡片里只有一个 Gap(8)），
// 标签顶部再往上就没有呼吸了（实测「12.4 MB」的字顶距卡片内容区仅 1px）。
const (
	plotTopInset    float32 = 8
	plotBottomInset float32 = 6
)

// plotTopHeadroom 比例尺顶端再往下让出的留白（DIPs）。
//
// 只有比例尺（柱顶、折线点、100% 虚线）吃这个内缩，刻度文字不吃：峰值柱顶
// 与最上刻度中心因此留出 3px。两者齐平时（上一版实测差 0.5px）1x 下读作
// 「柱顶顶到了坐标系上沿」，而柱子必须先于刻度收住，图才有上边框。
const plotTopHeadroom float32 = 3

// plotScale 绘图区的纵向比例尺：把 [0,1] 的比例换成 y。
//
// 唯一的换算入口。柱顶、柱底、折线点、参考虚线、左侧刻度标签**全部**走
// y()，同一个比例永远落在同一个像素上。上一轮的缺陷正是两套换算并存：
// 柱高按「96% 可用高度」算、100% 虚线按整高算，于是 100% 被画在 94% 的
// 位置（实测低 15px），虚线从每根柱顶下方横穿而过。
type plotScale struct {
	top  float32 // frac=1 的 y（轴顶）
	base float32 // frac=0 的 y（基线）
}

// newPlotScale 由绘图区矩形算比例尺。r 必须是绘图区（已扣掉刻度列），
// 不是整行：否则刻度的 x 与柱子的 x 会错开一个 gutter。
func newPlotScale(r ui.Rect) plotScale {
	top := r.Y + plotTopInset + plotTopHeadroom
	base := r.Y + r.H - 1 - plotBottomInset
	if base < top {
		base = top
	}
	return plotScale{top: top, base: base}
}

// y 比例尺：frac=0 → 基线，frac=1 → 轴顶。
func (s plotScale) y(frac float32) float32 {
	f := float32(clampF(float64(frac), 0, 1))
	return s.base - f*(s.base-s.top)
}

// height 一段比例（0..1）对应的高度（像素），供柱高使用。
func (s plotScale) height(frac float32) float32 { return s.y(0) - s.y(frac) }

// plotArea 绘图区：扣掉左侧刻度列的 gutter。
//
// 两张图共用同一组 gutter（chartAxisWidth + chartAxisGap），柱子的 x 落点
// 与刻度因此跨图对齐。
func plotArea(r ui.Rect) ui.Rect {
	gutter := chartAxisWidth + chartAxisGap
	if r.W <= gutter {
		return ui.Rect{}
	}
	return ui.Rect{X: r.X + gutter, Y: r.Y, W: r.W - gutter, H: r.H}
}

// axisTicks 纵轴刻度列。
//
// 位置由 plotScale 反算，不用 Grow 均匀分布：Grow 只能把 N 个标签摊开，
// 而刻度必须落在**数据**的 y 上。上一版靠 Grow（标签在行内居中），最上
// 刻度比最高柱顶低 4px，读起来像柱子顶出了坐标系。这里把标签绝对定位在
// 纵向盒里（Top/Bottom 内缩 = plotTopInset/plotBottomInset，见 plotScale），
// 每个标签以刻度值为中心；盒顶比比例尺顶端高 plotTopHeadroom，于是峰值柱顶
// 总在最上刻度中心之下 3px（柱顶不顶格）。
func (a *app) axisTicks(c *ui.Context, p *glass.Palette, size float32, labels ...string) {
	if len(labels) < 2 {
		return
	}
	ui.Box(c).Absolute().Left(0).Width(chartAxisWidth).
		Top(plotTopInset).Bottom(plotBottomInset).Children(func() {
		for i, label := range labels {
			frac := 1 - float32(i)/float32(len(labels)-1)
			ui.Box(c).Absolute().Left(0).Right(0).
				TopPercent((1-frac)*100).TranslatePercent(0, -50).
				AlignItems(ui.End).Children(func() {
				ui.Text(c, label).FontSize(size).FontFeatures("tnum").
					TextColor(p.Tertiary).SingleLine()
			})
		}
	})
}

// trafficCard 流量与延迟图（第二张）。
//
// ── 为什么需要第二张图 ────────────────────────────────────────────────────
// 上面那张图只回答「省了多少」——它是本产品的卖点，但不是全部事实。用户还要
// 知道「现在跑得顺不顺」：每秒上下行多少字节、延迟是不是在恶化。这两件事在
// 压缩率图上完全看不见（压缩率 99.9% 时连接可能同时卡得要死）。
//
// ── 纵轴设计（核心，和上一张图不同）────────────────────────────────────────
// 上一张图的纵轴是百分比，0-100% 天然有界。流量是无界的：一次 30MB 的请求
// 能把 1KB 的请求压成 0.003%。所以：
//   - 柱：上下行流量，**对同一个桶取同一个比例尺**（两条量共用纵轴基准），
//     否则「上行比下行小」这种判断会失真
//   - 线：延迟（毫秒），单独按本窗口的峰值归一，画在同样的绘图区里
//   - 两者的比例尺各自独立，所以图例必须写清楚「柱=字节，线=毫秒」
//
// ── 为什么不画握手延迟 ────────────────────────────────────────────────────
// 连接复用生效后，绝大多数桶的握手次数是 0（那正是复用的证据）。把它画成柱会
// 得到一片空白，读者会以为「数据丢了」。改为在摘要行里报「本窗口握手 N 次」：
// 0 次读作「复用正常」，非 0 时才值得看具体值。
func (a *app) trafficCard(c *ui.Context, p *glass.Palette) {
	g := a.agg
	running := a.running()
	hasData := a.hasTraffic()

	glass.Surface(ui.Column(c).Padding(12, 14, 12).Gap(8).Grow(1), p, glass.Card).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Gap(10).Children(func() {
			glass.Heading(c, p, "流量与延迟")
			ui.Box(c).Grow(1)
			if hasData {
				a.legendSwatch(c, p.UpBar, "上行")
				a.legendSwatch(c, p.DownBar, "下行")
				a.legendLine(c, p.Latency, "延迟")
				// 折线在空桶处断开，不是渲染失败。有这种断点时在图例里
				// 明说一句，否则读者会把「无样本」读成「线没画出来」。
				if a.latencyHasGaps() {
					ui.Text(c, "断点=无样本").FontSize(10).
						TextColor(p.Tertiary).SingleLine()
				}
			}
			a.liveBadge(c, p, running)
		})

		if !hasData {
			ui.Box(c).Grow(1).MinHeight(52).FillWidth().
				AlignItems(ui.Center).Justify(ui.Center).Children(func() {
				ui.Text(c, "等待流量数据").FontSize(11.5).
					TextColor(p.MutedForeground).SingleLine()
			})
			return
		}

		a.trafficPlot(c, p)
		ui.Text(c, a.trafficSummary(g)).FontSize(10.5).
			FontFeatures("tnum").TextColor(p.MutedForeground).SingleLine()
	})
}

// hasTraffic 是否有可画的流量数据（上下行任一非零即可）
func (a *app) hasTraffic() bool {
	for _, b := range a.agg.Series {
		if b.UpWire > 0 || b.DownWire > 0 {
			return true
		}
	}
	return false
}

// trafficPlot 画流量柱 + 延迟折线。
//
// 四条渲染契约（全部来自像素复核）：
//
//  1. 折线用 StrokePolyline（真斜线 + 圆头圆角），不是 Painter.Line——
//     Line 只画水平/垂直，斜段会退化成一条短横线，整条曲线看起来像坏点。
//  2. 折线顶点的 x 与柱心**是同一个表达式**（bucketCenter）。上一版折线落在
//     桶边界（柱缝中点）上，整体左移了半个槽位（实测 -8px），顶点压在柱缝上，
//     读数会归到错误的桶。重叠是可接受的——折线画在柱子之上，并且靠第 4 条
//     的描边把「叠加」这件事画清楚。
//  3. 只有「有延迟样本」的桶（Count>0 且 MsSum>0）才有点，其余断开。上一版
//     只判 Count>0，于是「有请求但没采到隧道往返」的桶按 AvgMs()=0 画在基线上，
//     在离柱群 160px 的空白处留下一条 17×2px 的橙线——1x 下就是一道划痕。
//  4. 折线先画一层深色描边（线宽 +2px）再画琥珀线：2px 的线直接斜穿蓝色柱体
//     时，柱身被切成上下两段，读作「渲染坏了」；1px 的深色环让线读作「浮在柱子
//     上的一条线」，这是「有意叠加」而不是划痕。选它而不是降低柱体透明度：
//     柱高/柱色本身承载量级读数，压透明度会让相邻桶的对比也一起变淡。
//
// 单点（孤立的非空桶）画圆点而不是横短划：顶点落在柱心之后，一根 3.8px 的
// 横短划会与柱顶的边混在一起；圆点（线宽 +2 的直径）在 1x 下读作「一个样本」。
//
// 量纲不混轴：左轴只标字节（顶=峰值、中=峰值/2、底=0），延迟的数值
// （当前/峰值/均值）由图下那行摘要给出。上一版在同一根左轴上同时标
// "2.2 MB" 与 "101603 ms" 且没有刻度，读者无法判断折线的高度代表什么。
// 柱与线的纵向位置都走 plotScale（峰值柱顶 / 峰值延迟点 = 比例尺顶端，
// 也就是最上刻度中心往下 plotTopHeadroom 的那条线）。
func (a *app) trafficPlot(c *ui.Context, p *glass.Palette) {
	series := a.agg.Series

	// 比例尺：柱的基准取「本窗口最大单桶字节」，线的基准取「本窗口最大平均延迟」。
	// 两者各算各的，但都至少取 1，避免除零。两条比例尺共用同一个纵向盒
	// （见 plotScale）：峰值柱的柱顶与峰值延迟点都精确落在轴顶刻度上。
	var maxBytes int64 = 1
	var maxMs int64 = 1
	for _, b := range series {
		if v := b.UpWire + b.DownWire; v > maxBytes {
			maxBytes = v
		}
		if !hasLatencySample(b) {
			continue
		}
		if v := b.AvgMs(); v > maxMs {
			maxMs = v
		}
	}

	ui.Box(c).Grow(1).MinHeight(chartMinHeight).FillWidth().
		Draw(func(pt *ui.Painter, r ui.Rect) {
			area := plotArea(r)
			if area.W <= 0 || area.H <= 0 {
				return
			}
			s := newPlotScale(area)

			// 基线
			pt.Fill(ui.Rect{X: area.X, Y: s.base, W: area.W, H: 1}, p.Edge, 0)

			n := len(series)
			if n == 0 {
				return
			}
			// 柱宽：留 22% 间隙，但不小于 1.5px（48 根柱子时仍要看得见）
			slot := area.W / float32(n)
			gap := slot * 0.22
			bw := max(slot-gap, 1.5)
			// bucketCenter 第 i 个桶的中心 x。柱与折线点共用它——这是
			// 「折线顶点 x == 柱心 x」在代码层面的唯一保证（见函数头第 2 条）。
			bucketCenter := func(i int) float32 { return area.X + (float32(i)+0.5)*slot }

			// 先画完所有柱子，再画折线：折线必须在柱子之上，否则后画的
			// 柱子会盖住它经过的那一段，看起来又像断线。
			for i, b := range series {
				total := b.UpWire + b.DownWire
				if total <= 0 {
					continue
				}
				x := bucketCenter(i) - bw/2

				// 上行与下行**堆叠**而不是并排：「这一桶总共走了多少」是
				// 更常见的读数，并排会让柱宽减半、48 根时几乎看不清。
				h := trafficBarHeight(total, maxBytes, s.base-s.top)
				upH := h * float32(b.UpWire) / float32(total)
				downH := h - upH
				if upH > 0 {
					pt.Fill(ui.Rect{X: x, Y: s.base - upH, W: bw, H: upH},
						p.UpBar, 0.8)
				}
				if downH > 0 {
					pt.Fill(ui.Rect{X: x, Y: s.base - upH - downH, W: bw, H: downH},
						p.DownBar, 0.8)
				}
			}

			// 延迟折线：连续有样本的桶连成一段；无样本（Count==0 或
			// MsSum==0）处断开——空转期没有延迟样本，连过去会画出
			// 一条「延迟平稳」的假象。
			var runs [][][2]float32
			var run [][2]float32
			flush := func() {
				if len(run) > 0 {
					runs = append(runs, run)
					run = nil
				}
			}
			for i, b := range series {
				if !hasLatencySample(b) {
					flush()
					continue
				}
				frac := float32(clampF(float64(b.AvgMs())/float64(maxMs), 0, 1))
				run = append(run, [2]float32{bucketCenter(i), s.y(frac)})
			}
			flush()

			// 两遍画：先所有描边、再所有线。逐段「描边+线」会在端点处
			// 让后一段的描边压住前一段的线头。
			for _, run := range runs {
				strokeLatency(pt, run, latencyHaloWidth, p.Shade)
			}
			for _, run := range runs {
				strokeLatency(pt, run, latencyLineWidth, p.Latency)
			}
		}).Children(func() {
		// 字节刻度三档：顶=本窗口峰值、中=峰值的一半、底=0。
		// 延迟不在同一根轴上（见函数头注释），它的数值在摘要行里。
		a.axisTicks(c, p, 8, humanBytes(maxBytes), humanBytes(maxBytes/2), "0")
	})
}

// latencyHaloWidth 折线描边的线宽（DIPs）：比琥珀线宽 2px，左右各 1px。
const latencyHaloWidth = latencyLineWidth + 2

// strokeLatency 画一段延迟折线（含描边时的同一个几何）。
//
// 单点画圆点：StrokePolyline 对单点什么都不画，而「一根横短划」在柱心位置
// 会与柱顶的边糊在一起（见函数头注释）。
func strokeLatency(pt *ui.Painter, run [][2]float32, width float32, col ui.Color) {
	if len(run) == 1 {
		d := width + 2
		pt.Fill(ui.Rect{X: run[0][0] - d/2, Y: run[0][1] - d/2, W: d, H: d}, col, d/2)
		return
	}
	pt.StrokePolyline(run, width, col)
}

// hasLatencySample 桶里是否有可画的延迟样本。
//
// Count>0 但 MsSum==0 的桶是「有请求、没采到隧道往返」（错误请求、本地命中
// 等）：AvgMs() 会返回 0，把它当样本画出来就是在基线上凭空多一个点。
func hasLatencySample(b stats.Bucket) bool {
	return b.Count > 0 && b.MsSum > 0
}

// latencyLineWidth 延迟折线的线宽（DIPs）。
//
// 2.0 而不是 1.4：1.4 的线在玻璃卡的低对比底色上会被抗锯齿冲淡成一条
// 若隐若现的虚影；2 加上圆头，稀疏点也读得出来。
const latencyLineWidth float32 = 2

// minTrafficBarPx 非零流量桶的最小总高度。
//
// 只对「有实质流量」的桶生效（见 minTrafficBarFrac）：真实数据的单桶流量是
// 重尾分布，峰值旁边常跟着 1% 量级的桶，按比例只剩 1-2px，抬到 3px 才读得
// 出「这里有过流量」。低于 1% 的桶按真实比例画（可能不足 1px），否则空置的
// 右半幅会被一排 3px 绿点填满。
const minTrafficBarPx float32 = 3

// minTrafficBarFrac 「有实质流量」的下限：占本窗口单桶峰值的 1%。
//
// 1% 的依据：峰值 3.9 MB 时它就是 39 KB，已是「一次正常请求」的量级；
// 更小的桶（0.08%）在任何柱宽下都读不出量级，只会在基线上留下碎屑。
const minTrafficBarFrac float32 = 0.01

// trafficBarHeight 一个桶的柱高（像素）。
//
// 峰值桶精确等于 plotH（轴顶刻度），其余按比例；最小可见高度只给
// ≥ 峰值 1% 的桶——上一版无条件把非零桶抬到 3px，右半幅空置时一排 0.08%
// 的桶变成 3px 绿点，读起来像基线掉渣。低于 1% 的桶按真实比例画（0-1px）。
func trafficBarHeight(total, maxBytes int64, plotH float32) float32 {
	if total <= 0 || maxBytes <= 0 || plotH <= 0 {
		return 0
	}
	frac := float32(total) / float32(maxBytes)
	h := frac * plotH
	if frac >= minTrafficBarFrac && h < minTrafficBarPx {
		h = minTrafficBarPx
	}
	return h
}

// trafficSummary 流量图下面那行摘要。
//
// 必须是一行（渲染处 SingleLine）。上一版把「握手 N 次 · 均 X ms」放在末尾，
// 560 宽时最后一个单位词被折到第二行、独占一个字——读起来像排版事故。
// 现在的取舍：去掉「首字节」与「握手均值」（它们都可以从别处推出，且不是
// 这张图要回答的问题），换成延迟的三档读数「当前 / 峰值 / 均值」——
// 折线本身没有独立右轴，这三个数就是它的量纲。
func (a *app) trafficSummary(g stats.Agg) string {
	var up, down, n, msSum, hsN int64
	var latest, peak int64
	for _, b := range g.Series {
		up += b.UpWire
		down += b.DownWire
		if b.Count <= 0 {
			continue
		}
		n += b.Count
		msSum += b.MsSum
		hsN += b.HsCount
		// 「当前」也走 hasLatencySample：没有延迟样本的桶（MsSum==0）不该
		// 把读数改写成 0 ms——它和图上「这里断线」是同一件事的两个说法。
		if !hasLatencySample(b) {
			continue
		}
		if ms := b.AvgMs(); ms > peak {
			peak = ms
		}
		latest = b.AvgMs()
	}
	if n == 0 {
		return "这段时间没有请求"
	}

	span := int(g.WindowSec)
	if span <= 0 {
		span = int(stats.DefaultWindow / time.Second)
	}

	// 上行/下行用箭头：图例就在上方（上行=蓝柱、下行=绿柱），这里再用四个
	// 汉字会把「延迟 当前/峰值/均值」这组更重要的读数挤出单行。
	// 三个延迟读数共用同一个单位（见 humanMsGroup）：同一句里混 ms/s 时
	// 三个数没法直接比大小。
	ms := humanMsGroup(latest, peak, msSum/n)
	text := fmt.Sprintf("最近 %s · ↑%s · ↓%s · 延迟 当前 %s / 峰值 %s / 均值 %s",
		humanWindow(span), humanBytes(up), humanBytes(down),
		ms[0], ms[1], ms[2])
	// 握手：0 次是好事（复用生效），所以措辞要中性偏正面，而不是「无数据」。
	// 均值不写：一个数已经够回答「有没有在反复握手」，具体耗时不是这行的问题。
	if hsN == 0 {
		text += " · 全程复用连接"
	} else {
		text += fmt.Sprintf(" · 握手 %s 次", groupDigits(hsN))
	}
	return text
}

// latencyHasGaps 非空延迟样本之间是否存在空洞（折线因此断开）。
//
// 判据与画线完全一致（hasLatencySample）：只在「两个有样本的桶之间夹着无样本
// 的桶」时算断点。开头/结尾的空洞不会让折线断在中间，不构成视觉上的断点。
// 两处不能各写一套条件——否则图例会为一条看不见的断点做说明，或者对一条
// 真的断掉的线沉默。
func (a *app) latencyHasGaps() bool {
	seen, gap := false, false
	for _, b := range a.agg.Series {
		if hasLatencySample(b) {
			if gap {
				return true
			}
			seen = true
			continue
		}
		if seen {
			gap = true
		}
	}
	return false
}

// 图表配色：直接引用色板里的系列色，不在视图里另立一套十六进制值。
//
// 语义分工（两张图共用同一组常量）：
//
//	p.DownBar 绿 = 产出/赚到：压缩图里的「省下」、流量图里的「下行」
//	p.UpBar   蓝 = 成本/花掉：压缩图里的「实际发送」、流量图里的「上行」
//
// 上一版这里硬编码了 #4ADE80/#3B82F6，而色板是 #4ec38a/#4c9ef0——同一屏
// 出现两种「同色系」，叠在一起像没对齐的两种主题。系列色本来就该只有一处定义。

// minWirePx 蓝段（实际发送）的最小可见高度。

// 压缩 99.9% 时它按比例只有 0.2px，抗锯齿后会完全消失，整根柱子读成纯亮色——
// 那会丢失「仍然发了东西」这个事实。3px 是 Retina 上能稳定看见一条线的高度。
const minWirePx float32 = 3

// hasSeries 是否有可画的数据（有一秒带流量即可）
func (a *app) hasSeries() bool {
	for _, b := range a.agg.Series {
		if b.UpRaw > 0 {
			return true
		}
	}
	return false
}

// emptyChartNote 空态在图上说什么。
//
// 三种情况必须分开说，因为它们对用户的下一步完全不同：
//
//	没启动              → 启动代理
//	启动了、历史也没有   → 去用 Agent（这才是第一次）
//	启动了、历史有数据   → 这段时间没流量（不是统计丢了）
//
// 第三种是 2026-11-13 加小窗口（1 分钟）后暴露的：统计从磁盘恢复，历史里有
// 上千条请求，但「最近 1 分钟」为空完全正常。此时说「等待第一个请求」会让
// 用户以为历史数据没了。
func (a *app) emptyChartNote() string {
	if !a.running() {
		return "启动代理后，这里显示上行体积与压缩效果"
	}
	if a.agg.Requests > 0 {
		return fmt.Sprintf("最近 %s没有请求（调大显示区间可看更早的数据）",
			humanWindow(int(a.agg.WindowSec)))
	}
	return "等待第一个请求经过本地代理"
}

// legend 图例。单列分层必须说明哪段是什么，否则两根不同颜色的段无从解释。
//
// 用两块真的方块（而不是 ■/▨ 字符）：字体缺字形时会变成豆腐块，
// 而方块是画出来的，任何字体下都在。
func (a *app) legend(c *ui.Context, p *glass.Palette) {
	ui.Row(c).Gap(9).AlignItems(ui.Center).Shrink(0).Children(func() {
		a.legendSwatch(c, p.DownBar, "省下")
		a.legendSwatch(c, p.UpBar, "实际发送")
	})
}

func (a *app) legendSwatch(c *ui.Context, col ui.Color, label string) {
	ui.Row(c).Gap(4).AlignItems(ui.Center).Shrink(0).Children(func() {
		// 6×6：与柱子的最小可辨宽度同量级（数据柱净宽 6.6-11px、圆角 2），
		// 图例 key 与数据同形，读图时不需要再换算一次。
		ui.Box(c).Size(6, 6).Radius(1.5).Shrink(0).Background(col)
		ui.Text(c, label).FontSize(10).TextColor(a.pal.Tertiary).SingleLine()
	})
}

// legendLine 折线类图例：画一小段横线而不是色块。
//
// 形状必须与柱状图例区分：同一张图里既有柱又有线时，两种图例都画成方块会让
// 人以为有三组柱子。线宽与数据折线共用 latencyLineWidth（不是另取一个 1.4），
// 否则图例上的一条细线会比图里的折线更细，读作两种不同的东西。
func (a *app) legendLine(c *ui.Context, col ui.Color, label string) {
	ui.Row(c).Gap(4).AlignItems(ui.Center).Shrink(0).Children(func() {
		ui.Box(c).Size(11, 7).Shrink(0).Draw(func(pt *ui.Painter, r ui.Rect) {
			pt.StrokePolyline([][2]float32{
				{r.X + latencyLineWidth/2, r.Y + r.H/2},
				{r.X + r.W - latencyLineWidth/2, r.Y + r.H/2},
			}, latencyLineWidth, col)
		})
		ui.Text(c, label).FontSize(10).TextColor(a.pal.Tertiary).SingleLine()
	})
}

// chartPlot 柱状图本体：纵轴刻度 + 100% 参考虚线 + 每分钟一根分层柱。
//
// 每根柱子的画法（自底向上）：
//
//	barH   = max(省下的比例 × 满量程, minWirePx)   ← 柱子总高 = 压缩率
//	wireH  = min(minWirePx, barH)                 （蓝，贴底）
//	savedH = barH - wireH                         （绿，在上）
//
// 满量程与 100% 虚线、左侧刻度全部来自 plotScale 的同一个换算（见 plotScale
// 的注释）：100% 的柱顶与虚线在像素上重合，不会再出现「虚线从柱顶下方穿过」。
//
// 为什么蓝段是「固定最小高度」而不是「按比例」：这是「压缩率当柱高」这个
// 映射本身带来的约束。压缩 99.9% 时按比例算蓝段只有 0.1px，抗锯齿后消失，
// 整根柱子读成纯绿——用户就看不出「还是发了点东西」。3px 是 Retina 上能稳定
// 看见一条线的高度，表达的是「这里确实有发送」，而不是一个量化读数
// （量化读数在图下方那行文字里，是精确的）。
//
// 语义分工：
//
//	柱高  = 这一分钟省了多少（成绩）
//	蓝段  = 确实发了（存在性，不是数值）
//	绿段  = 剩下的都是省下的
func (a *app) chartPlot(c *ui.Context, p *glass.Palette) {
	series := a.agg.Series
	// 绘图区吃掉卡片里的剩余高度，但有下限：窗口被拉到最小高度时
	// 图仍有 chartMinHeight，不至于压成一条线。
	ui.Box(c).Grow(1).MinHeight(chartMinHeight).FillWidth().
		Draw(func(pt *ui.Painter, r ui.Rect) {
			area := plotArea(r)
			if area.W <= 0 || area.H <= 0 {
				return
			}
			s := newPlotScale(area)

			// 基线（0%）：1px 发丝线，不给网格
			pt.Fill(ui.Rect{X: area.X, Y: s.base, W: area.W, H: 1}, p.Edge, 0)

			// 100% 参考虚线：y 取 s.y(1)，与一根「省满 100%」的柱顶是
			// **同一个表达式**。上一版柱高按 96% 的可用高度算、虚线按整高算，
			// 100% 因此被画在 94% 的位置（截图实测低 15px），1x 下就是虚线
			// 从每根柱顶下方横穿而过。画实线会与柱顶混在一起，3px/3px 的
			// 节奏足够让人看出「这是参考，不是数据」。
			dy := s.y(1)
			for x := float32(0); x < area.W; x += 6 {
				w := min(float32(3), area.W-x)
				pt.Fill(ui.Rect{X: area.X + x, Y: dy, W: w, H: 1}, p.Border, 0)
			}

			n := len(series)
			if n == 0 {
				return
			}
			// 柱间留 3px：30 根柱子在 380px 里每根约 9.6px，留 3px 后
			// 净宽 6.6px —— 足够看出「一根一根」，也容得下 2px 圆角。
			const gap float32 = 3
			slot := area.W / float32(n)
			bw := max(slot-gap, 1)

			for i, b := range series {
				if b.UpRaw <= 0 {
					continue // 空分钟不画：基线上的空白就是「这段时间没流量」
				}
				saved := float32(clampF(1-float64(b.UpWire)/float64(b.UpRaw), 0, 1))

				// 柱高 = 压缩率 × 比例尺的满量程。saved=1 时柱顶 = s.top，
				// 正好落在上面那条 100% 虚线上（像素差 < 1px）。
				barH := s.height(saved)
				// 一点没省（saved=0）时柱子高度为 0，只剩一条蓝线：
				// 那是准确的——「这一分钟完全没有优化」。
				wireH := min(minWirePx, barH)
				if barH < wireH {
					barH = wireH
				}
				savedH := barH - wireH

				x := area.X + float32(i)*slot + (slot-bw)/2
				top := s.base - barH

				// 绿段（省下）：柱子的主体，从上沿往下
				if savedH > 0 {
					pt.Fill(ui.Rect{X: x, Y: top, W: bw, H: savedH},
						p.DownBar, min(2, bw/2))
				}
				// 蓝段（实际发送）：贴底
				pt.Fill(ui.Rect{X: x, Y: s.base - wireH, W: bw, H: wireH},
					p.UpBar, min(2, bw/2))
			}
		}).Children(func() {
		// 纵轴刻度：0/25/50/75/100%。五档是因为柱高直接读作百分数，
		// 少一档就得靠目测插值。列宽与流量图共用 chartAxisWidth，
		// 两张图的绘图区因此左右对齐（同一屏叠放时 x 轴不错位）。
		a.axisTicks(c, p, 8.5, "100%", "75%", "50%", "25%", "0")
	})
}

// chartSummary 图下方那行文字：图给比例，文字给绝对值，各司其职。
//
// 窗口与粒度都从快照里取（WindowSec / BucketSec），不写死：用户切了显示区间，
// 这句话必须跟着说「最近 6 小时（每 12 分钟一柱）」，否则文字和图说的就不是
// 同一段时间——那比没有这句话更糟。
func (a *app) chartSummary(g stats.Agg) string {
	var raw, wire int64
	var n int64
	for _, b := range g.Series {
		raw += b.UpRaw
		wire += b.UpWire
		n += b.Count
	}
	if raw <= 0 {
		return a.emptyChartNote()
	}
	saved := 1 - float64(wire)/float64(raw)
	span := int(g.WindowSec)
	if span <= 0 {
		span = int(stats.DefaultWindow / time.Second)
	}
	text := fmt.Sprintf("最近 %s：%s → %s · 省下 %.1f%%（%s 次请求）",
		humanWindow(span), humanBytes(raw), humanBytes(wire), saved*100, groupDigits(n))
	if g.BucketSec > 0 {
		// 桶粒度单独一句、允许折到第二行：480px 宽时它和前三段挤不进一行，
		// 被省略号截掉的会是「每 N 一柱」这个最有用的信息。
		text += " · 每 " + humanWindow(int(g.BucketSec)) + "一柱"
	}
	return text
}

// humanWindow 把秒数说成人话（1800 → "30 分钟"）。
//
// 为什么不直接写「1800 秒」：图上每根柱子是一个时间桶，「30 分钟」让读者能
// 把「30 根柱子」和「30 分钟」对起来；「1800 秒」需要先做一次除法。
//
// 天这一档只在**超过** 24 小时时启用：24 小时是用户心里的「一天」也是
// 「这个窗口」，保留 24 小时档写作「24 小时」比「1 天」更贴合设置页的语义。
func humanWindow(seconds int) string {
	switch {
	case seconds <= 0:
		return "0 秒"
	case seconds > 24*3600 && seconds%(24*3600) == 0:
		return fmt.Sprintf("%d 天", seconds/(24*3600))
	case seconds%3600 == 0:
		return fmt.Sprintf("%d 小时", seconds/3600)
	case seconds%60 == 0:
		return fmt.Sprintf("%d 分钟", seconds/60)
	default:
		return fmt.Sprintf("%d 秒", seconds)
	}
}

// clampF 把 v 夹到 [lo, hi]
func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// heroNote 图中无数据时的一句话（也用于托盘与旧调用点）。
//
// 空态说「怎么开始」，运行态说「每 1,000 字节实际发多少」——空态要有下一步
// 动作，而不是一句「无数据」。
func (a *app) heroNote(g stats.Agg, running bool) string {
	if !running {
		return "启动代理后开始统计上行体积"
	}
	if g.UpRaw == 0 {
		return "等待第一个请求经过本地代理"
	}
	// 几乎没压缩时说清事实，而不是把 "节省 0.0%" 摆出来当成绩：
	// 小请求低于 delta 门槛是正常设计（省不了还白付隧道开销）。
	if g.UpRatio > 0.98 {
		return "本次流量未压缩（小于 delta 门槛的请求直接透传）"
	}
	return fmt.Sprintf("节省 %.1f%% · 每 1KB 只发 %dB",
		(1-g.UpRatio)*100, rawBytesPerKilo(g.UpRatio))
}

// actionButton 一个套了 solid-glass 边光的按钮。
//
// 用 ui.Button / ui.PrimaryButton 而不是 ButtonBase + 手画文字：前者负责文字
// 测量、内边距与 hover/pressed 三态背景（faces 机制），自己重做只会把布局弄坏
// （实测 ButtonBase 手画会让整行按内容宽度撑开、文字被裁）。玻璃感只额外加
// inset 阴影：顶部高光（rim）与内圈发丝线，pattern 与 Surface 一致。
//
// 三态颜色来自 Theme（AccentHover/AccentPressed 真的不同），所以按下有反馈。
func (a *app) actionButton(c *ui.Context, p *glass.Palette, key, label string, primary bool) ui.Element {
	if primary {
		return ui.PrimaryButton(c.Key(key), label).
			FontSize(12).FontWeight(540).Padding(0, 14).
			Height(30).Radius(glass.RadiusControl).
			InsetShadow(0, 1, 0, 0, p.PrimaryRim).
			InsetShadow(0, 0, 0, 1, p.PrimaryInner).
			Shadow(0, 1, 2, 0, p.PrimaryUnder).
			Shadow(0, 10, 22, -12, ui.RGBA(0, 0, 0, 0.80))
	}
	return ui.Button(c.Key(key), label).
		FontSize(11.5).FontWeight(520).Padding(0, 12).
		Height(28).Radius(glass.RadiusControl).
		InsetShadow(0, 1, 0, 0, p.ControlRim).
		InsetShadow(0, 0, 0, 1, ui.RGBA(255, 255, 255, 0.045)).
		Shadow(0, 0, 0, 1, ui.RGBA(255, 255, 255, 0.055)).
		Shadow(0, 1, 2, 0, ui.RGBA(0, 0, 0, 0.25))
}

// noticeLine 一次性操作反馈（设置保存失败、复制失败）。
//
// 只在失败时出现：成功都有别的、更靠近源头的地方在说（复制→徽标变对勾、
// 启动→开关变绿、设置写盘→下拉框自己已经改了），再补一句是重复。
//
// 备注：这一行参与内容流（不是浮层），出现时会把下面的卡片推下去。它只在
// 「用户刚做了一个动作、结果出乎意料」时出现，属于需要读的那类信息，
// 挤一下布局是可接受的；反过来，成功提示绝不能走这条路——那会让每次
// 复制都晃一下图表。
func (a *app) noticeLine(c *ui.Context, p *glass.Palette) {
	if a.notice == "" || time.Now().After(a.noticeExp) {
		return
	}
	color := p.Success
	if a.noticeErr {
		color = p.Danger
	}
	ui.Text(c, a.notice).FontSize(11).TextColor(color).MaxLines(2)
}

// ---- 设置 tab ----

// settingsPane 「设置」tab：两个视觉分区（连接 / 其他），仍在**一张**卡片里。
//
// 分组是需求（owner：「设置页内两个分区，不用滚」）。做法上刻意**不**拆成两张
// 卡片：上一版拆过一次，两张卡各自的圆角、内边距与卡间距吃掉约 40px，正好把
// 最后一行「显示区间」顶出 400px 窗口的可视区（见本文件历史）。
//
// 现在用「组标题 + 组间加高间隙」表达分区：
//
//	连接  上游地址 / 服务器域名 / 网络（直连·系统·代理）/ 监听地址 / 传输方式
//	其他  开机自启 / 日志保留 / 显示区间 / 自动更新 / 更新镜像 / 软件更新
//
// 组标题本身只有一行 11px 文字（约 26px 含上下留白），比一张卡的装饰开销小一个
// 数量级；卡片的厚度语义也保持单层，不会读成「两张卡上下堆着」。
func (a *app) settingsPane(c *ui.Context, p *glass.Palette) {
	running := a.running()

	glass.Surface(ui.Column(c).Padding(3, 14, 6), p, glass.Card).Children(func() {
		a.groupTitle(c, p, "连接", true)
		a.configField(c, p, "上游地址", &a.server, relayclient.DefaultServer, running)
		a.configRule(c, p)
		// TLS SNI：用 IP 连服务器时必须填证书上的域名，否则 x509 校验失败
		// （证书 SAN 只有 DNS，没有 IP）。留空时按上游地址推导，只在
		// 「上游地址是域名且证书就是那个域名」时才成立。
		a.configField(c, p, "服务器域名", &a.serverName,
			relayclient.DefaultServerName, running)
		a.configRule(c, p)
		// 源地址绑定：开代理 TUN 全局模式时，发往隧道服务器的流量也会被导进
		// utun。代理对 28443 这类非标端口没有正确规则，TLS 握手会被中途切断，
		// 客户端只看到 "tcp tls: EOF"——看起来像服务端崩了。填物理网卡 IP
		// （macOS: ipconfig getifaddr en0）让内核直接走该网卡即可绕过。
		a.networkRow(c, p, running)
		a.configRule(c, p)
		a.configField(c, p, "监听地址", &a.listen, relayclient.DefaultListen, running)
		a.configRule(c, p)
		a.transportRow(c, p, running)

		a.groupTitle(c, p, "其他", false)
		a.autoStartRow(c, p)
		a.configRule(c, p)
		// 两个时间跨度不碰监听，运行中改也安全：它们的提示在各自行内，
		// 不参与下面那句「先停止再改」。
		a.retainRow(c, p)
		a.configRule(c, p)
		a.windowRow(c, p)
		a.configRule(c, p)
		a.autoUpdateRow(c, p)
		a.configRule(c, p)
		a.mirrorRow(c, p)
		a.configRule(c, p)
		a.updateRow(c, p)

		// 运行中不可改：说清「为什么点不动」+「怎么做」，而不是让输入框静静失效。
		// 只针对上面那五个「连接类」字段——统计跨度与更新偏好随时可改。
		//
		// 这一行必须留在卡片内：上一版它跟在卡片外面，整行左缘比卡片内容列
		// 左凸 14px，而且 28px 高的「停止」按钮会悬在卡片底边之下约 11px，
		// 读起来像一个没有容器承接的浮层。放进卡片后它和字段共用同一条
		// 内容栅格，分隔线也把它明确归到「连接」这一组的尾部。
		if running {
			a.configRule(c, p)
			ui.Row(c).Padding(6, 0).AlignItems(ui.Center).Gap(6).Children(func() {
				ui.Text(c, "连接设置在运行中不可修改，先停止再改").
					FontSize(11).TextColor(p.Warning).MaxLines(1).Grow(1)
				if a.actionButton(c, p, "stop-settings", "停止", false).Clicked() {
					a.stop()
				}
			})
		}
	})
}

// groupTitle 设置卡内的分区标题（分隔靠「标题 + 上方加高留白」，不靠第二张卡）。
//
// 只有 11px 文字 + 少量留白（不做背景块、不加边框）：分区要在扫视时被认出来，
// 而不是被读成一张新卡片。
//
// # 为什么颜色是 Foreground 而不是 MutedForeground
//
// 行标签（「上游地址」这些）用的就是 MutedForeground + 11px，字号与色相都和
// 分区标题一样，只差字重——实测截图里「连接」「其他」读起来就是两个额外字段名，
// 分组等于没有（判读依据：分组的落点找不到「停一下」的视觉重量差）。
//
// 换成 Foreground（卡片里除输入值以外最亮的一档）之后标题在层级上确实高于行
// 标签，读者先看到「连接」再往下读字段。同时给首组之外的标题加 14 的顶部留白：
// 留白比加一条发丝线更能表达「换了一组」——那个位置本来就已经有一条行间线，
// 再加一条同色的只会读成又多了一行。
func (a *app) groupTitle(c *ui.Context, p *glass.Palette, text string, first bool) {
	top, bottom := float32(14), float32(5)
	if first {
		top, bottom = 4, 4
	}
	ui.Text(c, text).FontSize(11).FontWeight(620).LetterSpacing(0.8).
		TextColor(p.Foreground).SingleLine().
		Padding(top, 0, bottom, 0)
}

// retainRow 日志保留时长：磁盘上留多久的数据（同时决定重启重建的扫描范围）。
func (a *app) retainRow(c *ui.Context, p *glass.Palette) {
	labels := humanOptions(relayclient.RetainOptions)
	idx := clampIndex(a.retainIdx, len(labels))
	sel := labels[idx]

	a.timeRow(c, p, "日志保留", &sel, labels, "保留多久就占多少空间")
	// Select 直接改 sel：值变了就说明用户选了别的档位。
	if sel != labels[idx] {
		a.retainIdx = indexOf(labels, sel)
		a.applyTimeSettings()
	}
}

// windowRow 图表显示区间：柱状图横轴跨度（每桶粒度由它派生）。
//
// 「不超过保留时长」这个约束不在这里判断，统一由 applyTimeSettings 兜住：
// 从下拉选、从 settings.json 读、从 flag 覆盖，三条入口都必须得到同一个
// 合法组合，分散判断必然有一条会漏。
func (a *app) windowRow(c *ui.Context, p *glass.Palette) {
	labels := humanOptions(relayclient.WindowOptions)
	idx := clampIndex(a.windowIdx, len(labels))
	sel := labels[idx]

	a.timeRow(c, p, "显示区间", &sel, labels, "图横轴的跨度")
	if sel != labels[idx] {
		a.windowIdx = indexOf(labels, sel)
		a.applyTimeSettings()
	}
}

// timeRow 一行「标签 + 下拉 + 说明」
// timeRow 一行「标签 + 下拉 + 说明」。
//
// 宽度是量出来的，不是拍的：Select 有自己的最小内在宽度（最长档位 "30 分钟"
// 加右侧箭头约 130px），给它一个比这更小的盒子它会溢出到相邻的说明文字上
// （实测 104 宽时右边框压住提示文字）。这里给 132 并让说明只占剩下的空间。
func (a *app) timeRow(c *ui.Context, p *glass.Palette, label string, val *string, options []string, hint string) {
	ui.Row(c).Padding(6, 0).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Width(84).Shrink(0).Children(func() {
			ui.Text(c, label).FontSize(11).TextColor(p.MutedForeground).SingleLine()
		})
		ui.Box(c).Width(132).Shrink(0).Children(func() {
			ui.Select(c, val, options).FontSize(11).Height(26)
		})
		ui.Text(c, hint).FontSize(10.5).TextColor(p.Tertiary).MaxLines(1).Grow(1)
	})
}

// humanOptions 把档位列表转成下拉里的显示文案（30 分钟 / 7 天）
func humanOptions(opts []time.Duration) []string {
	out := make([]string, len(opts))
	for i, d := range opts {
		out[i] = humanWindow(int(d / time.Second))
	}
	return out
}

// indexOf 在下拉文案里找下标（找不到时回 0，防越界）
func indexOf(labels []string, s string) int {
	for i, l := range labels {
		if l == s {
			return i
		}
	}
	return 0
}

// configRule 设置行之间的发丝线
func (a *app) configRule(c *ui.Context, p *glass.Palette) {
	ui.Box(c).Height(1).FillWidth().Background(p.Hairline)
}

// configField 一行「标签 + 输入框」（标签固定 84px，两行字段左边缘对齐）
func (a *app) configField(c *ui.Context, p *glass.Palette, label string, val *string, placeholder string, disabled bool) {
	ui.Row(c).Padding(6, 0).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Width(84).Shrink(0).Children(func() {
			ui.Text(c, label).FontSize(11).TextColor(p.MutedForeground).SingleLine()
		})
		ui.Box(c).Grow(1).Children(func() {
			ui.TextInput(c, val).Placeholder(placeholder).
				FontSize(11).Height(26).Radius(9).Disabled(disabled)
		})
	})
}

// transportRow 传输方式：三段分段控件（设计稿的 seg）
// networkRow 网络出口三选一：直连 / 系统 / 代理。
//
// 为什么需要这一行（owner 2026-11-14）：代理软件的 TUN 全局模式会把整个
// IPv4 路由接管到虚拟网卡（实测 route get 47.82.76.102 → 198.18.0.1/utun5），
// 连隧道服务器的包也被导进代理，代理对 28443 这类非标端口没有正确规则，
// TLS 被切断——客户端只看到 `tcp tls: EOF` 或 QUIC 超时。以前让用户手填
// 「源地址」，等于把排查工作推给用户；现在三种模式说清楚：
//
//	直连  自动探测物理网卡 IP 并绑定（绕开 TUN），换网络也不用改
//	系统  什么都不做，交给操作系统路由（被 TUN 劫持就连不上）
//	代理  走 http/https/socks5 代理（QUIC 不可用，自动降级 TCP）
func (a *app) networkRow(c *ui.Context, p *glass.Palette, disabled bool) {
	options := []string{relayclient.NetworkDirect, relayclient.NetworkSystem, relayclient.NetworkProxy}
	labels := []string{"直连", "系统", "代理"}
	idx := 0
	for i, o := range options {
		if o == a.netMode {
			idx = i
		}
	}
	ui.Row(c).Padding(6, 0).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Width(84).Shrink(0).Children(func() {
			ui.Text(c, "网络").FontSize(11).TextColor(p.MutedForeground).SingleLine()
		})
		ui.Box(c).Grow(1).Children(func() {
			ui.Segmented(c, &idx, labels...).FontSize(11.5).Height(24).Disabled(disabled)
		})
	})
	if options[idx] != a.netMode {
		a.netMode = options[idx]
		// 切到直连时立刻探测一次，界面上能马上看到会绑哪个 IP；探不到
		// 就留空（Dial 会退回系统路由），不让切换本身失败。
		if a.netMode == relayclient.NetworkDirect {
			if ip, iface, err := netif.PrimaryIPv4(); err == nil {
				a.detectedIP, a.detectedIface = ip, iface
			}
		}
	}

	// 一行说明：三选一各自的含义不同，光看标签猜不出来。
	hint := ""
	switch a.netMode {
	case relayclient.NetworkDirect:
		if a.detectedIP != "" {
			hint = "绑定 " + a.detectedIface + " " + a.detectedIP + "，绕开 TUN 代理"
		} else {
			hint = "未探测到物理网卡，退回系统路由"
		}
	case relayclient.NetworkProxy:
		hint = "代理只承载 TCP，QUIC 自动降级"
	default:
		hint = "跟随系统路由（TUN 全局模式下可能连不上）"
	}
	ui.Row(c).Padding(6, 0).Gap(12).Children(func() {
		ui.Box(c).Width(84).Shrink(0)
		ui.Text(c, hint).FontSize(10).TextColor(p.Tertiary).SingleLine()
	})

	if a.netMode == relayclient.NetworkProxy {
		a.configField(c, p, "代理地址", &a.proxyURL,
			"http://127.0.0.1:7890 或 socks5://127.0.0.1:7890", disabled)
	}
}

func (a *app) transportRow(c *ui.Context, p *glass.Palette, disabled bool) {
	options := []string{"auto", "quic", "tcp"}
	labels := []string{"自动", "QUIC", "TCP"}
	idx := 0
	for i, o := range options {
		if o == a.transport {
			idx = i
		}
	}
	ui.Row(c).Padding(6, 0).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Width(84).Shrink(0).Children(func() {
			ui.Text(c, "传输方式").FontSize(11).TextColor(p.MutedForeground).SingleLine()
		})
		ui.Box(c).Grow(1).Children(func() {
			ui.Segmented(c, &idx, labels...).FontSize(11.5).Height(24).Disabled(disabled)
		})
	})
	if options[idx] != a.transport {
		a.transport = options[idx]
	}
}

// autoStartRow 开机自启开关。
//
// 这一行展示的是**操作系统登录项的真实状态**（每次切换都写 OS 再读回），
// 所以它和系统设置 / 任务管理器里看到的一致。
//
// 语言上要说清两件事：它是登录项（不是「后台服务」），以及下次登录才生效。
func (a *app) autoStartRow(c *ui.Context, p *glass.Palette) {
	ui.Row(c).Padding(6, 0).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Width(84).Shrink(0).Children(func() {
			ui.Text(c, "开机自启").FontSize(11).TextColor(p.MutedForeground).SingleLine()
		})

		if !autostart.Available() {
			ui.Text(c, "当前平台不支持").FontSize(11).TextColor(p.Tertiary).SingleLine()
			return
		}

		ui.Column(c).Grow(1).Gap(1).Children(func() {
			ui.Text(c, "登录后自动驻留托盘，下次登录生效").
				FontSize(11).TextColor(p.Tertiary).SingleLine()
		})
		// 开关直接切 a.autoStart；OnChange 在绑定值变化后、重绘之前触发，
		// 所以 applyAutostart 写盘 + 读回的结果会立刻反映到下一个帧；
		// 写失败时它会把 a.autoStart 翻回去，开关自己弹回原位。
		a.stateSwitch(c, p, &a.autoStart, false, "开机自启").OnChange(a.applyAutostart)
	})
}

// stateSwitch 开关的绘制：状态必须靠**位置 + 颜色**两件事表达，而不是只靠亮度。
//
// 框架自带的 ui.Switch 用 140ms 动画把拇指从左滑到右，但截图/首帧落在动画
// 起点，开与关都会画成「拇指在左」，只剩轨道亮度的差别；两个开关并排时，
// 关着的那个（或禁用的那个）还会因为亮度低被读成「禁用」而不是「关」。
// 这里直接从绑定值取位置：关 → 拇指在左、灰轨道；开 → 拇指在右、成功色轨道。
// 缩到一半尺寸时，16px 的横向位移仍是可辨认的差异。
func (a *app) stateSwitch(c *ui.Context, p *glass.Palette, value *bool, disabled bool, label string) ui.Element {
	on := *value
	sw := ui.SwitchBase(c, value).Size(36, 20).Shrink(0).
		Label(label).Disabled(disabled).
		Cursor(ui.CursorPointer)
	sw.Draw(func(pt *ui.Painter, r ui.Rect) {
		track, thumb := p.MutedForeground.Alpha(0.38), p.Tertiary
		if on {
			track, thumb = p.Success, ui.Hex("#ffffff")
		}
		if disabled {
			track, thumb = track.Alpha(0.55), thumb.Alpha(0.55)
		}
		pt.Fill(r, track, r.H/2)

		knob := switchKnobRect(r, on)
		pt.Shadow(knob, knob.H/2, 0, 1, 3, 0, ui.RGBA(0, 0, 0, 0.25))
		pt.Fill(knob, thumb, knob.H/2)
	})
	return sw
}

// switchKnobRect 拇指的落点：关在左、开在右（两端各内缩 2px）。
//
// 抽成纯函数是为了能被测试盯住：状态不能只靠亮度，位置差必须真实存在。
func switchKnobRect(r ui.Rect, on bool) ui.Rect {
	d := r.H - 4
	x := r.X + 2
	if on {
		x = r.X + r.W - d - 2
	}
	return ui.Rect{X: x, Y: r.Y + 2, W: d, H: d}
}

// ---- footer ----

// footer 钉在窗口底部的信息条：左侧一句话状态，右侧版本号。
//
// 写用户看得懂的话，不写实现细节（「mygo native UI · solid-glass 材质」对
// 用户零价值）。版本号只在这里出现一次。
func (a *app) footer(c *ui.Context, p *glass.Palette) {
	// 先画上边发丝线，再画内容（Row 不能反向插入已画元素，所以顺序即层次）
	ui.Box(c).Height(1).FillWidth().Background(p.Hairline)

	state := "未运行"
	if a.running() {
		state = "运行中"
		// 时长只在有真实启动时刻时追加：测试/截图里 StartedAt 是零值，
		// time.Since(零值) 会算出「2562047 小时」这种荒唐数字。
		if up := a.runner.StartedAt(); !up.IsZero() {
			state += " " + humanDuration(time.Since(up))
		}
	}
	// 单栏：状态 · 传输 · 统计摘要 ……… 版本号
	//
	// 2026-11-13 owner 要求「窗口做大，底部就不用两栏、合并成一栏」。原来的两栏是
	// 「内容区末尾的统计+复制」和「窗口底部状态条」——它们都只有一行高，各占
	// 27~32px，合起来在一屏里读作两条重复的底边。合并后信息密度反而更高：
	// 用户在同一个水平线上从左到右读到「跑没跑 → 走的什么 → 效果如何」。
	//
	// 顺序是有意义的，不是随便排：状态是「最常看」，统计摘要是「顺带看」，
	// 版本是最不需要看的（放最右，视线最少落到那里）。底栏不再有复制按钮：
	// 那是一个动作，放在顶栏徽标上；状态栏保持纯读取。
	ui.Row(c).Height(32).FillWidth().Padding(0, 16).
		AlignItems(ui.Center).Background(p.PageDim).Gap(footerSegmentGap).Children(func() {
		// 状态与传输方式之间、以及与后面的统计摘要之间，用的是同一种分隔点
		// 「 · 」。上一版把摘要前的点写成独立元素并给 Row 设了 Gap(10)，
		// 于是同一行里出现两种点距（实测 14/13px vs 7/8px），读起来像
		// 排版事故而不是分组。「自动选择传输」两侧那种多出来的空隙就是这么来的。
		head := fmt.Sprintf("%s · %s", state, a.transportLabel())

		// 统计摘要：只在有数据时出现。空态下这一格是「还没有请求记录」，
		// 而状态区已经说了「未运行」——同一件事说两遍只会稀释信息。
		if line := a.footerStatsLine(); line != "" {
			ui.Text(c, head+" ·").FontSize(10.5).TextColor(p.Tertiary).SingleLine()
			ui.Text(c, line).FontSize(10.5).FontFeatures("tnum").
				TextColor(p.MutedForeground).MaxLines(1).Grow(1)
		} else {
			ui.Text(c, head).FontSize(10.5).TextColor(p.Tertiary).SingleLine()
			ui.Box(c).Grow(1)
		}

		// 复制入口只在顶栏徽标：底栏不再放第二个按钮（owner 2026-11-13：
		// 「底部状态栏不需要"复制地址"这个按钮」）。状态栏只负责「读」，
		// 动作留在用户视线最先落到的那枚地址徽标上。

		// 用 displayVersion 统一（注入源有的带 v 有的不带），且开发构建
		// （"dev"）不显示版本号——"vdev" 对用户零信息量，只会被读成异常。
		if v := displayVersion(version); v != "" {
			ui.Text(c, v).FontSize(9.5).
				Font("monospace").TextColor(p.Tertiary).SingleLine()
		}
	})
}

// footerSegmentGap 底栏里两段文字之间的间隙（DIPs）。
//
// 4 ≈ 一个 10.5px 的半角空格：底栏内部的分隔点是写在字符串里的「 · 」，
// 段与段之间用同一个间隙，两处点距才不会一个 7px、一个 14px（见 footer）。
// 取 0 会把两段的字挤在一起，取 10 又回到「两种点距」的老问题。
const footerSegmentGap float32 = 4

// footerStatsLine 底部那一行里的统计摘要（空态返回空串，让调用方跳过）。
//
// 口径从「本次会话」改成「近 N 小时」：统计自 2026-11-13 起从磁盘日志重建，
// 重启不清零，所以「本次会话」已名不副实。前缀写明保留期，让时间跨度可见。
//
// 延迟读数走 humanMs（与卡片摘要里的「当前/峰值/均值」同一个换算函数）：
// 底栏曾经写裸毫秒（"p50 27766 ms"），同一屏上卡片写「31.1 s」——同一量纲
// 两种写法，读者得自己换算。
func (a *app) footerStatsLine() string {
	g := a.agg
	if g.Requests == 0 {
		return ""
	}
	saved := g.UpRaw - g.UpWire
	if saved < 0 {
		saved = 0
	}
	retain := int(g.RetainSec)
	if retain <= 0 {
		retain = int(stats.DefaultRetain / time.Second)
	}
	return fmt.Sprintf("近 %s %s 请求 · 节省 %s · p50 %s",
		humanWindow(retain), groupDigits(g.Requests), humanBytes(saved), humanMs(g.P50Ms))
}

// transportLabel 传输方式的显示名
func (a *app) transportLabel() string {
	switch a.transport {
	case "quic":
		return "QUIC"
	case "tcp":
		return "TCP"
	default:
		return "自动选择传输"
	}
}
