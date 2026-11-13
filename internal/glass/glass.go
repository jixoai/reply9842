// Package glass — solid-glass 材质在 mygo native UI 里的复刻
//
// 正交意图（3 个，2026-11-13 kzf 需求：「用 solid-glass + shadcn/ui 风格开发 GUI」）：
//  1. 把 solid-glass 的设计语言（厚度分层、边缘五层光影、一种状态）搬进 native UI
//  2. 用 mygo 的 glass 插件做**真实背景模糊**（不是模拟），玻璃背后真的有内容被抹开
//  3. 给出可直接用的控件封装（Surface / Control / Field / Primary），让 cmd/gui 里
//     只有业务，没有色值
//
// 参考实现：github.com/deeplethe/solid-glass，src/solid-glass.css（1409 行）。
// 它是 shadcn/ui 的 CSS 主题：只定义颜色变量与玻璃材质规则，不是 JS 库。
//
// 四条设计规则（照抄原文的取舍，这是材质统一感的来源）：
//
//  1. The finish is at the edge.  边缘做光影（rim 高光 + hairline + 描边 + 阴影），
//     面板表面绝不用渐变。
//  2. Heavy, not airy.           中性灰；模糊只柔化背后，不给它上色。
//  3. Depth is thickness.        panel（最薄）< card < popover（最厚），越厚越不透明。
//  4. One state.                 hover、选中、展开 用同一片小玻璃。
//
// ## 模糊是真的
//
// mygo v0.9.0 自带 github.com/egoist/mygo/plugins/glass，`glass.Blur{Radius: n}`
// 是一个 ui.Material：由 Metal/HLSL/GLSL 着色器 + CPU 光栅双实现，模糊的是
// **窗口本帧已经画在它下面的像素**。加上 WindowOptions.Vibrancy，桌面也会透进来，
// 于是模糊有真实内容可采。
//
// 对应 CSS 的 `backdrop-filter: blur(26px)`：顺序一致——先模糊背后，再叠元素自己
// 的半透明背景色。所以 frost 的实现是「Blur → 色调」两步，观感与 CSS 相同。
//
// 关于项目毛玻璃标准（AGENTS.md 的 contrast/brightness 固定搭配）：那条规则是为
// 了在 CSS 里用滤镜补偿可读性。native 侧 our tint 自己是中性的，且模糊半径更小，
// 不需要额外的 contrast/brightness 补偿——这里的等效手段是「厚度即不透明度」，
// 见 frost.tint 的取值（卡片 46%、popover 96%）。
package glass

import (
	myglass "github.com/egoist/mygo/plugins/glass"
	"github.com/egoist/mygo/ui"
)

// Palette 一套材质色板（取自 OpenDesign 的 index.html `:root`，数值是 oklch 的 sRGB 换算）
type Palette struct {
	Dark bool

	// 页底与三层厚度（panel 最薄 → popover 最厚）
	//
	// Background 带 alpha：窗口开 Vibrancy 后桌面会透进来，玻璃背后才有东西可模糊。
	// Page 是更深一档的「外壳」色，给 titlebar / footer 这两条 chrome 用。
	Background ui.Color // --bg
	PageDim    ui.Color // --bg-page（比 --bg 更暗一档的外壳色）
	Panel      ui.Color // --glass-panel
	Card       ui.Color // --glass-card
	Popover    ui.Color // --glass-popover

	// 文字三级
	//
	// 注意：这里的取值不等于设计稿的 --text-2 / --text-3。设计稿是在**纯黑**
	// 页面上取色，而本实现为了让 blur 真采得到东西，页面是半透明（桌面会透
	// 8-20%），卡片底色因此比纯黑亮一档（实测 #292929）。同一个灰在这种底上
	// 对比度掉 1.5 倍左右，直接照抄会让 text-3 跌到 3.68:1（低于 4.5 门槛）。
	//
	// 所以三级文字按「最亮的那层底（卡片 #292929）」反解后上调：
	//   Foreground #fafafa 12.9:1 / MutedForeground #a4a4a4 5.8:1 /
	//   Tertiary #949494 4.8:1 —— 三级全部 ≥4.5:1（实测，见报告）。
	Foreground      ui.Color // --text
	MutedForeground ui.Color // --text-2（次级说明）
	Tertiary        ui.Color // --text-3（弱化标签、页脚），比设计稿提亮到 #949494

	// 边框与控件
	Border    ui.Color
	Ring      ui.Color
	Field     ui.Color
	FieldEdge ui.Color

	// 边缘五层（玻璃感的真正来源）
	Rim          ui.Color // 顶部 1px 高光
	Inner        ui.Color // 内圈发丝线
	Edge         ui.Color // 外圈 1px 描边
	Shade        ui.Color // 大扩散阴影
	Contact      ui.Color // 贴近阴影
	Lift         ui.Color // popover 抬升阴影
	PopoverEdge  ui.Color
	PopoverUnder ui.Color

	// shadow-soft 的五个分量（薄面板专用，比 shadow-glass 更弱）
	SoftRim     ui.Color
	SoftInner   ui.Color
	SoftEdge    ui.Color
	SoftContact ui.Color
	SoftShade   ui.Color

	// 细分隔线（--hairline）、窗口顶边高光
	Hairline ui.Color

	// 控件与状态
	Control      ui.Color // 小控件用的清玻璃
	ControlRim   ui.Color
	Hover        ui.Color
	Pressed      ui.Color // 按下态：比 hover 再深一档
	Primary      ui.Color
	PrimaryHover ui.Color
	PrimaryDown  ui.Color
	PrimaryText  ui.Color
	PrimaryRim   ui.Color
	PrimaryUnder ui.Color
	PrimaryInner ui.Color

	// meter（进度条）
	MeterTrack ui.Color
	MeterFill  ui.Color

	// 语义色
	Success ui.Color
	Danger  ui.Color
	Warning ui.Color

	// 图表系列色。三个角色语义固定，不可互换：
	//   UpBar   上行实际发出的字节（蓝）——「成本」
	//   DownBar 下行收到的字节（绿）——「产出」
	//   Latency 延迟折线（琥珀）——「体验」，色相必须与两条流量柱分开，
	//           否则在同一张图里会被读成第三种流量。
	// 独立字段而非直接复用 Success/Danger：暗色与亮色两套板需要不同的明度
	// 补偿（亮底上的绿柱要更深才够对比），复用会让其中一套不可读。
	UpBar   ui.Color
	DownBar ui.Color
	Latency ui.Color
}

// Dark 暗色板：solid-glass 的设计原型（中性灰玻璃）
//
// 每个值都是 `oklch(L C H / A%)` 的 sRGB 换算结果，注释里保留原始 oklch，
// 便于日后对照上游 CSS 核对。
func Dark() *Palette {
	return &Palette{
		Dark: true,

		// 0.13 → #070707。页面是「压暗的纱」，不是实色底板。
		//
		// 取值范围由可读性反推（实测算式，别凭手感调）：
		//   白壁纸(255) 下最终底色 = 255×(1-α) + 9×α
		//   α=0.72 → 底 78，text-2(#a4a4a4) 对比 3.5:1   ✗ 小字不达标
		//   α=0.80 → 底 58，text-2 对比 4.7:1、正文 10.8:1  ✓
		//   α=0.92 → 底 31，但卡片背后几乎是纯色，blur 采不到东西     ✗ 白做模糊
		// 0.80 同时满足这两条，也是「heavy, not airy」的量感上限。
		// 页面几乎不透明：Vibrancy 只负责让窗口边缘透出一点桌面，不参与
		// 「页面本身多亮」的计算。0.80 时浅色壁纸会把页面冲成中灰（实测
		// #353535），导致卡片比页面暗、层深反转。玻璃的厚度感来自卡片
		// 之间的差异，页面必须是一个稳定的深色底。
		Background: ui.RGBA(9, 9, 9, 0.96),    // oklch(0.13 0 0)
		PageDim:    ui.RGBA(4, 4, 4, 0.97),    // oklch(0.085 0 0)，标题栏/底栏
		Card:       ui.RGBA(48, 48, 48, 0.46), // oklch(0.31 0 0 / 46%)
		Popover:    ui.RGBA(24, 24, 24, 0.94), // oklch(0.22 0 0 / 96%)

		Foreground:      ui.Hex("#fafafa"), // oklch(0.985 0 0)
		MutedForeground: ui.Hex("#a4a4a4"), // oklch(0.72 0 0)
		Tertiary:        ui.Hex("#949494"), // 设计稿 0.60 → 提到 0.64，保证卡片底上 4.8:1

		Border:    ui.RGBA(255, 255, 255, 0.11),
		Ring:      ui.Hex("#696969"), // oklch(0.52 0 0)
		Field:     ui.RGBA(0, 0, 0, 0.26),
		FieldEdge: ui.RGBA(255, 255, 255, 0.055),

		Rim:          ui.RGBA(255, 255, 255, 0.16),
		Inner:        ui.RGBA(255, 255, 255, 0.04),
		Edge:         ui.RGBA(255, 255, 255, 0.06),
		Shade:        ui.RGBA(0, 0, 0, 0.60),
		Contact:      ui.RGBA(0, 0, 0, 0.10),
		Lift:         ui.RGBA(0, 0, 0, 0.22),
		PopoverEdge:  ui.RGBA(255, 255, 255, 0.075),
		PopoverUnder: ui.RGBA(0, 0, 0, 0.0),

		SoftRim:     ui.RGBA(255, 255, 255, 0.10),
		SoftInner:   ui.RGBA(255, 255, 255, 0.035),
		SoftEdge:    ui.RGBA(255, 255, 255, 0.045),
		SoftContact: ui.RGBA(0, 0, 0, 0.10),
		SoftShade:   ui.RGBA(0, 0, 0, 0.55),

		Hairline: ui.RGBA(255, 255, 255, 0.055),

		Control:    ui.RGBA(255, 255, 255, 0.05),
		ControlRim: ui.RGBA(255, 255, 255, 0.14),
		Hover:      ui.RGBA(255, 255, 255, 0.085),
		Pressed:    ui.RGBA(255, 255, 255, 0.12),

		Primary:      ui.Hex("#e4e4e4"), // oklch(0.92 0 0)
		PrimaryHover: ui.Hex("#f5f5f5"), // oklch(0.96 0 0)
		PrimaryDown:  ui.Hex("#cfcfcf"), // oklch(0.84 0 0)
		PrimaryText:  ui.Hex("#141414"), // oklch(0.19 0 0)
		PrimaryRim:   ui.RGBA(255, 255, 255, 0.80),
		PrimaryUnder: ui.RGBA(0, 0, 0, 0.45),
		PrimaryInner: ui.RGBA(255, 255, 255, 0.28),

		MeterTrack: ui.RGBA(255, 255, 255, 0.055),
		MeterFill:  ui.RGBA(251, 251, 251, 0.92),

		Success: ui.Hex("#54bf5c"), // oklch(0.72 0.17 145)
		Danger:  ui.Hex("#ed413b"), // oklch(0.63 0.21 27)
		Warning: ui.Hex("#f79c40"), // oklch(0.77 0.15 62)

		// 图表系列（暗底）：蓝/绿/琥珀三色在 #0d0d0d 上的相对亮度接近，
		// 谁也不抢谁的视线；饱和度都压到 0.15 上下，避免在玻璃卡上发荧光。
		UpBar:   ui.Hex("#4c9ef0"), // oklch(0.70 0.14 250) 蓝
		DownBar: ui.Hex("#4ec38a"), // oklch(0.76 0.13 160) 绿
		Latency: ui.Hex("#e0a94a"), // oklch(0.79 0.12 75)  琥珀
	}
}

// Light 亮色板：同一套玻璃在日光下的版本
func Light() *Palette {
	return &Palette{
		Dark: false,

		Background: ui.RGBA(232, 234, 238, 0.86), // oklch(0.925 0.007 265)
		PageDim:    ui.RGBA(240, 242, 246, 0.92),
		Panel:      ui.RGBA(245, 247, 250, 0.44),
		Card:       ui.RGBA(247, 248, 251, 0.60),
		Popover:    ui.RGBA(252, 253, 255, 0.94),

		// 亮色同样是按实测底色反解的：卡片底 #ecedf0（很亮），所以三级文字
		// 都必须比设计稿更深。实测：正文 17:1、次级 5.7:1、弱化 4.8:1。
		Foreground:      ui.Hex("#0d0d0d"), // oklch(0.16 0 0)
		MutedForeground: ui.Hex("#5c5c5c"), // 设计稿 0.47 → 压深，亮卡片底上 5.7:1
		Tertiary:        ui.Hex("#5e5e5e"), // 设计稿 0.58 → 压深，亮底最差处(page) 4.73:1

		Border:    ui.RGBA(41, 46, 56, 0.13),
		Ring:      ui.Hex("#9e9e9e"), // oklch(0.7 0 0)
		Field:     ui.RGBA(255, 255, 255, 0.72),
		FieldEdge: ui.RGBA(41, 46, 56, 0.14),

		Rim:          ui.RGBA(255, 255, 255, 0.90),
		Inner:        ui.RGBA(255, 255, 255, 0.35),
		Edge:         ui.RGBA(59, 66, 83, 0.10),
		Shade:        ui.RGBA(31, 45, 76, 0.28),
		Contact:      ui.RGBA(27, 33, 48, 0.06),
		Lift:         ui.RGBA(27, 33, 48, 0.14),
		PopoverEdge:  ui.RGBA(59, 66, 83, 0.22),
		PopoverUnder: ui.RGBA(59, 66, 83, 0.16),

		SoftRim:     ui.RGBA(255, 255, 255, 0.80),
		SoftInner:   ui.RGBA(255, 255, 255, 0.30),
		SoftEdge:    ui.RGBA(59, 66, 83, 0.09),
		SoftContact: ui.RGBA(27, 33, 48, 0.06),
		SoftShade:   ui.RGBA(31, 45, 76, 0.22),

		Hairline: ui.RGBA(59, 66, 83, 0.10),

		Control:    ui.RGBA(255, 255, 255, 0.55),
		ControlRim: ui.RGBA(255, 255, 255, 0.80),
		Hover:      ui.RGBA(41, 46, 56, 0.10),
		Pressed:    ui.RGBA(41, 46, 56, 0.16),

		Primary:      ui.Hex("#1a1a1a"), // oklch(0.2 0 0)
		PrimaryHover: ui.Hex("#2e2e2e"),
		PrimaryDown:  ui.Hex("#0a0a0a"),
		PrimaryText:  ui.Hex("#fafafa"), // oklch(0.985 0 0)
		PrimaryRim:   ui.RGBA(255, 255, 255, 0.30),
		PrimaryUnder: ui.RGBA(0, 0, 0, 0.45),
		PrimaryInner: ui.RGBA(255, 255, 255, 0.07),

		MeterTrack: ui.RGBA(41, 46, 56, 0.10),
		MeterFill:  ui.RGBA(20, 20, 20, 0.86),

		Success: ui.Hex("#2f9e57"), // 亮底上加深，保证对比度
		Danger:  ui.Hex("#c9342f"),
		Warning: ui.Hex("#b8730f"),

		// 图表系列（亮底）：全部压暗一档，否则白底上会「发光」且对比不足。
		UpBar:   ui.Hex("#2f6fb5"), // oklch(0.55 0.14 250)
		DownBar: ui.Hex("#2f8f63"), // oklch(0.58 0.11 160)
		Latency: ui.Hex("#96681f"), // oklch(0.56 0.10 75)
	}
}

// 圆角：solid-glass 的 --radius 是 0.875rem（14px），控件降一档取 0.7rem（11px）
const (
	RadiusPane    float32 = 14
	RadiusControl float32 = 11
)

// 模糊半径（DIPs）：越厚的面板模糊越强，观感上才有「一层一层叠起来」的纵深。
const (
	BlurPanel   float32 = 12
	BlurCard    float32 = 18
	BlurPopover float32 = 24
)

// Thickness 面板厚度（决定模糊半径、色调与阴影配方）
type Thickness int

const (
	// Thinnest 页面分区：比页面亮一点点，模糊很轻
	Thinnest Thickness = iota
	// Card 卡片：中等，46-60% 不透明
	Card
	// Popover 菜单/弹窗：最厚，96% 不透明
	Popover
)

// frost 是 ui.Material 的实现：真模糊 + 厚度色调。
//
// 顺序与 CSS 的 backdrop-filter 一致——先模糊元素背后的像素，再把元素自己的
// 半透明背景色叠上去。solid 是系统要求「减少透明」时的实色退化值（模糊层不可
// 用时不能留一片可读性不足的半透明）。
type frost struct {
	blur  float32
	tint  ui.Color
	solid ui.Color
}

func (f frost) PaintMaterial(p *ui.Painter, box ui.Rect, radii [4]float32) {
	if box.W <= 0 || box.H <= 0 {
		return
	}
	if p.Preferences().ReduceTransparency {
		p.FillRadii(box, radii, f.solid)
		return
	}
	if f.blur > 0 {
		myglass.Blur{Radius: f.blur}.PaintMaterial(p, box, radii)
	}
	if f.tint.A > 0 {
		p.FillRadii(box, radii, f.tint)
	}
}

// Solid 把一个半透明色压成「叠在页面上之后」的实色，供减少透明时退化使用。
func (p *Palette) overtint(c ui.Color) ui.Color {
	base := ui.Hex("#070707")
	if !p.Dark {
		base = ui.Hex("#e8eaee")
	}
	return c.Over(base)
}

// frostFor 按厚度给出材质
func (p *Palette) frostFor(t Thickness) frost {
	switch t {
	case Thinnest:
		return frost{blur: BlurPanel, tint: p.Panel, solid: p.overtint(p.Panel)}
	case Popover:
		return frost{blur: BlurPopover, tint: p.Popover, solid: p.overtint(p.Popover)}
	default:
		return frost{blur: BlurCard, tint: p.Card, solid: p.overtint(p.Card)}
	}
}

// Surface 按厚度给元素上材质。
//
// 这是整个复刻的核心，两件事叠在一起：
//
//  1. 面板本身：真模糊 + 厚度色调（frost）
//  2. 边缘五层：`inset 0 1px 0 rim` 顶部高光 + `inset 0 0 0 1px inner` 内发丝线
//     + `0 0 0 1px edge` 外描边 + `0 1px 2px contact` 贴近阴影 + 大扩散 shade。
//
// 玻璃「厚」的读法来自第 2 件事，所以它不可省——只模糊不描边会显得像半透明色块。
func Surface(e ui.Element, p *Palette, t Thickness) ui.Element {
	switch t {
	case Thinnest:
		// panel：比页面亮一点点，只有发丝线和描边，没有大阴影（它不浮起来）
		return e.Radius(RadiusPane).
			Material(p.frostFor(Thinnest)).
			InsetShadow(0, 1, 0, 0, p.SoftRim).
			InsetShadow(0, 0, 0, 1, p.SoftInner).
			Shadow(0, 0, 0, 1, p.SoftEdge).
			Shadow(0, 1, 2, 0, p.SoftContact).
			Shadow(0, 10, 26, -16, p.SoftShade)

	case Popover:
		// popover：最厚，抬得最高
		return e.Radius(RadiusPane).
			Material(p.frostFor(Popover)).
			InsetShadow(0, 1, 0, 0, p.Rim).
			InsetShadow(0, -1, 0, 0, p.PopoverUnder).
			Shadow(0, 0, 0, 1, p.PopoverEdge).
			Shadow(0, 24, 60, -18, p.Shade).
			Shadow(0, 6, 16, -6, p.Lift)

	default: // Card
		return e.Radius(RadiusPane).
			Material(p.frostFor(Card)).
			InsetShadow(0, 1, 0, 0, p.Rim).
			InsetShadow(0, 0, 0, 1, p.Inner).
			Shadow(0, 0, 0, 1, p.Edge).
			Shadow(0, 1, 2, 0, p.Contact).
			Shadow(0, 12, 32, -14, p.Shade)
	}
}

// Control 小控件（按钮、字段）的清玻璃：比卡片薄，只有顶部高光与外描边。
// 小控件刻意不模糊——它们的面积小，模糊收益低而每帧代价实在。
func Control(e ui.Element, p *Palette) ui.Element {
	return e.Radius(RadiusControl).
		Background(p.Control).
		InsetShadow(0, 1, 0, 0, p.ControlRim).
		InsetShadow(0, 0, 0, 1, p.Inner).
		Shadow(0, 0, 0, 1, p.Edge).
		Shadow(0, 1, 2, 0, p.Contact)
}

// Field 输入框：一片可以往上打字的清玻璃，顶边有光、四周有发丝线
func Field(e ui.Element, p *Palette) ui.Element {
	return e.Radius(RadiusControl).
		Background(p.Field).
		InsetShadow(0, 1, 2, 0, p.Contact).
		Shadow(0, 0, 0, 1, p.FieldEdge)
}

// Primary 主按钮：一块实心玻璃，顶边受光、底边背光，四周一圈发丝线
func Primary(e ui.Element, p *Palette) ui.Element {
	return e.Radius(RadiusControl).
		Background(p.Primary).
		InsetShadow(0, 1, 0, 0, p.PrimaryRim).
		InsetShadow(0, -1, 0, 0, p.PrimaryUnder).
		InsetShadow(0, 0, 0, 1, p.PrimaryInner).
		Shadow(0, 0, 0, 1, p.PrimaryUnder)
}

// Panel 返回 Thinnest 别名，便于在调用处读作「这是一个面板」。
func Panel() Thickness { return Thinnest }
