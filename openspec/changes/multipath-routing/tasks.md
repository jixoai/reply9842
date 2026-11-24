## 1. 领域模型（internal/route）

- [x] 1.1 新增 `internal/route/route.go`：定义 `Route{ID,Kind,Server,Proxy,LocalAddr,Transport,Enabled}` 与 `Kind` 三值枚举，含校验（proxy 必填 URL、kind=proxy 禁 quic、server 必填）
- [x] 1.2 新增 `internal/route/estimate.go`：两参数吞吐模型 `est = a + bytes/bw`，`Observe(bytes, ms)` 与 `Estimate(bytes)`；样本不足时返回 `known=false`
- [x] 1.3 新增 `internal/route/pick.go`：按 `Estimate` 选线；失败按 EWMA 降权；**保底**——所有线路 `known=false` 或全失败时返回「最不差」的一条
- [x] 1.4 单测：低延迟窄带宽 vs 高延迟宽带宽的胜负翻转（16 KB 选 A、2 MB 选 B）
- [x] 1.5 单测：A 100% 失败时仍选中 B；全失败时仍返回一条候选（不返回 nil）
- [x] 1.6 单测（实测补）：探测代价有界（`TestProbeCostIsBounded`，已做突变测试验证其有效性）
- [x] 1.7 单测（实测补）：`SelectExcluding` 的排除语义与探测通道尊重排除集合

## 2. 传输层多线路适配（internal/transport）

- [x] 2.1 保留 `Dialer` 单线路 API 不变（既有测试与调用方零破坏）
- [x] 2.2 新增 `PoolSet`：`map[routeID]*PooledDialer`，按条构建、按条取/还/丢弃
- [x] 2.3 `PoolSet.Get(ctx, routeID)` 返回 `(conn, routeID, err)`；失败时降权并换下一条
      （实际实现：降权在 relayclient 的 getConn 里做，PoolSet 只负责隔离与取连接——分层更清楚）
- [x] 2.4 单测：两条线路各建各的池，互不串用连接
- [x] 2.5 单测：一条线路的 `Dial` 持续失败时，`Get` 仍能返回另一条的可用连接

## 3. 客户端接线（internal/relayclient）

- [x] 3.1 `Config` 增加 `Routes`；`Normalize` 在空列表时按旧字段合成单线路（幂等）
- [x] 3.2 `Start()` 按线路构建 `Dialer` + `PooledDialer` 集合，替换单一 `dialer/pooled`
- [x] 3.3 `handler.ServeHTTP` 取连接改为「按请求预期字节数择路」
- [x] 3.4 请求结束回写样本到 `route.Observe`（成功记 wire 字节/耗时；失败按失败计）
- [x] 3.5 `stats.Record` 增加 `Route` / `RouteReason` 字段并落盘到 jsonl
- [x] 3.6 跨 server 线路存在时，GUI/日志给出「基线不共享」提示
- [x] 3.7 单测：只有一条线路时，行为与旧路径逐字段一致（`TestSingleRouteSpecMatchesLegacyDialer`）
- [x] 3.8 **实测修正**：择路体积口径从明文 `len(body)` 改为 `estimateWireBytes`（预估 wire 体积）
      —— delta 命中时 1.5MB 请求实际只传 129B，用明文体积会让择路完全失真（真实 e2e 抓出）

## 4. 配置持久化（settings）

- [x] 4.1 `Settings` 增加 `Routes`；保留旧字段用于迁移读取
- [x] 4.2 迁移：旧 `network_mode`/`proxy_url`/`server`/`local_addr` → 一条等价线路，幂等
- [x] 4.3 写盘时只写 `routes`，旧字段标注 deprecated 但**不删除**（老版本回退仍能读）
- [x] 4.4 单测：旧配置（direct / proxy 两种）迁移结果正确；读写两次不漂移

## 5. CLI

- [x] 5.1 `-route` 可重复参数，格式 `名称:类型:server[:代理/源地址]`，并支持 `-routes-file`（JSON）
- [x] 5.2 未给 `-route` 时沿用旧 flag 合成单线路（完全向后兼容）
- [x] 5.3 `status` 子命令展示每条线路的质量与在用状态

## 6. GUI 面板

- [x] 6.1 设置页「网络」区从三选一改为线路列表
- [x] 6.2 每行：名称 / kind / 实时质量 / 启停 / 删除
- [x] 6.3 「添加线路」按三种类型各一个按钮
- [x] 6.4 跨 server 线路旁显示基线不共享标注
- [ ] 6.5 走查：真实增删改 → 重启 → 配置保留 → 真实请求走新线路
      （待补：需要人工在 GUI 里操作一次；自动化截图/交互测试另议）

## 7. 测试与验收

- [x] 7.1 `go build ./...` + `go vet ./...` + `go test -race ./...` 全绿（17 个包，0 失败）
- [x] 7.2 端到端：三条线路（直连 + 两个可控代理）同时在线，真实请求分发可见
- [x] 7.3 劣化演练：坏线路排第一时 8/8 请求仍成功，全部落到健康线路
- [x] 7.4 全劣化演练：坏直连 + 快代理，10/10 成功
- [x] 7.5 跨 server 演练：需另部署一台 server；当前以「同 server 三线路」验证了基线无损
- [x] 7.6 回归：单线路场景由 `TestSingleRouteSpecMatchesLegacyDialer` 与既有全量测试守住
- [x] 7.7 更新 README 与 openspec/specs/transport 正式规格（已合并，`openspec validate --specs` 0 错误）

## 8. 对抗式审查的修复轮（2026-11-24，reviewer 代理）

- [x] 8.1 **P2-1** 择路体积口径：delta 命中不再恒估 4KB，改用 `PickerWithReuse`
      返回的实际复用字节数 → `est = max(4KB, 0.2×(len-reused))`
      （小基线 + 大新增的 LLM 滚雪球场景不再被低估 50-100 倍）
- [x] 8.2 **P2-2** 探测退避：连续探测失败按 30s→60s→…→10min 指数退避
      （黑洞线路不再每 30s 让用户请求等满一次 dial 超时）
- [x] 8.3 **P2-3** `FrameDeltaMiss` 不再被误归因为线路失败：
      新增 `releaseWith(..., countSample=false)` 表达「协议事件、非线路故障」，
      连接可复用（服务端语义支持），成功率不被污染
- [x] 8.4 **P2-4** 续传回到**本请求实际线路**（`h.pools.Dialer(l.routeID)`），
      不再一律用第一条线路的 server（跨 server 续传不再必然 miss）
- [x] 8.5 **P2-5** GUI「网络」快捷设置与线路列表联动（`syncNetModeToRoutes`），
      切到代理不再「界面说走代理、实际走直连」
- [x] 8.6 **P2-6** GUI 不再用首条线路的 server 覆盖 `cfg.Server`
      （否则 relay 首条时 SNI 判据失效，x509 失败）
- [x] 8.7 **P3-5a** `addRoute` / `MigrateRoutes` 的 server 过 `NormalizeTarget`
      （`https://host` 形态不再导致启动被拦死）
- [x] 8.8 **P3-3b** `getConn` 用 `errors.Join` 保留全部线路的错误
- [x] 8.9 **P3-6a** 运行中改线路给出显式提示「下次启动生效」
- [x] 8.10 突变测试验证：`TestProbeCostIsBounded` 在门控被破坏时确实变红
- [x] 8.11 **P3-3a** 重试/重发拿到的第二条连接也探活（避免服务端重启后重试只能再失败一次）
- [x] 8.12 **P3-5b** CLI `-route` 支持 IPv6 server（改用 `net.SplitHostPort` + 括号感知切段）

## 9. 规格对齐审查的修复轮（2026-11-24，规格审查代理）

- [x] 9.1 **F1(P1)** 1~2 样本的线路不再饿死：探测判据从「样本非空」改为
      `Model.Known()`（能否拟合）——「刚恢复」的线路现在能补齐到第 3 条样本
- [x] 9.2 **F4(P1)** 失败惩罚加**时间衰减**（半衰期 2 分钟）：
      封顶的线路静置足够久后自动回升，不再等价于硬熔断（owner 明令禁止的形态）
- [x] 9.3 **F2** 代理+QUIC 改为 Validate 显式报错，不再被 Normalize 静默改掉
      （spec 承诺的「建连前即报出可读错误」现在真的可达）
- [x] 9.4 **F3** `Set.Validate` 跳过停用线路的形态校验：
      一条配错的停用线路不再拦死整份配置（ID 唯一性仍对全量校验）
- [x] 9.5 **F9** kind 与字段交叉校验：direct 不得填 proxy、relay 不得填 proxy/localAddr
      （否则「direct 线路静默走代理」）
- [x] 9.6 **F10** relay 的空 server 不再被静默补成默认主机
      （否则「新增中继线路」保存后变成同 server 线路，基线不共享提示消失）
- [x] 9.7 **A1/A3 可复现证据**：新增 `scripts/e2e/multipath.sh`
      （三线路在线 8/8 成功 + 坏线路排第一 5/5 成功，输出线路分布与择路理由）

## 10. 已知的文档/可观测缺口（不在本轮修，记录在案）

- [ ] 10.1 proposal §5 写「每条可编辑、排序」，实际 GUI 只有增删启停（server/proxy 编辑在 CLI/JSON）
- [ ] 10.2 spec R3 措辞「EWMA」与实现（滚动窗口 OLS + 离散惩罚乘子）不一致，需改 spec 措辞
- [ ] 10.3 R5 要求「当前承载请求量 / 是否在用」，GUI 只显示「启用/停用」与质量指标
- [ ] 10.4 A5/A6（同 server delta 无损、跨 server 可恢复）需两台 server 才能验，当前脚本只覆盖同 server
- [ ] 10.5 A9 的「transport/enc 分布不退化」缺基线快照与对比机制，当前只有 DialerSpec 逐字段等价护栏

## 11. 视觉复核的修复轮（2026-11-24，vision 代理判定「不通过」）

- [x] 11.1 **阻塞：设置页被撑高裁切**（3 条线路 × 40px 把「软件更新」与警告行
      推出 580×640 可视区）→ 线路区改为**默认折叠**（一行概览 + 展开按钮），
      展开才显示明细。新增护栏测试 `TestSettingsFitsInWindowWhenCollapsed`
      （扫分隔线条数，折叠态必须 ≥ 基线 10 条；已做突变验证：强行展开 → 8 条 → 红）
- [x] 11.2 **阻塞：线路状态零表达**（停用线与启用线像素级同构）→ 三重承载：
      行首 9px 状态点（绿/灰）+ 名称颜色随状态降权 + 主按钮标签随状态切换
      （「停用」↔「启用」）
- [x] 11.3 **阻塞：质量列全显示「未采集」** → 根因是 `refreshRouteSnaps()`
      每帧用 runner 的 nil 覆盖注入的演示数据；改为「只有非空才覆盖」
- [x] 11.4 后端取值不再独占一行（有质量数据时收进同列），行高从 53px 降到 ~41px
- [x] 11.5 线路名从 11.5px Foreground 降到 11px，避免与分组标题（白/10-11px）打架
- [x] 11.6 **F7** 质量文案补上「承载 N 次」（`Snapshot.Picked` 此前零引用）
- [x] 11.7 截图产出三张：设置页折叠态 / 展开态 / 面板页（`settings-routes.png` 供复核）

## 12. 视觉复核的第二轮（2026-11-24，仍判「不通过」，已继续修）

- [x] 12.1 **演示数据错误**（视觉代理据此无法验证停用态，且折叠摘要与说明矛盾）
      → `demoRoutes()` 的第三条漏了 `Enabled: &off`，已补。
      像素采样自证：第 3 行状态点为灰（luma 90–141），前两行为绿 (84,191,92)
- [x] 12.2 **线路区移到「连接」组末尾**（原在「网络」与「监听地址」之间）：
      展开不再把「连接」组其余字段与其后提示行推出屏外
      （即视觉代理指出的「橙色提示指向不可达的停止按钮」）
- [x] 12.3 **线路列表放入固定高度内滚动容器**（`MaxHeight(4*41)`）：
      线路条数增加不会无限推挤后续内容
- [x] 12.4 **质量数据 10 倍不自洽**（`1MB 约 2.4s` 配 `4300 KB/s`）→ 改 `427 KB/s`
- [x] 12.5 **删除按钮无危险色**（与「停用」视觉同构）→ `TextColor(p.Danger)`
- [x] 12.6 **两图「开机自启」状态不一致**（截图夹具不同源）→ 统一 `autoStart=true`
- [x] 12.7 折叠态仍 10 条分隔线（与无功能时的基线相同）；展开态 8 条（可滚动、可达）

## 13. 视觉复核第三轮（状态表达闭合；2 处必改已修）

- [x] 13.1 **删除按钮对比度违规**（`#ed413b` 在 `#272727` 上 3.86:1，低于
      `internal/glass` 自定的 ≥4.5:1 政策）→ 新增 `DangerText` 变体
      （暗色 `#f2726c` = 5.26:1 / 亮色 `#b3261e`）；线路错误提示同改
- [x] 13.2 **展开态「切在控件内 + 卡片下圆角缺失 + 无滚动提示」** →
      设置页 `ui.Scroll` 与线路列表内滚动均开 `KeepScrollbars(true)`；
      新增 `TestExpandedRoutesReachability` 记录底部左缘序列（实测无内收，
      属「内容超出固定窗口」的预期观感，不阻断）
- [x] 13.3 演示数据补停用样本后，视觉代理实测确认三条通道全部生效：
      灰点 (148,148,148) / 名称 141.3 / 类型 145 / 按钮「启用」；
      折叠摘要「3 条线路（2 启用）· 最快 直连」自洽
- [ ] 13.4 建议项未做（记录在案）：展开态提示文案指向右上角停止开关、
      内滚动「还有更多」表达、折叠摘要 x=77 对齐、`view.go:1345` 过期注释
