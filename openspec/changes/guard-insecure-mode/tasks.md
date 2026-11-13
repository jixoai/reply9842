# Tasks: guard-insecure-mode

## 1. 运行时可见（必做）

- [ ] 1.1 `Runner.Start` 在 `Config.Insecure` 为 true 时打印显著警告（一行，非 debug，
      措辞含「证书校验已关闭，中间人可解开全部流量」）
- [ ] 1.2 GUI 在 insecure 时于状态行或 footer 显示持续可见标记（不是 5 秒提示），
      例如 `未校验证书` 徽标（用 `p.Warning` 色）
- [ ] 1.3 `stats.Record` 新增 `insecure` 字段（`json:"insecure,omitempty"`），
      客户端在记录时填入；`Agg` 不一定要聚合它，但 JSONL 里必须有

## 2. 地址分级（可选，建议单独一轮）

- [ ] 2.1 `transport` 增加 `IsLoopbackOrPrivate(addr)` 判定（127/8、::1、10/8、
      172.16/12、192.168/16、169.254/16）
- [ ] 2.2 若做二次确认：对非回环/非私网地址启用 insecure 时，要求同时给
      `-insecure-allow-remote`，否则启动失败并给出明确提示
- [ ] 2.3 在 `README.md` / `DEPLOY.md` 更新对应说明；把这条列为 BREAKING（若有脚本
      对公网开 insecure）

## 3. 验证

- [ ] 3.1 单测：`TestInsecureLogsWarning`（用可捕获的 logger 断言警告出现）、
      `TestInsecureRecordedInStats`
- [ ] 3.2 单测（若做 2.x）：`TestInsecureRemoteRequiresAck`、`TestInsecureLoopbackAllowed`
- [ ] 3.3 手工：`-insecure` 启动后 GUI 上看得到标记；`client-<date>.jsonl` 里该请求带
      `"insecure":true`

## 验收标准

- 任何一次 insecure 连接都能在启动输出与逐请求日志里被识别出来。
- 若做地址分级：对公网开 insecure 默认被拒（需显式确认），对回环/私网不受影响。
