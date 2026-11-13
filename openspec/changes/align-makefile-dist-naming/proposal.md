## Why

`distribution` spec 要求「所有通道产物命名统一为 `<kind>-<os>-<arch>[.exe]`」，CI 与 npm
都遵守了，**Makefile 没有**：

```
Makefile:78    out=$(DIST_DIR)/relay-cli-$$os-$$arch;
Makefile:97    out=$(DIST_DIR)/relay-gui-$$os-$$arch;
.github/workflows/release.yml:208  out="dist/${kind}-${os}-${arch}${ext}"   ← 无 relay- 前缀
```

Makefile 的注释还明确写着「命名与 .github/workflows/release.yml 完全一致，便于本地验证
release 产物」，但实际产物是 `relay-cli-darwin-arm64` 而 Release 上是
`cli-darwin-arm64`。也就是说 `make dist` 产出的东西**恰恰不能**用来验证 Release 产物，
而这正是它存在的理由。

另外 `make dist` 只构建 cli 与 gui，**不构建 server**：

```
Makefile:33  CLI_TARGETS := ...
Makefile:34  GUI_TARGETS := ...
Makefile:35  （没有 SERVER_TARGETS）
```

而 CI 构建三种 kind、npm 下载三种 kind、DEPLOY.md 说「下载 `server-<os>-<arch>`」。因此
本地无法用一条命令产出与 Release 完全一致的全套 16 个产物。

## What Changes

- Makefile 产物名去掉 `relay-` 前缀：`$(DIST_DIR)/cli-$$os-$$arch`、`gui-...`。
- 新增 `SERVER_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64`
  与 `dist-server` 目标；`dist` 变为 `dist-cli dist-gui dist-server`。
- `make dist` 结束后断言产物齐全（与 CI 的完整性断言同款：逐一比对清单，缺失即失败），
  这样本地能复现 CI 的「静默少一个」检查。
- `make build`（本地单平台）保持现有 `bin/relay-cli` / `bin/relay-gui` 命名不变
  ——那是本地开发用的名字，不与分发命名冲突（但要在 help 里说清区别）。

## Capabilities

### New Capabilities

无。

### Modified Capabilities

- `distribution`: 追加「Makefile 的 dist 产物必须与 Release 命名及清单完全一致」需求。

## Impact

- 改动 `Makefile`（`dist-cli` / `dist-gui` 的 out 命名、新增 `dist-server`、
  `dist` 依赖、`help` 文案、完整性断言）。
- 改动 `INSTALL.md` 第 61–68 行的产物表（当前写的是 `relay-cli-linux-amd64` 等，与
  Release 实际名称不符——同一个问题的文档面）。
- 对使用者是**破坏性**的（如果你有脚本依赖 `dist/relay-cli-*`），但当前 `dist/` 是
  gitignore 的本地产物，影响面仅限本地脚本。
