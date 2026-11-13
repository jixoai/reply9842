// view.go — 界面绘制（mygo native UI 的模型：view 是状态的纯函数）
//
// 布局（2026-11-13 kzf 反馈后重排）：
//
//	titlebar  品牌标 + 应用名 + [面板｜设置] tabs + 端口徽标
//	面板 tab  状态行（状态 + 主操作同一行）· 上行压缩 hero · 三格统计 · 复制配置
//	设置 tab  上游 / 监听 / 传输方式 / 开机自启
//	footer    运行态 + 传输方式 + 版本号（版本号只在这里出现）
//
// 空态的两条硬规则：
//   - 不摆 68px 巨号：空态没有数字，摆「—」是一根读作「坏掉的进度条」的粗横条，
//     摆「0×」又会被读成「压缩率是 0」
//   - 三格统计整条不出现：空卡占 25% 面积却零信息量，空态的信息由 hero 的一句话承担
package main

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/jixoai/rfc9842-reply/internal/autostart"
	"github.com/jixoai/rfc9842-reply/internal/glass"
	"github.com/jixoai/rfc9842-reply/internal/relayclient"
	"github.com/jixoai/rfc9842-reply/internal/stats"
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
		// 内容滚动、footer 钉底、复制按钮钉在内容区底部。
		//
		// 为什么把复制按钮从内容流里拿出来钉底：两个 tab 的内容高度差很大
		// （面板运行态约 350px，空态只有约 235px），窗口高度是固定的，
		// 空态底部那 100+px 若直接留空会读成「没画完」。让次要操作贴住
		// 内容区的下沿，这段空白就变成「上面是主体、下面是动作」的结构，
		// 而不是漏画的区域。
		ui.Column(c).Grow(1).Children(func() {
			ui.Scroll(c).Grow(1).Children(func() {
				ui.Column(c).Padding(16, 16, 10).Gap(10).Children(func() {
					if a.tab == tabSettings {
						a.settingsPane(c, p)
					} else {
						a.panelPane(c, p)
					}
					a.noticeLine(c, p)
				})
			})
			a.footerPane(c, p)
		})
		a.footer(c, p)
	})
}

// ---- titlebar ----

// titleBar 自绘标题栏：品牌标 + 应用名 + tabs + 端口徽标。
//
// 拖拽交给 DragWindow（双击最大化的行为也由它负责）。
// 左侧留出红绿灯的位置（c.TitleBar().Left）——不留的话红绿灯会压住品牌标。
//
// 版本号不在这里：它和 footer 各显示一次时，用户会以为是两个不同的东西。
func (a *app) titleBar(c *ui.Context, p *glass.Palette) {
	bar := c.TitleBar()
	height := max(bar.Height, 44)

	ui.Row(c).Height(height).FillWidth().
		Padding(0, 12, 0, max(bar.Left, 14)).
		Gap(9).AlignItems(ui.Center).
		Background(p.PageDim).
		DragWindow().
		Children(func() {
			// 品牌标：三条圆角横杠（设计稿的 mark）
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

			ui.Text(c, "rfc9842-reply").FontSize(12.5).FontWeight(590).
				LetterSpacing(0.06).TextColor(p.Foreground).SingleLine()
			ui.Box(c).Grow(1)

			// tabs 在右上角（用户明确要求），紧挨端口徽标左侧
			// 用 Segmented 而不是 ui.Tabs：Tabs 的选中态是一条下划线，
			// 在这条 44px 的深色条上几乎看不见；分段控件的选中块与玻璃
			// 材质语言一致（「一片薄玻璃贴在厚玻璃上」）。
			ui.Segmented(c, &a.tab, "面板", "设置").
				FontSize(11.5).Height(26).Shrink(0)

			a.portBadge(c, p)
		})

	// 底边发丝线：与 body 之间一道分界，让 titlebar 读作一条独立 chrome
	ui.Box(c).Height(1).FillWidth().Background(p.Hairline)
}

// portBadge 监听端口的徽标（设计稿的 brand-port：mono + 浅底 + 内描边）
func (a *app) portBadge(c *ui.Context, p *glass.Palette) {
	addr := a.listen
	if a.running() {
		if r := a.runner.Addr(); r != "" {
			addr = r
		}
	}
	ui.Row(c).Padding(2.5, 6).Background(ui.RGBA(255, 255, 255, 0.04)).
		InsetShadow(0, 0, 0, 1, ui.RGBA(255, 255, 255, 0.045)).
		Radius(6).Shrink(0).
		Children(func() {
			ui.Text(c, addr).FontSize(10).Font("monospace").
				TextColor(p.Tertiary).SingleLine()
		})
}

// ---- 面板 tab ----

// panelPane 「面板」tab 的全部内容
func (a *app) panelPane(c *ui.Context, p *glass.Palette) {
	a.statusRow(c, p)
	a.chartCard(c, p)
}

// statusRow 状态 + 主操作同一行。
//
// 为什么合并：独立的状态卡用 90px 高度说「没事发生」，却占着首屏黄金位；
// 而「启动代理」是用户打开这个窗口的唯一目的，应该在第一屏最显眼处。
//
//	┌──────────────────────────────────────────────┐
//	│ ● 已停止 · 本地流量不经过压缩          [启动代理] │
//	└──────────────────────────────────────────────┘
func (a *app) statusRow(c *ui.Context, p *glass.Palette) {
	kind, label, detail := a.state()
	color := p.MutedForeground
	switch kind {
	case stateRunning:
		color = p.Success
	case stateFailed:
		color = p.Danger
	}

	glass.Surface(ui.Row(c).Padding(9, 12).AlignItems(ui.Center).Gap(9), p, glass.Panel()).
		Children(func() {
			// 状态点：实心小圆 + 一圈极淡外描边。
			// 不用同色光晕——solid-glass 的规矩是「光影在边缘」，雾状红晕是它明确反对的。
			ui.Box(c).Size(8, 8).Radius(4).Shrink(0).Background(color).
				Shadow(0, 0, 0, 1, ui.RGBA(0, 0, 0, 0.35))

			// 大字号用 15 而不是设计稿的 23：这是一行内联状态，不是页面标题。
			// 23px 会把这一行顶到 46px 高，又把「一屏放下」破坏掉。
			ui.Text(c, label).FontSize(15).FontWeight(600).
				LetterSpacing(-0.16).TextColor(color).SingleLine()

			ui.Text(c, "·").FontSize(11).TextColor(p.Tertiary).SingleLine()
			ui.Text(c, detail).FontSize(11).TextColor(p.MutedForeground).
				MaxLines(1).Grow(1)

			// 主操作按钮贴右：它是这一行唯一的可点目标，位置固定才不会
			// 在「启动/停止」切换时左右跳。
			if a.running() {
				if a.actionButton(c, p, "stop", "停止", true).Clicked() {
					a.stop()
				}
			} else {
				if a.actionButton(c, p, "start", "启动", true).Clicked() {
					a.start()
				}
			}
		})
}

// liveBadge 「实时 / 已暂停」胶囊（内嵌 4px 圆点）。
//
// 圆点不带同色发光：solid-glass 的规矩是「光影在边缘」，雾状光晕会把 8px 的
// 状态点晕成 20px 的一团（上一轮真实截图里，错误态的红晕半径约 20px）。
// 要强调就给一圈极淡的外描边。
func (a *app) liveBadge(c *ui.Context, p *glass.Palette, running bool) {
	dot, text := p.Success, "实时"
	if !running {
		dot, text = p.Tertiary, "已暂停"
	}
	ui.Row(c).Padding(2.5, 7, 2.5, 5).Gap(5).AlignItems(ui.Center).
		Background(ui.RGBA(255, 255, 255, 0.045)).
		InsetShadow(0, 0, 0, 1, ui.RGBA(255, 255, 255, 0.05)).
		Radius(999).Shrink(0).
		Children(func() {
			ui.Box(c).Size(4, 4).Radius(2).Background(dot).Shrink(0)
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

	glass.Surface(ui.Column(c).Padding(12, 14, 12).Gap(8), p, glass.Card).Children(func() {
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
			// 高度只给 56 而不是 chartHeight：空态撑满 132px 只是把一句
			// 引导泡在一大片空白里，反而显得没画完。
			ui.Box(c).Height(56).FillWidth().AlignItems(ui.Center).
				Justify(ui.Center).Children(func() {
				ui.Text(c, a.emptyChartNote()).FontSize(11.5).
					TextColor(p.MutedForeground).SingleLine()
			})
			return
		}

		a.chartPlot(c, p)
		ui.Text(c, a.chartSummary(g)).FontSize(11).
			FontFeatures("tnum").TextColor(p.MutedForeground).MaxLines(1)
	})
}

// chartHeight 绘图区高度（DIPs）。130 让 5% 的差异也有 6px 可读高度，
// 又不至于把窗口顶到需要滚动。
const chartHeight float32 = 132

// 图表配色。绿 = 省下的（赚到的），蓝 = 实际发出去的（花掉的）。
//
// 为什么不用 p.Foreground / p.Tertiary 这套中性灰：柱状图里两段必须能**在
// 一秒内分辨**是哪一段，而灰度差在中灰底的小面积上会被抗锯齿冲淡。蓝绿是
// 色相差异，比明度差异更抗干扰；两者都对深色底有足够对比（实测 #4ADE80
// 在 #202020 上约 9.8:1，#3B82F6 约 5.1:1），都在 4.5:1 以上。
//
// 绿用偏亮的一档（#4ADE80 而非 #22C55E）：它是柱子的主体面积，太暗会让
// 整张图在深色页面上显得「没点亮」。蓝保持中等亮度，避免与绿争夺注意力。
var (
	chartSaved = ui.Hex("#4ADE80")
	chartWire  = ui.Hex("#3B82F6")
)

// minWirePx 蓝段（实际发送）的最小可见高度。
//
// 压缩 99.9% 时它按比例只有 0.2px，抗锯齿后会完全消失，整根柱子读成纯亮色——
// 那会丢失「仍然发了东西」这个事实。3px 是 Retina 上能稳定看见一条线的高度。
const minWirePx float32 = 3

// threshold 90% 参考线：让每根柱子自动跟「好成绩」对比。
//
// 为什么是 90%：本产品的生产数据长期在 99% 以上，90% 是「已经明显生效」的门槛；
// 低于它说明这一秒的请求都是小包或首包（要走全量），看一眼就知道发生了什么。
const gaugeThreshold = 0.90

// hasSeries 是否有可画的数据（有一秒带流量即可）
func (a *app) hasSeries() bool {
	for _, b := range a.agg.Series {
		if b.UpRaw > 0 {
			return true
		}
	}
	return false
}

// emptyChartNote 空态在图上说什么：分清「没启动」与「启动了但还没请求」。
// 两者对用户的下一步完全不同（启动 / 去用 Agent）。
func (a *app) emptyChartNote() string {
	if !a.running() {
		return "启动代理后，这里显示每秒的上行体积与压缩效果"
	}
	return "等待第一个请求经过本地代理"
}

// legend 图例。单列分层必须说明哪段是什么，否则两根不同颜色的段无从解释。
//
// 用两块真的方块（而不是 ■/▨ 字符）：字体缺字形时会变成豆腐块，
// 而方块是画出来的，任何字体下都在。
func (a *app) legend(c *ui.Context, p *glass.Palette) {
	ui.Row(c).Gap(9).AlignItems(ui.Center).Shrink(0).Children(func() {
		a.legendSwatch(c, chartSaved, "省下")
		a.legendSwatch(c, chartWire, "实际发送")
	})
}

func (a *app) legendSwatch(c *ui.Context, col ui.Color, label string) {
	ui.Row(c).Gap(4).AlignItems(ui.Center).Shrink(0).Children(func() {
		ui.Box(c).Size(7, 7).Radius(1.5).Shrink(0).Background(col)
		ui.Text(c, label).FontSize(10).TextColor(a.pal.Tertiary).SingleLine()
	})
}

// chartPlot 柱状图本体：纵轴刻度 + 90% 参考线 + 每分钟一根分层柱。
//
// 每根柱子的画法（自底向上）：
//
//	barH   = max(省下的比例 × 可用高度, minWirePx)   ← 柱子总高 = 压缩率
//	wireH  = min(minWirePx, barH)                   （蓝，贴底）
//	savedH = barH - wireH                           （绿，在上）
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
	plot := ui.Row(c).Height(chartHeight).FillWidth().AlignItems(ui.Stretch).Gap(6)

	plot.Children(func() {
		// 纵轴刻度：0/25/50/75/100%。五档是因为柱高直接读作百分数，
		// 少一档就得靠目测插值；26px 宽的标签列装得下「100%」。
		ui.Column(c).Width(28).Shrink(0).AlignItems(ui.End).Children(func() {
			for i, label := range []string{"100%", "75%", "50%", "25%", "0"} {
				if i > 0 {
					ui.Box(c).Grow(1)
				}
				ui.Text(c, label).FontSize(8.5).FontFeatures("tnum").
					TextColor(p.Tertiary).SingleLine()
			}
		})

		// 绘图区
		ui.Box(c).Grow(1).Draw(func(pt *ui.Painter, r ui.Rect) {
			if r.W <= 0 || r.H <= 0 {
				return
			}
			// 基线（0%）：1px 发丝线，不给网格
			pt.Fill(ui.Rect{X: r.X, Y: r.Y + r.H - 1, W: r.W, H: 1}, p.Edge, 0)

			// 90% 参考线：虚线。画实线会与柱顶混在一起；
			// 3px/3px 的节奏足够让人看出「这是参考，不是数据」。
			ty := r.Y + r.H*(1-gaugeThreshold)
			for x := float32(0); x < r.W; x += 6 {
				w := min(float32(3), r.W-x)
				pt.Fill(ui.Rect{X: r.X + x, Y: ty, W: w, H: 1}, p.Border, 0)
			}

			n := len(series)
			if n == 0 {
				return
			}
			// 柱间留 3px：30 根柱子在 380px 里每根约 9.6px，留 3px 后
			// 净宽 6.6px —— 足够看出「一根一根」，也容得下 2px 圆角。
			const gap float32 = 3
			slot := r.W / float32(n)
			bw := max(slot-gap, 1)

			// 柱顶留 4% 空隙：100% 压缩的柱子否则会顶到卡片上沿，像被裁了。
			usable := r.H * 0.96

			for i, b := range series {
				if b.UpRaw <= 0 {
					continue // 空分钟不画：基线上的空白就是「这段时间没流量」
				}
				saved := clampF(1-float64(b.UpWire)/float64(b.UpRaw), 0, 1)

				barH := float32(saved) * usable
				// 一点没省（saved=0）时柱子高度为 0，只剩一条蓝线：
				// 那是准确的——「这一分钟完全没有优化」。
				wireH := min(minWirePx, barH)
				if barH < wireH {
					barH = wireH
				}
				savedH := barH - wireH

				x := r.X + float32(i)*slot + (slot-bw)/2
				bottom := r.Y + r.H - 1

				// 绿段（省下）：柱子的主体，从上沿往下
				if savedH > 0 {
					pt.Fill(ui.Rect{X: x, Y: bottom - barH, W: bw, H: savedH},
						chartSaved, min(2, bw/2))
				}
				// 蓝段（实际发送）：贴底
				pt.Fill(ui.Rect{X: x, Y: bottom - wireH, W: bw, H: wireH},
					chartWire, min(2, bw/2))
			}
		})
	})
}

// chartSummary 图下方那行文字：图给比例，文字给绝对值，各司其职。
//
// 统计窗口固定为 SeriesSeconds（60）。用「最近 60 秒」而不是「本次会话」：
// 图上的柱子就是这 60 秒，文字与图必须说的是同一段时间，否则用户会把两个
// 数字当成同一件事的两个说法。
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
	return fmt.Sprintf("最近 %s：原始 %s · 实际发送 %s · 省下 %.1f%%（%s 次请求）",
		humanWindow(stats.SeriesSeconds),
		humanBytes(raw), humanBytes(wire), saved*100, groupDigits(n))
}

// humanWindow 把秒数说成人话（1800 → "30 分钟"）。
//
// 为什么不直接写「1800 秒」：图上每根柱子是一分钟，「30 分钟」让读者能
// 把「30 根柱子」和「30 分钟」对起来；「1800 秒」需要先做一次除法。
func humanWindow(seconds int) string {
	switch {
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

// footerPane 内容区底部的一行：左侧三格统计压成的文字摘要，右侧复制按钮。
//
// 为什么统计压成一行文字：三格卡片要占 60px 高，而它们的信息（请求数、
// 节省量、p50）在有图表之后是次要的——图表已经回答了「效果如何」，
// 这些数字是「顺带看一眼」的量级。腾出来的高度给图表。
func (a *app) footerPane(c *ui.Context, p *glass.Palette) {
	g := a.agg
	saved := g.UpRaw - g.UpWire
	if saved < 0 {
		saved = 0
	}

	ui.Row(c).FillWidth().Padding(0, 16, 12).AlignItems(ui.Center).Gap(10).Children(func() {
		ui.Text(c, a.statsLine(g, saved)).FontSize(11).
			FontFeatures("tnum").TextColor(p.MutedForeground).MaxLines(1).Grow(1)
		if a.actionButton(c, p, "copy", "复制配置", false).Clicked() {
			a.copyEnv()
		}
	})
}

// statsLine 三格统计的一行写法。
//
// 会话累计（不是最近一分钟）：图表说的是最近 60 秒，这一行说的是本次会话，
// 前缀分别写明「最近 60 秒」与「本次会话」，避免两个数字被当成同一件事。
func (a *app) statsLine(g stats.Agg, saved int64) string {
	if g.Requests == 0 {
		// 没跑过的新用户：给上次会话的成绩（如果有），否则给一句引导
		if a.history != nil && a.history.Requests > 0 {
			return a.historyLine(a.history)
		}
		return "本次会话还没有请求"
	}
	return fmt.Sprintf("本次会话 %s 请求 · 节省 %s · p50 %d ms",
		groupDigits(g.Requests), humanBytes(saved), g.P50Ms)
}

// historyLine 上一次会话的成绩。回访用户最想知道「上次效果如何」，
// 空态只说「还没有请求」等于把最有用的信息藏起来了。
func (a *app) historyLine(h *stats.Agg) string {
	saved := h.UpRaw - h.UpWire
	if saved < 0 {
		saved = 0
	}
	return fmt.Sprintf("上次会话 %s 请求 · 节省 %s",
		groupDigits(h.Requests), humanBytes(saved))
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

// noticeLine 一次性操作反馈（复制成功、启动/停止结果）。
//
// 只报操作结果，不重复渲染启动失败原因——那个在状态行里常驻显示过了。
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

// settingsPane 「设置」tab：上游 / 监听 / 传输方式 / 开机自启。
//
// 取代原来可折叠的配置区：折叠区的两层交易（标题行 + 展开面板）在小窗口里
// 要么挤、要么被裁；改成 tab 后主面板不必滚动，设置也不必。
func (a *app) settingsPane(c *ui.Context, p *glass.Palette) {
	running := a.running()

	glass.Surface(ui.Column(c).Padding(4, 14, 8), p, glass.Card).Children(func() {
		a.configField(c, p, "上游地址", &a.server, relayclient.DefaultServer, running)
		a.configRule(c, p)
		a.configField(c, p, "监听地址", &a.listen, relayclient.DefaultListen, running)
		a.configRule(c, p)
		a.transportRow(c, p, running)
		a.configRule(c, p)
		a.autoStartRow(c, p)
	})

	// 运行中不可改：说清「为什么点不动」+「怎么做」，而不是让输入框静静失效
	if running {
		ui.Row(c).AlignItems(ui.Center).Gap(6).Children(func() {
			ui.Text(c, "运行中不可修改，先停止再改").
				FontSize(11).TextColor(p.Warning).MaxLines(1).Grow(1)
			if a.actionButton(c, p, "stop-settings", "停止", false).Clicked() {
				a.stop()
			}
		})
	}
}

// configRule 设置行之间的发丝线
func (a *app) configRule(c *ui.Context, p *glass.Palette) {
	ui.Box(c).Height(1).FillWidth().Background(p.Hairline)
}

// configField 一行「标签 + 输入框」（标签固定 84px，两行字段左边缘对齐）
func (a *app) configField(c *ui.Context, p *glass.Palette, label string, val *string, placeholder string, disabled bool) {
	ui.Row(c).Padding(8, 0).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Width(84).Shrink(0).Children(func() {
			ui.Text(c, label).FontSize(11).TextColor(p.MutedForeground).SingleLine()
		})
		ui.Box(c).Grow(1).Children(func() {
			ui.TextInput(c, val).Placeholder(placeholder).
				FontSize(11).Height(28).Radius(9).Disabled(disabled)
		})
	})
}

// transportRow 传输方式：三段分段控件（设计稿的 seg）
func (a *app) transportRow(c *ui.Context, p *glass.Palette, disabled bool) {
	options := []string{"auto", "quic", "tcp"}
	labels := []string{"自动", "QUIC", "TCP"}
	idx := 0
	for i, o := range options {
		if o == a.transport {
			idx = i
		}
	}
	ui.Row(c).Padding(8, 0).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Width(84).Shrink(0).Children(func() {
			ui.Text(c, "传输方式").FontSize(11).TextColor(p.MutedForeground).SingleLine()
		})
		ui.Box(c).Grow(1).Children(func() {
			ui.Segmented(c, &idx, labels...).FontSize(11.5).Height(26).Disabled(disabled)
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
	ui.Row(c).Padding(8, 0).Gap(12).AlignItems(ui.Center).Children(func() {
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
		// Switch 直接切 a.autoStart；OnChange 在绑定值变化后、重绘之前触发，
		// 所以 applyAutostart 写盘 + 读回的结果会立刻反映到下一个帧；
		// 写失败时它会把 a.autoStart 翻回去，开关自己弹回原位。
		ui.Switch(c, &a.autoStart).OnChange(a.applyAutostart)
	})
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
	ui.Row(c).Height(27).FillWidth().Padding(0, 16).
		AlignItems(ui.Center).Background(p.PageDim).Children(func() {
		ui.Textf(c, "%s · %s", state, a.transportLabel()).
			FontSize(10.5).TextColor(p.Tertiary).SingleLine()
		ui.Box(c).Grow(1)
		ui.Textf(c, "v%s", version).FontSize(9.5).
			Font("monospace").TextColor(p.Tertiary).SingleLine()
	})
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
