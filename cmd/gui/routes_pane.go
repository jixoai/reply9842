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
	"context"
	"fmt"
	"net"
	"strings"
	"time"

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
	// 草稿期间的摘要补充。
	//
	// 「列表已收起」只在**真的**收起时说：早期这里强制收起，于是出现
	// 「摘要说已收起、右边按钮还写折叠」的同屏矛盾（视觉终验 P1）。
	// 现在改为在 beginAddRoute 里**收起一次**，之后用户可自由展开/折叠，
	// 文案因此必须跟着真实状态走。
	if a.pendingRoute.ID != "" {
		summary += " · 新建中"
		if !a.routesExpanded {
			summary += "（列表已收起）"
		}
		// 草稿态不渲染 applyRow（列表折叠时「上面这些线路」没有指代对象），
		// 但状态不能凭空消失——否则用户取消草稿后才发现改动还在等应用。
		// 这里顺带说明入口什么时候回来（视觉终验 P3）。
		if a.routesDirty() {
			// 用「展开后」而不是「收起草稿后」：屏幕上的按钮就叫「展开」，
			// 指令与控件同词；同时省约 4 个汉字宽，避免末尾被省略号吃掉
			// （580px 固定宽度下这段文案只差 25-30px 就能完整显示）。
			summary += " · 有改动待应用（展开后可应用）"
		}
	}

	ui.Row(c).Padding(6, 0).AlignItems(ui.Center).Gap(6).Children(func() {
		ui.Text(c, "线路").FontSize(11).TextColor(p.MutedForeground).Width(40).Shrink(0)
		ui.Text(c, summary).FontSize(11).TextColor(p.Tertiary).Grow(1).SingleLine()
		// 展开/折叠：雪佛龙图标 + 旋转语义（朝下=可展开，朝上=可折叠）。
		//
		// 这个动作只出现一处，本可以留文字；但它是**纯方向**语义，图标比
		// 「展开/折叠」两个词更省视线，而且它在列表右上角、与三个行内图标
		// 同一视觉族。可访问名仍然给全（「展开线路列表」）。
		label := "展开线路列表"
		if a.routesExpanded {
			label = "折叠线路列表"
		}
		tint := p.MutedForeground
		if a.routesExpanded {
			tint = p.Foreground
		}
		// iconChevron 画的是「朝下」；展开时用 Scale 翻转成朝上，
		// 而不是再维护一个 iconChevronUp（两个 SVG 容易画得不对称）。
		e := a.iconButton(c, p, "routes-toggle", iconChevron, tint, label, false)
		if a.routesExpanded {
			e = e.Scale(1, -1)
		}
		if e.Clicked() {
			a.routesExpanded = !a.routesExpanded
			a.invalidate()
		}
	})

	// 草稿期间列表**默认收起**（由 beginAddRoute 收起一次），但用户可以再展开。
	//
	// 为什么不强制收起（视觉终验 P1）：强制会产生同屏矛盾——摘要说「列表已
	// 收起」，右边按钮还写着「折叠」，点它既不像折叠也不像展开。用户想看
	// 现有线路再决定加什么，是合理需求，不该被锁死。
	//
	// 高度问题由「默认收起」解决就够了：草稿表单约 200px + 展开列表 123px
	// 会让动作行压在折线上（视觉复验 N1 实测被切 4px）；默认收起后动作行
	// 离 footer 约 160px。用户主动展开则是他自己的选择，页面本身可滚动。
	// 注意：折叠只跳过**列表**，绝不能在这里 return——草稿编辑区与添加按钮
	// 都在后面，早期版本在折叠时直接 return，导致「点了＋代理，列表收起，
	// 然后草稿表单根本没渲染」（视觉第四轮用像素差实测抓到：草稿态与折叠态
	// 只差摘要行 1747 px，表单完全不在屏上）。这是「测试只看摘要文案、
	// 没看表单是否存在」放过去的缺陷。
	showList := a.routesExpanded

	// 运行中改线路不会立刻生效（Runner 的 SetConfig 在运行中拒绝，
	// 与同页其它连接类字段的语义一致）。必须明说，否则用户点了开关、
	// 落盘成功、质量列还在跳，却不知道要「停止→启动」才生效。
	// （对抗审查 P3-6a。）

	if showList {
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
	} // showList

	// 「有改动未生效」+ 立即应用：放在列表与编辑区之间，紧挨着被改的东西。
	a.routesApplyRow(c, p)

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

	// 草稿编辑区：正在新建的那条线路（在添加按钮**上方**，因为它属于
	// 「即将发生的事」，而按钮是触发它的入口——先看到内容再看到按钮，
	// 阅读顺序才顺）。
	if a.pendingRoute.ID != "" {
		a.pendingRouteEditor(c, p, running)
	}

	// 添加线路：三个按钮（三种类型各一个），比「点添加再选类型」少一步。
	//
	// 草稿期间**整行不渲染**（不是禁用）：草稿已经占据一块表单高度，再摆三个
	// 点不动的按钮只会把页面底部内容推出可视区——视觉实测过「三枚按钮被 footer
	// 切断、只露 6px」。同屏也不该有两个「新建中的线路」，「取消」会指代不明。
	if a.pendingRoute.ID == "" {
		ui.Row(c).Padding(6, 0).AlignItems(ui.Center).Gap(6).Children(func() {
			ui.Text(c, "添加").FontSize(11).TextColor(p.MutedForeground).Width(40).Shrink(0)
			for _, k := range route.Kinds {
				k := k
				if a.actionButton(c, p, "route-add-"+string(k),
					"＋"+route.KindLabels[k], false, false).Clicked() {
					a.beginAddRoute(k)
				}
			}
		})
	}
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

	// Gap(6)：三个图标按钮之间的间隙与应用其它按钮组保持一致（视觉实测
	// 行内只有 4px，比下方「＋直连/＋代理/＋中继」的 6px 紧，低精度指针
	// 下易误触）。
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

		// 三个动作图标成一小组：内层 Gap(8) 让**可见**间隙达到 6px
		// （图标按钮的 1px 描边各吃掉 1px，外层 Gap(6) 实测只剩 4px，
		// 比应用其它按钮组的可见 6px 紧）。外层 Row 的 Gap 不动，
		// 免得连带拉开状态点/名称/质量列的间距。
		ui.Row(c).AlignItems(ui.Center).Gap(8).Shrink(0).Children(func() {
			// 启停：电源图标，**颜色即状态**。
			//
			// 为什么必须带状态样式（owner 2026-11-25：「线路的启用和停用，图标是
			// 完全一样的，这虽然没问题，但是不是应该带状态样式呢」）：原先两态只差
			// #a4a4a4 → #949494，在 1x 下几乎不可辨——那等于按钮不反映状态，
			// 用户要靠行首那个 9px 圆点去判断，还得先知道圆点的颜色含义。
			//
			// 用绿/灰而不是换形状（电源 vs 播放）：形状混用会让人拿不准「这个
			// 按钮当前是开还是关」；颜色是 toggle 类按钮最通用的状态约定，
			// 并且与行首状态点同色（绿=在跑），两处说法一致。
			// 图标表达「当前状态」，可访问名表达「点击后会发生什么」——两者
			// 各司其职，不冲突。
			onTint := p.Success
			if !on {
				onTint = p.Tertiary
			}
			onLabel := "停用这条线路"
			if !on {
				onLabel = "启用这条线路"
			}
			if a.iconButton(c, p, key+"-on", iconPower, onTint, onLabel, false).Clicked() {
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
			editLabel := "编辑这条线路的字段"
			editTint := p.MutedForeground
			if a.editingRoute() == idx {
				editLabel = "收起字段编辑区"
				// 正在编辑时点亮：图标不能换文字，就用颜色表达「已打开」。
				editTint = p.Foreground
			}
			if a.iconButton(c, p, key+"-edit", iconEdit, editTint, editLabel, false).Clicked() {
				if a.editingRoute() == idx {
					a.setEditingRoute(-1)
				} else {
					a.setEditingRoute(idx)
				}
				a.invalidate()
			}
			// 删除：图标 + 危险色。危险色不能省——垃圾桶的通用性足够表达
			// 「破坏性」，但同色系的中性图标会和左边两个混成一片，红色是这一行里
			// 唯一的「手别抖」信号（视觉复核曾指出破坏性动作不该靠位置猜）。
			if a.iconButton(c, p, key+"-del", iconTrash, p.DangerText, "删除这条线路", false).Clicked() {
				a.removeRoute(idx)
			}
		})
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

// beginAddRoute 进入「新建线路」草稿态（不落盘、不参与择路）。
//
// 为什么不再直接 append（owner 2026-11-24）：「添加代理时你就直接用 7890
// 端口了，我都不能编辑表单然后测试线路最后再确认添加？」
//
// 旧实现点一下按钮就 append 一条带默认值的线路并立刻写盘：用户没机会改
// 代理端口（硬编码 7890，而多数人的代理并不在 7890），也没机会验证，只能
// 先加进去再看它失败——而失败时界面只说「未采集」，不指向根因。
//
// 草稿态把「配置」与「生效」分开：编辑 → 测试 → 添加 / 取消。
func (a *app) beginAddRoute(k route.Kind) {
	n := len(a.routesDraft) + 1
	// 草稿的 ID 要避开已有名称：用户可能删过中间的线路，"代理2" 仍被占用。
	id := fmt.Sprintf("%s%d", route.KindLabels[k], n)
	for a.routeIDTaken(id) {
		n++
		id = fmt.Sprintf("%s%d", route.KindLabels[k], n)
	}

	r := route.Route{
		ID:   id,
		Kind: k,
		// 服务器默认继承全局目标（绝大多数场景就是同一台），用户可改。
		Server: relayclient.NormalizeTarget(strings.TrimSpace(a.server)),
	}
	switch k {
	case route.KindProxy:
		// 不预设端口：真实代理端口五花八门（7890/1080/1081/8888…），
		// 写死一个反而让用户误以为「已经填好了」。占位符给常见示例即可。
		r.Proxy = ""
	case route.KindRelay:
		// 中继必须指向**另一台**，留空逼用户显式填，比悄悄复制同地址诚实。
		r.Server = ""
		if a.detectedIP != "" {
			r.LocalAddr = a.detectedIP
		}
	}
	// 继承「网络」快捷设置探测到的源地址（TUN 环境下非绑不可）。
	if k != route.KindProxy && r.LocalAddr == "" && a.detectedIP != "" {
		r.LocalAddr = a.detectedIP
	}

	a.pendingRoute = r
	// routeProbe.reset 作废可能仍在飞的上一次探测（gen 递增让回调丢弃结果）
	a.pendingProbe = routeProbe{gen: a.pendingProbe.gen + 1}
	// 新建时顺手作废编辑区的探测：进草稿会收起编辑区，那份结果已经没有
	// 展示位了，留着只会在下次进编辑区时冒出一个过期结论。
	a.editProbe = routeProbe{gen: a.editProbe.gen + 1}
	// 新建时收起其它线路的编辑区，避免同屏两套输入框。
	a.setEditingRoute(-1)
	// 收起列表**一次**（不是强制锁定）：草稿表单约 200px，与展开的列表
	// 同时在场会把动作行压到视口折线上（视觉实测被切 4px）。之后用户想
	// 展开看现有线路仍然可以——那时代码里没有任何强制折叠。
	a.routesExpanded = false
	a.invalidate()
}

// routeIDTaken 判断某个线路名是否已被占用（草稿提交前的重名检查）。
func (a *app) routeIDTaken(id string) bool {
	for _, r := range a.routesDraft {
		if r.ID == id {
			return true
		}
	}
	return false
}

// commitPendingRoute 把草稿提交进列表并落盘。
//
// 校验在这里做而不是在 start()：用户点「添加」时就该知道这条线路合不合法，
// 而不是等到启动时看到一句「线路配置有误」——那时他已经离开这个页面了。
func (a *app) commitPendingRoute() {
	r := a.pendingRoute.Normalize()
	if err := r.Validate(); err != nil {
		a.setNotice("这条线路还不能添加："+err.Error(), true)
		return
	}
	if a.routeIDTaken(r.ID) {
		a.setNotice(fmt.Sprintf("已有一条线路叫「%s」，换个名字", r.ID), true)
		return
	}
	a.routesDraft = append(a.routesDraft, r)
	if err := a.saveSettings(); err != nil {
		a.setNotice(humanError(err), true)
	}
	a.pendingRoute = route.Route{}
	a.pendingProbe = routeProbe{gen: a.pendingProbe.gen + 1}
	a.invalidate()
}

// cancelPendingRoute 放弃草稿（不落盘）。
func (a *app) cancelPendingRoute() {
	a.pendingRoute = route.Route{}
	a.pendingProbe = routeProbe{gen: a.pendingProbe.gen + 1}
	a.invalidate()
}

// testPendingRoute 用草稿当前字段真实拨号一次。
//
// 异步执行：拨号最坏要等满一个超时（QUIC 回退 TCP 时更久），阻塞主线程会让
// 整个窗口卡死。复用 update.go 的 runAsync/runMain 模式，并用世代号丢弃
// 过期结果（用户可能连点、或测到一半就取消了）。
func (a *app) testPendingRoute() {
	if a.pendingProbe.testing || a.pendingRoute.ID == "" {
		return
	}
	r := a.pendingRoute.Normalize()
	if err := r.Validate(); err != nil {
		a.setNotice("先把这个填对再测："+err.Error(), true)
		return
	}
	a.probeAsync(&a.pendingProbe, r)
}

// testEditingRoute 用编辑区里的当前字段（可能还没应用）真实拨号一次。
//
// owner 2026-11-25：「修改线路的时候，不能测试线路」。改已有线路比新建更该
// 能测——改坏了影响的是正在用的那条，而新建最坏只是多一条坏线路。
//
// 测的是**界面上这份**（routesDraft[idx]，含尚未应用的改动），不是 Runner
// 正在跑的那份：用户想知道的正是「我这样改之后通不通」。
// ProbeRoute 会新建独立连接，不会干扰正在服务的连接。
func (a *app) testEditingRoute() {
	idx := a.editingRoute()
	if idx < 0 || idx >= len(a.routesDraft) {
		return
	}
	if a.editProbe.testing {
		return
	}
	r := a.routesDraft[idx].Normalize()
	if err := r.Validate(); err != nil {
		a.setNotice("先把这个填对再测："+err.Error(), true)
		return
	}
	a.probeAsync(&a.editProbe, r)
}

// probeAsync 探测一条线路并回填状态（草稿与编辑区共用）。
//
// p 是指针：回调必须写回**发起时那个**状态槽。若中途用户切到另一处
// （改草稿 / 收起草稿），对应槽位的 gen 已经递增，回调比对失败后丢弃结果。
func (a *app) probeAsync(p *routeProbe, r route.Route) {
	gen := p.start()
	insecure := a.insecure
	a.invalidate()

	a.runAsync(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res, err := relayclient.ProbeRoute(ctx, r, insecure)
		a.runMain(func() {
			if gen != p.gen {
				return // 过期：字段已改 / 已取消 / 又点了一次
			}
			p.testing = false
			if err != nil {
				p.err = err
			} else {
				p.result = &res
			}
			a.invalidate()
		})
	})
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
		// 字段一改，上一次的「测通了」就不再成立（那是对改之前那份配置的
		// 结论）。作废它，否则用户会以为新字段也验证过了。
		a.editProbe.reset()
		a.invalidate()
	}

	// 归属行：明确「这些框改的是哪一条」，并在同一行给出关闭入口。
	ui.Row(c).Padding(6, 0).AlignItems(ui.Center).Gap(6).Children(func() {
		ui.Text(c, "正在编辑「"+r.ID+"」").FontSize(11).TextColor(p.Foreground).Grow(1).SingleLine()
		if a.actionButton(c, p, "route-editor-close", "完成", false, false).Clicked() {
			a.setEditingRoute(-1)
			a.invalidate()
		}
	})

	// 始终可编辑（owner 2026-11-24：「我不觉得线路必须停止才能编辑」）。
	// 运行中改的是「待应用」配置，点上面的「立即应用」才交给 Runner。
	a.routeFields(c, p, r, false, save)

	// 测试连接（owner 2026-11-25：「修改线路的时候，不能测试线路」）。
	// 测的是界面上这份（含尚未应用的改动）——用户要知道的是「这样改之后
	// 通不通」。ProbeRoute 新建独立连接，不干扰正在服务的连接。
	a.probeRow(c, p, "route-edit-test", &a.editProbe, *r,
		a.testEditingRoute, "建议先测一次再应用")

	if running {
		ui.Row(c).Padding(6, 0).Children(func() {
			ui.Text(c, "改完点「立即应用」生效（隧道会短暂重启）").
				FontSize(10).TextColor(p.Tertiary).Grow(1)
		})
	}
}

// routeFields 渲染一条线路的全部可编辑字段（按 kind 决定显示哪些）。
//
// 抽成独立函数是为了让「编辑已有线路」与「新建草稿」共用同一套布局：
// 两处若各写一份，字段顺序、标签宽度、占位符文案迟早会漂移，用户会看到
// 「新建时能填的项，添加后就找不到了」。
//
// disabled 与 onChange 由调用方决定：编辑态是「运行中禁用 + 每次改动落盘」，
// 草稿态是「永远可编辑 + 只改内存不落盘」。
func (a *app) routeFields(c *ui.Context, p *glass.Palette,
	r *route.Route, disabled bool, onChange func()) {

	field := func(label, placeholder string, val *string) {
		ui.Row(c).Padding(6, 0).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Box(c).Width(84).Shrink(0).Children(func() {
				ui.Text(c, label).FontSize(11).TextColor(p.MutedForeground).SingleLine()
			})
			ui.Box(c).Grow(1).Children(func() {
				ui.TextInput(c, val).Placeholder(placeholder).
					FontSize(11).Height(26).Radius(9).Disabled(disabled).
					OnChange(onChange)
			})
		})
	}

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
		field("代理地址", "如 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080", &r.Proxy)
	}

	// 源地址：direct 与 relay 都适用（中继的第一跳仍是本机外连），
	// proxy 不适用（CONNECT 由代理进程发起）。
	if r.Kind != route.KindProxy {
		field("源地址", "留空＝系统路由；TUN 环境填物理网卡 IP", &r.LocalAddr)
	}
}

// pendingRouteEditor 新建线路的草稿编辑区（含测试连接与添加/取消）。
func (a *app) pendingRouteEditor(c *ui.Context, p *glass.Palette, running bool) {
	r := &a.pendingRoute

	// 草稿的每次字段改动都要作废上一次探测结果：
	// 「测通了」是针对**改之前**那份配置的结论，改了地址还显示通过会误导。
	onEdit := func() {
		a.pendingProbe.reset()
		a.invalidate()
	}

	// 标题独占一行，不再重复放「取消」：视觉指出顶部与底部各一个取消，
	// 同页两个同名动作且都能点，会让用户犹豫「这两个一样吗」。
	//
	// 字色用 MutedForeground 而不是 Foreground（视觉 nit N3）：原来它和分区
	// 标题「连接/其他」同字号同色同缩进，读起来像是第三个平级分区；它其实是
	// 「线路」这个分区内部的一块。降一档色并加一条上方分隔线来表达从属。
	ui.Row(c).Padding(6, 0).AlignItems(ui.Center).Gap(6).Children(func() {
		ui.Text(c, "新建"+route.KindLabels[r.Kind]+"线路").
			FontSize(10.5).TextColor(p.MutedForeground).Grow(1).SingleLine()
	})

	a.routeFields(c, p, r, false, onEdit)

	// 测试连接 + 结果（与编辑区共用同一套渲染，见 probeRow）。
	//
	// 只有「测过」才允许添加吗？不强制——用户可能在别处已验证过，或只是想
	// 先把配置存下来。强制测试会把「添加」变成有副作用的动作（要联网）。
	a.probeRow(c, p, "route-pending-test", &a.pendingProbe, *r,
		a.testPendingRoute, "建议先测一次再添加")

	// 「添加」是这一屏唯一的主操作。校验不过时**不置灰**，而是退回描边样式
	// 并说明缺什么：
	//
	// 为什么不用 Disabled（视觉复验的阻塞项 N2）：mygo 的 PrimaryButton 禁用态
	// 渲染成 #7F7F7F 实心块（对卡片底 4.30:1），是草稿表单里除主胶囊外最亮的
	// 大面积色块——禁用的控件反而比可用的「取消」抢眼，权重完全反了；
	// 配 #494949 的字只有 2.25:1，像「卡住的按钮」而不是「还差一步」。
	//
	// 改用「描边形态 + 说明文字」，既保持了对比度，又把「缺什么」直接说出来。
	canCommit := r.Normalize().Validate() == nil
	if !canCommit {
		ui.Row(c).Padding(6, 0).Children(func() {
			ui.Text(c, "还需填写："+pendingRouteMissing(*r)).
				FontSize(10).TextColor(p.Warning).MaxLines(2).Grow(1)
		})
	}
	ui.Row(c).Padding(6, 0).AlignItems(ui.Center).Gap(8).Children(func() {
		ui.Box(c).Grow(1)
		if a.actionButton(c, p, "route-pending-cancel", "取消", false, false).Clicked() {
			a.cancelPendingRoute()
		}
		if canCommit {
			if a.actionButton(c, p, "route-pending-commit", "添加", true, false).Clicked() {
				a.commitPendingRoute()
			}
		} else {
			// 描边 + 禁用：视觉上明确「这一步还没到」，且不会变成一个亮块。
			a.actionButton(c, p, "route-pending-commit", "添加", false, true)
		}
	})
}

// editingRoute 返回正在编辑的线路下标（-1 表示没有）。
func (a *app) editingRoute() int { return a.editRouteIdx - 1 }

// setEditingRoute 设置编辑态；传 -1 表示收起。
func (a *app) setEditingRoute(idx int) { a.editRouteIdx = idx + 1 }

// routesDirty 当前编辑是否与 Runner 正在用的配置不一致。
//
// 未运行时恒为 false：没有「已应用的配置」这回事，改动在下次启动时自然生效，
// 提示「有改动未生效」只会制造焦虑（用户没在跑，没有东西需要生效）。
func (a *app) routesDirty() bool {
	if a.runner == nil || !a.running() {
		return false
	}
	return !a.routesDraft.Normalize().Equal(a.appliedRoutes)
}

// applyRoutes 把当前线路配置交给 Runner 并立刻生效（运行中会优雅重启）。
//
// owner 2026-11-24：「编辑完是落入一个未应用的配置中，最后实现一键生效。
// 如果是运行中的就直接接纳新的配置」。
//
// 异步执行：重启内含 Shutdown（等在途请求，5 秒上限）+ 重新监听，阻塞主线程
// 会让窗口在最长 5 秒里完全无响应。
func (a *app) applyRoutes() {
	if a.applying || a.runner == nil {
		return
	}
	rs := a.routesDraft.Normalize()
	if err := rs.Validate(); err != nil {
		a.setNotice("线路配置有误，未应用："+err.Error(), true)
		return
	}

	cfg := a.runner.Config()
	cfg.Routes = rs
	// 全局单值字段保留（它们仍参与 SNI 继承等默认值推导），但线路是真相源。
	a.applying = true
	a.applyErr = nil
	a.invalidate()

	a.runAsync(func() {
		restarted, err := a.runner.ApplyConfig(cfg)
		a.runMain(func() {
			a.applying = false
			if err != nil {
				a.applyErr = err
				// 失败时**不更新** appliedRoutes：界面继续显示「有改动未生效」，
				// 用户能看到自己的改动还在，可以再试或改回去。
				a.setNotice("应用失败："+humanError(err), true)
				a.invalidate()
				return
			}
			a.appliedRoutes = rs
			if restarted {
				a.setNotice("已应用新线路配置（隧道已重启，在途请求会重连）", false)
			} else {
				a.setNotice("已保存线路配置，下次启动生效", false)
			}
			a.invalidate()
		})
	})
}

// revertRoutes 放弃未应用的改动，回到 Runner 正在跑的那份配置。
//
// 为什么需要（owner 2026-11-25：「立即应用旁边是不是应该给一个取消呢」）：
// 「有改动未生效」是个需要收尾的状态，而收尾有两个方向——接受或放弃。
// 只给「立即应用」的话，想反悔的用户只能手动把每个字段改回去，或者去翻
// settings.json；那是把「撤销」的成本推给了人。
//
// 命名用「放弃改动」而不是「取消」：草稿流程里已经有一个「取消」
// （取消新建），同页两个「取消」会让人分不清是在撤销什么。
func (a *app) revertRoutes() {
	if len(a.appliedRoutes) == 0 {
		return
	}
	// 深拷贝：直接赋值会让两个切片共享底层数组，之后编辑 routesDraft
	// 会静默改到 appliedRoutes，dirty 判断从此永远为假。
	a.routesDraft = append(route.Set(nil), a.appliedRoutes...)
	a.setEditingRoute(-1)
	if err := a.saveSettings(); err != nil {
		a.setNotice(humanError(err), true)
	}
	a.applyErr = nil
	a.invalidate()
}

// routesApplyRow 「有改动未生效」提示 + 一键应用按钮。
//
// 只在真的 dirty 时出现：常驻一行「没有改动」既占高度又没有信息量
// （列表密度是本页的既有硬约束，视觉复核为此打过回）。
func (a *app) routesApplyRow(c *ui.Context, p *glass.Palette) {
	// 草稿态一律不显示（视觉复验 M4 的归属歧义）：这时列表是折叠的，
	// 「上面这些线路」没有指代对象；用户注意力在草稿上，两个「进行中的事」
	// 并排只会让人分不清哪句说的是什么。状态改由摘要行携带（见 routesSection）。
	if a.pendingRoute.ID != "" {
		return
	}
	if !a.routesDirty() && a.applyErr == nil && !a.applying {
		return
	}
	ui.Row(c).Padding(6, 0).AlignItems(ui.Center).Gap(8).Children(func() {
		switch {
		case a.applying:
			ui.Text(c, "正在应用（重启隧道）…").FontSize(10).
				TextColor(p.Tertiary).Grow(1).SingleLine()
		case a.applyErr != nil:
			ui.Text(c, "应用失败："+humanError(a.applyErr)).FontSize(10).
				TextColor(p.DangerText).MaxLines(2).Grow(1)
		default:
			// 明确写「上面这些线路」而不是只写「线路有改动」：这一行夹在列表
			// 与新建草稿之间，笼统的措辞会被读成「草稿也有问题」（视觉验收
			// 指出的归属歧义）。草稿是尚未添加的东西，不该被算进「未生效」。
			ui.Text(c, "上面这些线路有改动，尚未生效").FontSize(10).
				TextColor(p.Warning).Grow(1).SingleLine()
		}
		// 放弃入口用次级描边：它有破坏性（丢掉用户刚做的修改），
		// 但不像「删除线路」那样不可逆（配置还在，改回去就行），
		// 所以不套危险色，只用比「立即应用」低一档的权重。
		if !a.applying &&
			a.actionButton(c, p, "routes-revert", "放弃改动", false, false).Clicked() {
			a.revertRoutes()
		}
		label := "立即应用"
		if a.applying {
			label = "应用中…"
		}
		if a.actionButton(c, p, "routes-apply", label, true, a.applying).Clicked() {
			a.applyRoutes()
		}
	})
}

// pendingRouteMissing 用一句话说清草稿还缺哪些必填项。
//
// 存在的理由：把「添加」画成灰按钮只能说「不能点」，说不出「为什么」。
// 用户填了一半的表单最需要的是这句话，而不是一个点不动的按钮。
func pendingRouteMissing(r route.Route) string {
	n := r.Normalize()
	var missing []string
	if strings.TrimSpace(r.Server) == "" {
		missing = append(missing, "服务器地址")
	} else if _, _, err := net.SplitHostPort(n.Server); err != nil {
		missing = append(missing, "服务器端口的写法（host:port）")
	}
	if r.Kind == route.KindProxy {
		if strings.TrimSpace(r.Proxy) == "" {
			missing = append(missing, "代理地址")
		} else if !strings.Contains(r.Proxy, "://") {
			missing = append(missing, "代理地址的协议头（http:// / socks5://）")
		}
	}
	if n.Kind == route.KindRelay && n.Server != "" && relayclient.SNINeeded(n.Server) &&
		strings.TrimSpace(r.ServerName) == "" {
		missing = append(missing, "证书域名（目标为 IP 时必须填）")
	}
	if len(missing) == 0 {
		return "线路名称（不能与现有线路重名）"
	}
	return strings.Join(missing, "、")
}

// probeRow 「测试连接」按钮 + 结果/提示一行（草稿与编辑区共用）。
//
// 抽出来的理由：两处的三段式状态机完全一样（测试中 / 失败 / 成功），
// 各写一份迟早分叉——尤其是「校验不过时按钮置灰 + 提示改口径」这种细节。
//
// hint 是「还没测」时的引导语：草稿说「建议先测一次再添加」，编辑区说
// 「建议先测一次再应用」——它要指向各自的下一个动作。
func (a *app) probeRow(c *ui.Context, p *glass.Palette, key string,
	probe *routeProbe, r route.Route, onTest func(), hint string) {

	// 校验不过时也置灰：字段没填全，拨号必然失败，让它可点只会白等一个超时。
	canProbe := r.Normalize().Validate() == nil
	ui.Row(c).Padding(6, 0).AlignItems(ui.Center).Gap(8).Children(func() {
		label := "测试连接"
		if probe.testing {
			label = "测试中…"
		}
		if a.actionButton(c, p, key, label, false, probe.testing || !canProbe).Clicked() {
			onTest()
		}
		switch {
		case probe.testing:
			ui.Text(c, "正在拨号…").FontSize(10).
				TextColor(p.Tertiary).Grow(1).SingleLine()
		case probe.err != nil:
			ui.Text(c, probe.err.Error()).FontSize(10).
				TextColor(p.DangerText).MaxLines(3).Grow(1)
		case probe.result != nil:
			pr := probe.result
			txt := fmt.Sprintf("通了 · 握手 %d ms · 往返 %d ms · %s",
				pr.HandshakeMS, pr.RTTMS, strings.ToUpper(pr.Transport))
			if pr.Dcz2 {
				txt += " · dcz2"
			}
			ui.Text(c, txt).FontSize(10).TextColor(p.Success).Grow(1).SingleLine()
		case !canProbe:
			// 按钮此时是禁用的，再写「建议先测一次」就是在提示一个当前
			// 做不到的动作。改成说清前置条件。
			ui.Text(c, "填完必填项后可测试").FontSize(10).
				TextColor(p.Tertiary).Grow(1).SingleLine()
		default:
			ui.Text(c, hint).FontSize(10).
				TextColor(p.Tertiary).Grow(1).SingleLine()
		}
	})
}
