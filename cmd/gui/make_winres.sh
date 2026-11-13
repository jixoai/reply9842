#!/usr/bin/env bash
# 生成 Windows 资源对象（.syso），把图标与版本信息嵌进 exe。
#
# 为什么需要这一步：Go 没有把图标写进 PE 资源的原生手段，Windows 又只认
# PE 资源段里的 RT_GROUP_ICON —— 光有 app.ico 文件对双击后的资源管理器
# 毫无作用。.syso 是 Go 会把同目录下的 *.syso 自动链接进包的机制，正好用来
# 承载 windres 风格的资源。
#
# 产物：cmd/gui/rsrc_windows_amd64.syso（已入库，CI 与本地都不需要再装工具）
#
# 重新生成的前提：图标或版本信息变了才需要跑；纯代码改动不需要。
# 依赖：go install github.com/tc-hib/go-winres@latest
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
assets="$here/assets"
ico="$assets/app.ico"

test -f "$ico" || { echo "找不到 $ico，先跑 make_icons.sh" >&2; exit 1; }

winres="$(command -v go-winres || true)"
if [ -z "$winres" ]; then
  winres="$(go env GOPATH)/bin/go-winres"
fi
test -x "$winres" || {
  echo "缺少 go-winres：go install github.com/tc-hib/go-winres@latest" >&2
  exit 1
}

# 版本号来源优先级：
#   1. 环境变量 VERSION（CI 的 tag 构建与 Makefile 都走这条，唯一可信来源）
#   2. git describe 的**真正版本标签**（形如 v1.2.3）
# 不接受裸 commit hash：git describe 没有 tag 时会退回 96eba9e，那不是版本号，
# 直接取数字前缀会得到 "96.0.0.0" 这种假版本，比 0.0.0 更误导用户。
ver="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo "")}"
num="$(printf '%s' "$ver" | sed -nE 's/^v?([0-9]+(\.[0-9]+)+).*/\1/p')"
[ -n "$num" ] || num="0.0.0"
# 补齐到四段：Windows 的 FILEVERSION 固定是 a.b.c.d
case "$(printf '%s' "$num" | awk -F. '{print NF}')" in
  1) num="$num.0.0.0" ;;
  2) num="$num.0.0" ;;
  3) num="$num.0" ;;
esac

# 只在 cmd/gui 下生成，产物落在源码目录旁边，go build 自动拾取。
# --manifest gui 同时写入「按 DPI 感知、非高 DPI 不和操作系统打架」的清单，
# 以及 requestedExecutionLevel=asInvoker（不要提权，我们要绑本地端口而已）。
cd "$here"
"$winres" simply \
  --arch amd64 \
  --out rsrc \
  --manifest gui \
  --icon "$ico" \
  --product-name "rfc9842-reply" \
  --file-description "rfc9842-reply 客户端" \
  --original-filename "relay-gui-windows-amd64.exe" \
  --product-version "$num" \
  --file-version "$num" \
  --copyright "MIT License"

ls -lh "$here"/rsrc_windows_*.syso
echo "版本 ${num}（来源 ${ver}）"
