package glass

import (
	"github.com/egoist/mygo/ui"
)

// Theme 把色板转成 mygo 的 Theme：控件（按钮、输入框、Select、滚动条）
// 会自动取这里的 Surface/Border/Accent，所以只要设了 Theme，整个窗口的
// 基础控件就都是这套材质，不必逐个重画。
//
// 这里刻意让 Surface 比 Card 更暗一点：solid-glass 的控件是「一片薄玻璃贴在
// 厚玻璃上」，如果 Surface 与 Card 同色就分不出层次。
//
// AccentHover / AccentPressed 必须真的不同：三态全等的话主按钮按下去毫无反馈
// （用户分不清点击是否生效），这是本文件存在感最强的一处约束。
func (p *Palette) Theme() *ui.Theme {
	t := &ui.Theme{
		Dark:            p.Dark,
		Background:      p.Background,
		Surface:         p.Control,
		SurfaceHover:    p.Hover,
		SurfacePressed:  p.Pressed,
		Border:          p.Border,
		Text:            p.Foreground,
		TextMuted:       p.MutedForeground,
		Accent:          p.Primary,
		AccentHover:     p.PrimaryHover,
		AccentPressed:   p.PrimaryDown,
		AccentText:      p.PrimaryText,
		InactiveAccent:  p.Primary,
		Danger:          p.Danger,
		Warning:         p.Warning,
		Success:         p.Success,
		Selection:       p.Hover,
		Focus:           p.Ring,
		FocusRingWidth:  2,
		FocusRingOffset: 1,
		Radius:          RadiusControl,
		Spacing:         4,
		FontSize:        12,
		// 滚动条：设计稿的 thumb 是 14% 白，轨道全透明
		Scrollbar:      p.Foreground.Alpha(0.14),
		ScrollbarWidth: 9,
	}
	// 提示气泡用「翻过来的」一套：暗色底下用亮底黑字，和 solid-glass 的 popover 一致
	if p.Dark {
		t.Inverse = ui.RGBA(235, 235, 235, 1)
		t.InverseText = ui.Hex("#121212")
	} else {
		t.Inverse = ui.Hex("#1b1b1b")
		t.InverseText = ui.Hex("#fafafa")
	}
	return t
}

// Page 页面底板。
//
// 半透明（带 alpha）是有意的：窗口开 Vibrancy 后桌面会透进来，上层卡片
// 的模糊才有真实内容可采。纯色底板会让所有 blur 退化成「模糊一块纯色」。
func (p *Palette) Page(c *ui.Context) ui.Element {
	return ui.Column(c).Fill().Background(p.Background)
}

// Heading 区块标题（设计稿的 eyebrow：600 / letter-spacing .12em）。
//
// 字号 11 而非设计稿的 10：设计稿的 eyebrow 是英文大写，中文在小字号下笔画
// 糊成一团（9.5-10px 的中文在 Retina 上都发虚）。这是中文的一处硬约束，
// 凡中文一律不低于 11px；数字与拉丁字母仍可保持设计稿的小字号。
func Heading(c *ui.Context, p *Palette, text string) ui.Element {
	return ui.Text(c, text).FontSize(11).FontWeight(600).LetterSpacing(1.1).TextColor(p.MutedForeground)
}

// Label 弱化标签（设计稿的 stat-label：text-3、小字号；中文取 11px 下限）
func Label(c *ui.Context, p *Palette, text string) ui.Element {
	return ui.Text(c, text).FontSize(11).LetterSpacing(0.5).TextColor(p.Tertiary)
}

// Value 数字（tabular-nums 防跳动 + 负字距收紧，设计稿的 stat-value）
func Value(c *ui.Context, p *Palette, text string, size float32) ui.Element {
	return ui.Text(c, text).FontSize(size).FontWeight(550).
		LetterSpacing(-size * 0.015).FontFeatures("tnum").TextColor(p.Foreground)
}

// Separate 一条发丝线分隔（solid-glass 里没有显式 divider，用 sub-panel 的
// 边缘光影代替；这里给配置区之间用一条极淡的线）
func Separate(c *ui.Context, p *Palette) ui.Element {
	return ui.Box(c).Height(1).Background(p.Hairline)
}

// VSeparate 竖发丝线（stats 三格之间的分隔）
func VSeparate(c *ui.Context, p *Palette) ui.Element {
	return ui.Box(c).Width(1).Background(p.Hairline.Alpha(1.2))
}
