# reply9842 — 交叉编译入口
#
# 设计（2026-11-13 kzf 需求：CLI 全平台 + GUI darwin/windows）：
#   - 纯 Go、CGO_ENABLED=0，任何机器都能编出所有平台的产物
#   - CLI 与 GUI 是两条独立的构建线：GUI 只有 darwin/windows（native UI 无 Linux 需求）
#   - 版本号从 git describe 注入，release 与本地构建同源
#
# 用法：
#   make build            本地（当前平台）编 CLI + GUI 到 bin/
#   make dist             全平台产物到 dist/（与 GitHub Releases 的命名一致）
#   make cli / make gui   只编一条线
#   make test             跑测试
#   make clean

SHELL := /bin/bash

# 版本：优先用 git 描述（v1.2.3 / v1.2.3-4-gabcdef），无 tag 时退回 commit
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# 剥掉 git describe 的前缀 v，产物名里不要出现 v
VERSION_NUM := $(patsubst v%,%,$(VERSION))

# -s -w 去符号与调试信息；-X 注入版本（两个 main 包各自有 version 变量）
LDFLAGS := -s -w -X main.version=$(VERSION_NUM)
# GUI 的 Windows 产物用 windowsgui 子系统，双击不弹黑框。
# 注意 go install 无法传这个 flag，所以 cmd/gui 还有 console_windows.go 兜底。
LDFLAGS_GUI_WIN := $(LDFLAGS) -H=windowsgui

GO ?= go
BUILD_DIR := bin
DIST_DIR := dist

# 全平台清单（GUI 只有三个）
CLI_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
GUI_TARGETS := darwin/arm64 darwin/amd64 windows/amd64

.PHONY: all build cli gui dist dist-cli dist-gui test vet fmt clean help icons winres

all: build

help:
	@echo "make build      本地编 CLI + GUI 到 $(BUILD_DIR)/"
	@echo "make dist       全平台产物到 $(DIST_DIR)/"
	@echo "make cli        dist 里的 CLI 部分"
	@echo "make gui        dist 里的 GUI 部分"
	@echo "make test       跑全部测试"
	@echo "make vet        跑 go vet"
	@echo "make fmt        gofmt 全部 Go 文件"
	@echo "make icons      从 icon.svg 重生成 AppIcon.icns / app.ico / 预览图"
	@echo "make winres     重生成 Windows 资源对象（图标变了才需要）"
	@echo "make clean      清掉 $(BUILD_DIR)/ 与 $(DIST_DIR)/"
	@echo ""
	@echo "VERSION=$(VERSION)"

# ---- 本地构建 ----

build: cli gui

cli:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/relay-cli ./cmd/cli

gui:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/relay-gui ./cmd/gui

# ---- 全平台产物 ----
#
# 命名与 .github/workflows/release.yml 完全一致，便于本地验证 release 产物：
#   relay-cli-<os>-<arch>[.exe]
#   relay-gui-<os>-<arch>[.exe]

dist: dist-cli dist-gui

dist-cli:
	@mkdir -p $(DIST_DIR)
	@set -e; for t in $(CLI_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		out=$(DIST_DIR)/relay-cli-$$os-$$arch; \
		if [ "$$os" = "windows" ]; then out=$$out.exe; fi; \
		echo "  cli  $$os/$$arch -> $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath \
			-ldflags "$(LDFLAGS)" -o "$$out" ./cmd/cli; \
	done

dist-gui:
	@mkdir -p $(DIST_DIR)
	@# Windows 资源对象：go build 自动链接同目录的 *.syso，缺了它 exe 里就没有
	@# 图标与版本信息。入库存的是图标；版本号是构建期的，含 tag 的构建必须
	@# 重新生成一次，否则 exe 属性里会永远停在入库那一刻的版本。非 Windows
	@# 构建不受影响（syso 只有 GOOS=windows 时才参与链接）。
	@if [ ! -f cmd/gui/rsrc_windows_amd64.syso ] || [ "$(VERSION_NUM)" != "dev" ]; then \
		echo "生成 Windows 资源（图标 + 版本 $(VERSION_NUM)）…"; \
		VERSION="$(VERSION)" ./cmd/gui/make_winres.sh >/dev/null; \
	fi
	@set -e; for t in $(GUI_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		out=$(DIST_DIR)/relay-gui-$$os-$$arch; \
		flags="$(LDFLAGS)"; \
		if [ "$$os" = "windows" ]; then \
			out=$$out.exe; flags="$(LDFLAGS_GUI_WIN)"; \
		fi; \
		echo "  gui  $$os/$$arch -> $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath \
			-ldflags "$$flags" -o "$$out" ./cmd/gui; \
	done
	@echo ""
	@echo "产物："
	@ls -lh $(DIST_DIR)

# ---- 检查 ----

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w $$(git ls-files '*.go')

# ---- 图标资源 ----
#
# icon.svg 是唯一事实来源；.icns / .ico / 预览图都是从它派生的产物。
# 改了 SVG 就必须重跑这两步，否则二进制里的还是旧图标。
#
# macOS 的 bundle 图标用 go:embed 打进二进制（见 cmd/gui/bundle_darwin.go），
# Windows 的图标必须先经 go-winres 变成 .syso 才能进 PE 资源段。

icons:
	./cmd/gui/make_icons.sh

winres:
	./cmd/gui/make_winres.sh

clean:
	rm -rf $(BUILD_DIR) $(DIST_DIR)
