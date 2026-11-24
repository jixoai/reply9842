package main

// routes_pane.go — 设置页的「线路」分区。
//
// 正交意图（3 个，owner 2026-11-24 需求：「直连 / 代理 / 中继三种可以混搭，
// 多条线路同时使用，哪条快就多用哪条」）：
//  1. 多条线路可增删改、启停（顺序即优先级，列表顺序会被持久化）
//  2. 每条线路显示**实时质量**（预计完成时间 / 带宽 / 成功率 / 连续失败）
//  3. 跨 server 的线路要显式标注「基线不共享」——那是用户必须知道的代价
//
// 二八空间法则：最高频的动作是「加一条线路」和「开关某条」，放成按钮与行内
// 开关；低频的 kind/server/proxy/transport 编辑收进展开区，不让列表被输入框淹没。

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/jixoai/reply9842/internal/glass"
	"github.com/jixoai/reply9842/internal/relayclient"
	"github.com/jixoai/reply9842/internal/route"
)

// routesSection 渲染整段线路管理（嵌在设置页「连接」组里）。
func (a *app) routesSection(c *ui.Context, p *glass.Palette, running bool) {
	// 折叠态（默认）：一行概览 + 「展开/收起」。见 app.routesExpanded 的注释。
	// 概览给三件事：几条、几条启用、当前最快的是哪条（后者是用户最想一眼看到的）。
	n := len(a.routesDraft)
	enabled := 0
	for _, r := range a.routesDraft {
		if r.IsEnabled() {
			enabled++
		}
	}
	summary := fmt.Sprintf("%d 条线路（%d 启用）", n, enabled)
	if best := a.fastestRouteID(); best != "" {
		summary += " · 最快 " + best
	}

	ui.Row(c).Padding(6, 0).AlignItems(ui.Center).Gap(6).Children(func() {
		ui.Text(c, "线路").FontSize(11).TextColor(p.MutedForeground).Width(40).Shrink(0)
		ui.Text(c, summary).FontSize(11).TextColor(p.Tertiary).Grow(1).SingleLine()
		// 用「折叠」而不是「收起」：行内的编辑按钮在编辑态也叫「收起」，
		// 两者作用域完全不同（这个收整段分区、那个收编辑区），同名会误点。
		// 三个动作三个词：折叠（分区）/ 收起（编辑区）/ 完成（编辑区底部）。
		label := "展开"
		if a.routesExpanded {
			label = "折叠"
		}
		if a.actionButton(c, p, "routes-toggle", label, false).Clicked() {
			a.routesExpanded = !a.routesExpanded
			a.invalidate()
		}
	})

	if !a.routesExpanded {
		return // 折叠态到此为止，不占更多纵向空间
	}

	// 运行中改线路不会立刻生效（Runner 的 SetConfig 在运行中拒绝，
	// 与同页其它连接类字段的语义一致）。必须明说，否则用户点了开关、
	// 落盘成功、质量列还在跳，却不知道要「停止→启动」才生效。
	// （对抗审查 P3-6a。）
	if running {
		ui.Row(c).Padding(6, 0).AlignItems(ui.Center).Gap(6).Children(func() {
			ui.Text(c, "线路改动在下次启动时生效（运行中请先停止再改）").
				FontSize(10).TextColor(p.Warning).MaxLines(2).Grow(1)
		})
	}
	// 跨 server 提示：只在真的指向不同 server 时出现。
	// 用户必须知道「跨 server 切换会偶发一次全量重发」这个代价。
	if servers := a.routesDraft.Servers(); len(servers) > 1 {
		ui.Row(c).Padding(6, 0).AlignItems(ui.Center).Gap(6).Children(func() {
			ui.Text(c, fmt.Sprintf("这些线路指向 %d 台不同的服务器：delta 基线不共享，跨服务器切换时会偶发一次全量重发",
				len(servers))).
				FontSize(10).TextColor(p.Tertiary).MaxLines(2).Grow(1)
		})
	}

	// 展开态：列表放进**固定高度的内滚动容器**。
	//
	// 为什么需要它（视觉复核的阻塞项「展开态把下面所有设置项推出屏外、
	// 且裁在控件中间」）：外层设置页本来就可滚动，但展开一段长列表会把
	// 「其他」组整体推下去，用户看到的是「页面突然变长」。内滚动把展开
	// 的影响限制在 listMaxHeight 之内——无论多少条线路，多占的空间恒定。
	//
	// 高度换算：每行 41px（按钮 31 + 上下 padding），4 行可见 = 164px，
	// 再多就滚。选 4 行而不是 3 行：默认的三条线路不该一展开就要滚。
	const listMaxHeight = 4 * 41
	ui.Scroll(c).MaxHeight(listMaxHeight).KeepScrollbars(true).Children(func() {
		// 右侧留 14px 给滚动条 thumb：KeepScrollbars 的 thumb 是**覆盖**在
		// 内容上的，不留位就会压住最右侧「删除」按钮的圆角与边框
		// （视觉复核实测：9px 宽的半透明竖条连续穿过两行按钮右缘）。
		ui.Column(c).Padding(0, 14).Children(func() {
			for i := range a.routesDraft {
				a.routeRow(c, p, i, running)
				a.configRule(c, p)
			}
		})
	})

	// 编辑区**放在滚动容器之外**，不跟在行后面。
	//
	// 为什么（视觉复核的阻塞项）：编辑区跟在行内时，它和行一起受 listMaxHeight
	// 的视口约束——在最后一行展开会从视口底边被切断，第一个输入框只剩半个
	// （缺底边框、字形少 2px），用户看不到完整字段，也判断不出还有几项。
	// 放到列表下方后高度不受限；归属靠下面那行「正在编辑 XXX」承担，比
	// 「只靠按钮从编辑变收起」明确得多。
	if idx := a.editingRoute(); idx >= 0 && idx < len(a.routesDraft) {
		a.routeEditor(c, p, idx, running)
	}

	if a.routeErr != nil {
		ui.Row(c).Padding(6, 0).Children(func() {
			ui.Text(c, "线路配置有误："+a.routeErr.Error()).
				FontSize(11).TextColor(p.DangerText).MaxLines(2).Grow(1)
		})
	}

	// 添加线路：三个按钮（三种类型各一个），比「点添加再选类型」少一步。
	ui.Row(c).Padding(6, 0).AlignItems(ui.Center).Gap(6).Children(func() {
		ui.Text(c, "添加").FontSize(11).TextColor(p.MutedForeground).Width(40).Shrink(0)
		for _, k := range route.Kinds {
			k := k
			if a.actionButton(c, p, "route-add-"+string(k), "＋"+route.KindLabels[k], false).Clicked() {
				a.addRoute(k)
			}
		}
	})
}

// routeRow 单条线路：**单行**（状态点 + 名称 + 类型 + 质量 + 启停 + 删除）。
//
// 为什么是单行（视觉复核后的重构）：
// 最初每行占两行（摘要 + 后端取值小字），三条线路就吃掉 159px，把设置页
// 从 v1 的整体下推 183px、底部 6 行挤出可视区。而「后端取值」这类低频信息
// 完全可以收进质量列同行（一份列表里同一行读起来更省视线移动）。
// 单行后每条 26px，三条共 78px——把大部分高度还给了既有设置项。
//
// 状态表达（视觉复核的阻塞项）：停用线路必须有**可辨的视觉差**，否则
// 「已停用」只存在于配置里、界面上看不出来。三重承载：
//  1. 行首状态点（绿=启用 / 灰=停用）
//  2. 名称颜色（启用=Foreground / 停用=Tertiary，整行降权）
//  3. 主按钮标签随状态切换（启用中显示「停用」，反之「启用」）
func (a *app) routeRow(c *ui.Context, p *glass.Palette, idx int, running bool) {
	r := a.routesDraft[idx]
	key := fmt.Sprintf("r%d", idx)
	on := r.IsEnabled()

	// 停用行的整体降权色
	nameColor := p.Foreground
	dotColor := p.Success
	muteColor := p.MutedForeground
	if !on {
		nameColor = p.Tertiary
		dotColor = p.Tertiary
		muteColor = p.Tertiary
	}

	ui.Row(c).Padding(6, 0).AlignItems(ui.Center).Gap(6).Children(func() {
		// 状态点：9px 圆点，绿=启用 / 灰=停用
		ui.Box(c).Width(9).Height(9).Shrink(0).Background(dotColor).Radius(5)

		// 名称
		ui.Text(c, r.ID).FontSize(11).TextColor(nameColor).Width(80).SingleLine()

		// 类型标签
		ui.Text(c, route.KindLabels[r.Kind]).FontSize(9.5).
			TextColor(muteColor).Width(26).Shrink(0)

		// 质量 + 后端取值合并成一列（低频信息不必独占一行）
		ui.Text(c, a.routeSummaryText(r)).FontSize(9.5).TextColor(p.Tertiary).
			Grow(1).SingleLine()

		// 启停：标签随状态切换（这是页面上唯一表达「可操作方向」的文字）
		btnLabel := "停用"
		if !on {
			btnLabel = "启用"
		}
		if a.actionButton(c, p, key+"-on", btnLabel, false).Clicked() {
			a.routesDraft[idx] = a.routesDraft[idx].SetEnabled(!on)
			if err := a.saveSettings(); err != nil {
				a.setNotice(humanError(err), true)
			}
			a.invalidate()
		}
		// 编辑：展开/收起这条线路的字段编辑区。
		//
		// 为什么必须有它（自查发现的死路）：线路的可配置字段里，中继线路的
		// 「证书域名」与「源地址」都是**必填**——不填就 x509 失败或 TUN 下
		// tls: EOF，实测两条都踩过。而此前 GUI 只提供增删启停，用户新增一条
		// 中继线路后没有任何地方能填这两项，等于「加了就废」。
		editLabel := "编辑"
		if a.editingRoute() == idx {
			editLabel = "收起"
		}
		if a.actionButton(c, p, key+"-edit", editLabel, running).Clicked() {
			if a.editingRoute() == idx {
				a.setEditingRoute(-1)
			} else {
				a.setEditingRoute(idx)
			}
			a.invalidate()
		}
		// 删除：用危险色文字区分「破坏性操作」与「启停」。
		//
		// 视觉复核指出：两者曾完全同构（同宽、同填充、同描边），页脚那条
		// 橙色警告反而是全页唯一的危险色信号。破坏性动作不该靠位置猜。
		if ui.Button(c.Key(key+"-del"), "删除").
			FontSize(11.5).FontWeight(520).Padding(0, 12).Height(28).
			Radius(glass.RadiusControl).TextColor(p.DangerText).
			InsetShadow(0, 1, 0, 0, p.ControlRim).
			InsetShadow(0, 0, 0, 1, ui.RGBA(255, 255, 255, 0.045)).
			Shadow(0, 0, 0, 1, ui.RGBA(255, 255, 255, 0.055)).
			Clicked() {
			a.removeRoute(idx)
		}
	})
}

// routeSummaryText 一行里同时给出「质量」与「连哪儿」。
//
// 优先级：有实时质量就以质量为主（那是用户决策依据），后端取值缩成尾注。
// 没有质量时（未运行/新线路）退化成纯后端描述——总比显示「未采集」占满
// 整整一列更有信息量。
func (a *app) routeSummaryText(r route.Route) string {
	var parts []string

	// 质量（若已采集）
	if q := a.routeQualityText(r.ID); q != "" && q != "未采集" && q != "已停用" {
		parts = append(parts, q)
	}

	// 「连哪儿」只在**没有质量数据**时显示。
	//
	// 为什么不在有数据时也显示（视觉复核的阻塞项「设置页被撑高」）：
	// 每行塞进 server + 代理 + 源地址 + 传输后，文字会占满整列并把右缘
	// 顶到按钮上，行高与行距被迫放大。而这一列的首要用途是「这条现在
	// 快不快」，server 是低频信息——它已经在同一行的类型标签与「网络」
	// 区表达过了（同 server 多线路是最常见形态，重复显示没有信息量）。
	//
	// 兜底用途仍然保留：未运行时没有质量可显示，此时列出 server 至少让
	// 用户确认「这条线路指向哪儿」。
	loc := r.Server
	if r.Proxy != "" {
		loc += "　经 " + r.Proxy
	}
	if r.LocalAddr != "" {
		loc += "　绑 " + r.LocalAddr
	}
	if r.Transport != "" && r.Transport != "auto" {
		loc += "　" + strings.ToUpper(r.Transport)
	}
	if len(parts) == 0 {
		parts = append(parts, "→ "+loc)
	}

	return strings.Join(parts, "  ·  ")
}

// routeQualityText 把一条线路的实时快照压成一行可读文字。
//
// 口径与用户关心的问题对齐（owner：「最终一个请求传输完成的时间总耗时」）：
// 主指标是 1 MB 请求的预计完成时间，其次是带宽与成功率。
// 没有数据时显示「未采集」而不是 0——"0 ms" 会被误读成极快。
func (a *app) routeQualityText(id string) string {
	for _, s := range a.routeSnaps {
		if s.ID != id {
			continue
		}
		if !s.Enabled {
			return "已停用"
		}
		if !s.Known || s.Est1MB <= 0 {
			if s.Samples > 0 {
				return fmt.Sprintf("采样中（%d 条）", s.Samples)
			}
			return "未采集"
		}
		parts := []string{fmt.Sprintf("1MB 约 %.1fs", float64(s.Est1MB)/1000)}
		if s.BandwidthKBps > 0 {
			parts = append(parts, fmt.Sprintf("%.0f KB/s", s.BandwidthKBps))
		}
		if s.SuccessRate >= 0 {
			parts = append(parts, fmt.Sprintf("成功 %.0f%%", s.SuccessRate*100))
		}
		if s.ConsecFails > 0 {
			parts = append(parts, fmt.Sprintf("连续失败 %d", s.ConsecFails))
		}
		// 「承载量」= 本次运行中被选中多少次（规格审查 F7 要求可见）。
		// 放最后：它是过程量，前三项才是决策依据。
		if s.Picked > 0 {
			parts = append(parts, fmt.Sprintf("承载 %d 次", s.Picked))
		}
		return strings.Join(parts, " · ")
	}
	return "未采集"
}

// addRoute 追加一条新线路（用该类型的合理默认值，用户随后可改）。
//
// 默认值的选择：direct 用全局 server（绝大多数场景）；proxy 给一个本机常见
// 端口当提示；relay 必须填**另一台** server，所以先留空——故意让它校验失败，
// 把「请填另一台服务器」明确说出来，比悄悄复制一个同地址的线路更诚实。
func (a *app) addRoute(k route.Kind) {
	n := len(a.routesDraft) + 1
	r := route.Route{
		ID:   fmt.Sprintf("%s%d", route.KindLabels[k], n),
		Kind: k,
		// 必须过 NormalizeTarget（对抗审查 P3-5a）：全局「目标机器」框允许
		// 用户填 https://host 这种形态，而 route.Validate 只认 host:port。
		// 直接复制原文会在 start() 时被「线路配置有误」拦死，且提示不指向根因。
		Server: relayclient.NormalizeTarget(strings.TrimSpace(a.server)),
	}
	switch k {
	case route.KindProxy:
		r.Proxy = "http://127.0.0.1:7890"
	case route.KindRelay:
		r.Server = ""
	}
	a.routesDraft = append(a.routesDraft, r)
	if err := a.saveSettings(); err != nil {
		a.setNotice(humanError(err), true)
	}
	a.invalidate()
}

// removeRoute 删掉一条线路。
//
// 不允许删到 0 条：没有线路的客户端无法启动，而「删光后才发现起不来」
// 是个糟糕的交互终局。保留最后一条并提示。
func (a *app) removeRoute(idx int) {
	if len(a.routesDraft) <= 1 {
		a.setNotice("至少保留一条线路（没有线路就连不上了）", true)
		return
	}
	if idx < 0 || idx >= len(a.routesDraft) {
		return
	}
	a.routesDraft = append(a.routesDraft[:idx], a.routesDraft[idx+1:]...)
	// 编辑态跟着移动：删的正是被编辑那条 → 收起；删的是它前面的 → 下标左移。
	// 不做这一步，用户删掉上一条后编辑区会突然变成另一条线路的字段。
	switch cur := a.editingRoute(); {
	case cur == idx:
		a.setEditingRoute(-1)
	case cur > idx:
		a.setEditingRoute(cur - 1)
	}
	if err := a.saveSettings(); err != nil {
		a.setNotice(humanError(err), true)
	}
	a.invalidate()
}

// refreshRouteSnaps 从 Runner 拉一次线路实时质量（每秒心跳里调用）。
//
// 注意：只在 Runner 真的有数据时才覆盖。未运行时 Routes() 返回 nil，
// 无条件覆盖会抹掉测试/截图注入的演示数据（实测导致官网截图里三条线路
// 全显示「未采集」——视觉复核抓到的阻塞项之一）。
func (a *app) refreshRouteSnaps() {
	if a.runner == nil {
		return
	}
	if snaps := a.runner.Routes(); len(snaps) > 0 {
		a.routeSnaps = snaps
	}
}

// syncNetModeToRoutes 把「网络」快捷设置的结果反映到线路列表。
//
// 存在的理由（对抗审查 P2-5）：设置页上有两个能表达「从哪出去」的入口——
// 上方的「网络」三选一，和下方的线路列表。它们若互不联动，用户切到「代理」
// 并填好 URL 后，界面说「已走代理」，实际 cfg 里没有任何 proxy 线路，仍然
// 走直连——在 TUN 环境下这就是「怎么都不通」的死循环。
//
// 语义：netMode 是**对第一条线路的意图表达**，不是另一套配置：
//
//	direct → 第一条线路变 direct，带上探测到的源地址
//	proxy  → 第一条线路变 proxy，带上 proxyURL
//	system → 第一条线路变 direct，清掉源地址（交给系统路由）
//
// 只动第一条（用户的其余线路是显式配置，不该被快捷设置覆盖）。
func (a *app) syncNetModeToRoutes() {
	if len(a.routesDraft) == 0 {
		return
	}
	r := a.routesDraft[0]
	switch a.netMode {
	case relayclient.NetworkDirect:
		r.Kind = route.KindDirect
		r.LocalAddr = a.detectedIP // 可能为空（探测失败时让 Dial 退回系统路由）
		r.Proxy = ""
	case relayclient.NetworkProxy:
		r.Kind = route.KindProxy
		r.Proxy = strings.TrimSpace(a.proxyURL)
		r.LocalAddr = ""
	case relayclient.NetworkSystem:
		r.Kind = route.KindDirect
		r.LocalAddr = ""
		r.Proxy = ""
	}
	a.routesDraft[0] = r
	if err := a.saveSettings(); err != nil {
		a.setNotice(humanError(err), true)
	}
}

// fastestRouteID 当前预计耗时最短的线路（折叠态概览用）。
//
// 判据用 Est1MB（1 MB 的预计完成时间）——与用户关心的「传完要多久」同口径。
// 没有样本时返回空串（不假装知道哪条快）。
func (a *app) fastestRouteID() string {
	best := ""
	var bestEst int64
	for _, s := range a.routeSnaps {
		if !s.Enabled || !s.Known || s.Est1MB <= 0 {
			continue
		}
		if best == "" || s.Est1MB < bestEst {
			best, bestEst = s.ID, s.Est1MB
		}
	}
	return best
}

// routeEditor 一条线路的字段编辑区（渲染在列表**下方**，不在滚动容器内）。
//
// 为什么在列表下方而不是贴着被编辑那行（视觉复核的阻塞项）：贴着行时它和行
// 一起受 listMaxHeight 的视口约束，在最后一行展开会被视口底边横向切断——
// 第一个输入框只剩半个、看不到完整字段，也判断不出后面还有几项。
//
// 归属表达：顶部一行「正在编辑「名称」」，而不是只靠按钮从「编辑」变「收起」。
// 字段按 kind 显示，不做「全部列出 + 灰掉不适用」：灰掉的字段会让用户以为
// 「只是暂时不能改」，而 direct 线路填代理地址会被 Validate 直接判非法。
//
// 网格：标签 Width(84) + 输入框 Grow(1)，Padding(6,0)，与页面上其它
// configField 完全同列（视觉复核实测过编辑区曾左右各错 12px）。
func (a *app) routeEditor(c *ui.Context, p *glass.Palette, idx int, running bool) {
	r := &a.routesDraft[idx]

	save := func() {
		if err := a.saveSettings(); err != nil {
			a.setNotice(humanError(err), true)
		}
		a.invalidate()
	}

	field := func(label, placeholder string, val *string) {
		ui.Row(c).Padding(6, 0).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Box(c).Width(84).Shrink(0).Children(func() {
				ui.Text(c, label).FontSize(11).TextColor(p.MutedForeground).SingleLine()
			})
			ui.Box(c).Grow(1).Children(func() {
				ui.TextInput(c, val).Placeholder(placeholder).
					FontSize(11).Height(26).Radius(9).Disabled(running).
					OnChange(save)
			})
		})
	}

	// 归属行：明确「这些框改的是哪一条」，并在同一行给出关闭入口。
	ui.Row(c).Padding(6, 0).AlignItems(ui.Center).Gap(6).Children(func() {
		ui.Text(c, "正在编辑「"+r.ID+"」").FontSize(11).TextColor(p.Foreground).Grow(1).SingleLine()
		if a.actionButton(c, p, "route-editor-close", "完成", false).Clicked() {
			a.setEditingRoute(-1)
			a.invalidate()
		}
	})

	field("名称", "如 香港 / 上海", &r.ID)
	field("服务器", "host:port", &r.Server)

	// 证书域名：中继指向另一台 IP 时的必填项。域名目标下 SNI 就是域名本身，
	// 多一个字段只会让人疑惑，所以只在需要时出现。
	if r.Kind == route.KindRelay || relayclient.SNINeeded(r.Server) {
		field("证书域名", "证书上的域名，如 "+relayclient.DefaultServerName, &r.ServerName)
		if r.Kind == route.KindRelay && r.ServerName == "" &&
			relayclient.SNINeeded(r.Server) {
			// 指向 IP 却留空 SNI：能用但一定失败，提前说清而不是等它 x509。
			ui.Row(c).Padding(6, 0).Children(func() {
				ui.Text(c, "指向 IP 的中继必须填证书域名，否则证书校验必失败").
					FontSize(10).TextColor(p.Warning).MaxLines(2).Grow(1)
			})
		}
	}

	if r.Kind == route.KindProxy {
		field("代理地址", "http://127.0.0.1:7890", &r.Proxy)
	}

	// 源地址：direct 与 relay 都适用（中继的第一跳仍是本机外连），
	// proxy 不适用（CONNECT 由代理进程发起）。
	if r.Kind != route.KindProxy {
		field("源地址", "留空＝系统路由；TUN 环境填物理网卡 IP", &r.LocalAddr)
	}

	if running {
		ui.Row(c).Padding(6, 0).Children(func() {
			ui.Text(c, "运行中不可编辑，停止后可改").
				FontSize(10).TextColor(p.Tertiary).Grow(1)
		})
	}
}

// editingRoute 返回正在编辑的线路下标（-1 表示没有）。
func (a *app) editingRoute() int { return a.editRouteIdx - 1 }

// setEditingRoute 设置编辑态；传 -1 表示收起。
func (a *app) setEditingRoute(idx int) { a.editRouteIdx = idx + 1 }
