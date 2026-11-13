## Why

`go test ./...` 全绿，但覆盖有明显缺口——几个承载关键不变量的包**没有一行测试**：

```
internal/store    → 只有 internal/store/store.go（无测试）   ← 承载「内容寻址 + LRU 淘汰」这条核心不变量
internal/idem     → 只有 internal/idem/key.go（无测试）     ← 承载「跨调用者隔离」这条安全不变量
internal/glass    → 只有 glass.go/theme.go    ← GUI 材质（视觉，靠快照测试覆盖）
cmd/cli           → 无测试                    ← 子命令嗅探（无子命令即 serve）这条兼容性承诺
cmd/server        → 无测试                    ← flag 默认值、hop-by-hop 剥离、健康检查
```

其中两条特别值得补：

1. **`internal/store`**：这是整个压缩收益的中枢（`hash → body` + 字节预算 LRU）。它没有
   测试，意味着「淘汰后必须 miss」「Put 必须复制 body 不得持有调用方缓冲」「预算记账不
   漂移」全靠肉眼。而 `spool` 因为踩过记账漂移的坑（a7f017a 的第 3 个 bug）补了
   `TestBudgetAccountingNoDrift`——同一个坑在 store 里没有护栏。
2. **`internal/idem`**：`CallerKeyID` 与 `Key` 承载「跨调用者不得互相命中」这条安全不变量
   （`internal/idem/key.go:10` 明确写「安全约束，勿改」）。没有测试 = 这条约束没有证据。

另外 `cmd/cli` 的「无子命令即 serve」是明确承诺「旧写法的脚本无需改动」的兼容性行为，
只有一次提交说明、没有测试。

## What Changes

- 新增 `internal/store/store_test.go`：Put/Get/Has、body 复制（改原切片不影响缓存）、
  同内容只存一份、字节预算触发淘汰、淘汰后 Get 返回 false、`Stats()` 计数正确、
      超单条预算时的行为（当前 `evictLocked` 有 `s.lru.Len() > 1` 的保护，需断言）。
- 新增 `internal/idem/key_test.go`：同输入同键、callerKeyId 不同则键不同、无凭据退化为
  `anon`、header 子集参与计算（content-type 变则键变）、authorization 不直接进入键材料、
  `StreamRequested` 的真假判定（若保留该函数）。
- 新增 `cmd/cli/main_test.go`（或抽出可测的子命令解析函数）：`serve`/`status`/`version`/
  `help` 识别、`-listen x` 按 serve、未知子命令退出码 2。
- 新增 `cmd/server/main_test.go`：`isHopByHop` 表驱动、flag 默认值断言（与 README 参数表
  一致）、`-health-probe` 地址解析。
- 可选：`internal/glass` 不做单测（视觉靠 `cmd/gui/view_test.go` 的快照）。

## Capabilities

### New Capabilities

无。

### Modified Capabilities

- `uplink-delta`: 追加「store 的淘汰与记账必须有测试护栏」需求（它属于该能力）。
- `security`: 追加「幂等键的调用者隔离必须有测试证明」需求。
- `client-cli`: 追加「子命令嗅探必须有测试」需求。

## Impact

- 只新增测试文件，不改生产代码（除非测试暴露 bug——那时按「修 bug」单独处理）。
- 心智影响：把三条「口头承诺」变成可执行证据。
