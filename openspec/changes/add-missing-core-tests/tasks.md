# Tasks: add-missing-core-tests

## 1. internal/store（优先级最高）

- [ ] 1.1 `internal/store/store_test.go`：`TestPutGetRoundtrip`、`TestPutCopiesBody`
      （Put 后修改原切片，缓存内容不变——`Put` 里有一份 `copy`，需钉住）
- [ ] 1.2 `TestPutSameContentSingleEntry`（同内容两次 Put 只有一份，`Stats().Items == 1`）
- [ ] 1.3 `TestEvictOnBudget`（超预算后最旧条目消失，`Get` 返回 false）
- [ ] 1.4 `TestEvictKeepsAtLeastOne`（单条 body 就超过 maxByte 时的行为：断言不会把自己
      也淘汰成空——`evictLocked` 有 `s.lru.Len() > 1` 保护，需明确这是刻意的）
- [ ] 1.5 `TestStatsCounters`（hits/misses/evictions/evictedBytes 与操作一一对应）

## 2. internal/idem（安全不变量）

- [ ] 2.1 `TestKeyStable`（同输入两次得同一键）
- [ ] 2.2 `TestKeySeparatesCallers`（仅 callerKeyId 不同 → 键不同）
- [ ] 2.3 `TestCallerKeyIDFallbacks`（Bearer / X-Api-Key / 空 → `anon`；同 token 稳定）
- [ ] 2.4 `TestKeyIncludesHeaderSubset`（content-type 变则键变；user-agent 变则键不变）
- [ ] 2.5 `TestKeyBodySensitivity`（body 变则键变）
- [ ] 2.6 若保留 `StreamRequested`：`TestStreamRequested`（四种写法与 false 用例）；
      若不保留则由 `expose-store-cache-metrics` 的清理任务删除

## 3. cmd/cli

- [ ] 3.1 把子命令嗅探抽成一个可测的纯函数（若能不改行为地抽），或直接测 `main` 的
      分支逻辑
- [ ] 3.2 用例：无子命令 → serve；`-listen x` → serve；`serve`/`status`/`version`/`help`
      → 各自分支；`foo` → 退出码 2

## 4. cmd/server

- [ ] 4.1 `TestIsHopByHop`（表驱动：全量 hop-by-hop 头为 true，
      `content-type`/`accept` 为 false，大小写不敏感）
- [ ] 4.2 `TestFlagDefaults`（断言与 README 参数表一致的默认值；`-cache-mb` 256、
      `-tmp-mb` 512、`-idem-mb` 2048、`-upstream-timeout` 10m）
- [ ] 4.3 `TestHealthProbeAddrResolution`（`0.0.0.0:8080` / `:8080` / `127.0.0.1:8080`
      三种输入解析成正确的探测目标）

## 5. 验证

- [ ] 5.1 `go test ./...` 全绿，且 `internal/store`、`internal/idem`、`cmd/cli`、
      `cmd/server` 从「no test files」变为有测试
- [ ] 5.2 `go test -race ./...` 通过（store 有 mutex，值得跑一次）
- [ ] 5.3 若测试暴露生产 bug：单独提交修复，并在提交信息里写明「测试先红后绿」

## 验收标准

- 四个包不再显示 `[no test files]`。
- store 的「淘汰后必须 miss」与 idem 的「跨调用者隔离」各有可执行证据。
- 不改动任何生产行为（除非暴露真 bug 并单独修复）。
