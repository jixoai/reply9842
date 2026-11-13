# Tasks: align-makefile-dist-naming

## 1. 命名对齐

- [ ] 1.1 `dist-cli`：`out=$(DIST_DIR)/cli-$$os-$$arch`（去掉 `relay-`）
- [ ] 1.2 `dist-gui`：`out=$(DIST_DIR)/gui-$$os-$$arch`
- [ ] 1.3 更新 Makefile 头部注释里的命名说明，并明确「`bin/` 用本地名、`dist/` 用分发名」

## 2. 补齐 server

- [ ] 2.1 新增 `SERVER_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
      windows/amd64`
- [ ] 2.2 新增 `dist-server` 目标（与 `dist-cli` 同构）
- [ ] 2.3 `dist: dist-cli dist-gui dist-server`
- [ ] 2.4 `.PHONY` 与 `help` 文案同步

## 3. 完整性断言

- [ ] 3.1 `dist` 末尾逐一比对预期的 16 个文件名，缺失或 0 字节即 `exit 1`（与
      `.github/workflows/release.yml` 的断言等价）
- [ ] 3.2 断言 `gui-linux-*` 不存在

## 4. 文档

- [ ] 4.1 `INSTALL.md` 的「下载预编译二进制」产物表：把 `relay-cli-linux-amd64` 等
      改为 `cli-linux-amd64` 等（或说明「文件名以 Release 页面为准」）
- [ ] 4.2 `INSTALL.md` 的「从源码构建」段落补 `make dist` 现在也产 server

## 5. 验证

- [ ] 5.1 `make dist` 后 `ls dist | sort` 与 Release Assets 清单逐个对得上
- [ ] 5.2 故意删掉一个产物再跑断言，确认它会失败（验证断言本身有效）
- [ ] 5.3 `go build` 与 `make build`（本地单平台）不受影响

## 验收标准

- `make dist` 产出 16 个产物，命名与 Release 完全一致。
- 少一个产物时 `make dist` 会失败，而不是静默产出不完整的 dist/。
- `INSTALL.md` 的产物表与实际文件名一致。
