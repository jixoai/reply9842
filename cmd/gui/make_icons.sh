#!/usr/bin/env bash
# 从 cmd/gui/assets/icon.svg 生成打包用的图标资源。
#
# 产出（与 mygo 的 macos-bundle 规范一致）：
#   cmd/gui/assets/AppIcon.icns   macOS 图标，go:embed 进二进制
#   cmd/gui/assets/app.ico        Windows 图标，喂给 go-winres 生成 .syso
#   cmd/gui/assets/preview/*.png  各尺寸预览，供人工核对 16px 可辨认性
#
# 依赖：rsvg-convert（brew install librsvg）、iconutil（macOS 自带）、
#       Pillow（用于合成 .ico 与预览页，走 assets/requirements.txt）
#
# 为什么不用 sips：sips 只能缩放已有位图，不能把 SVG 栅格化；图标必须从矢量
# 源出，否则 1024/512/256 各档会是同一个位图的插值，小尺寸锐度全丢。
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
assets="$here/assets"
svg="$assets/icon.svg"
iconset="$assets/AppIcon.iconset"
preview="$assets/preview"

if ! command -v rsvg-convert >/dev/null 2>&1; then
  echo "缺少 rsvg-convert：brew install librsvg" >&2
  exit 1
fi
if ! command -v iconutil >/dev/null 2>&1; then
  echo "缺少 iconutil（需要 macOS）" >&2
  exit 1
fi
test -f "$svg" || { echo "找不到 $svg" >&2; exit 1; }

rm -rf "$iconset" "$preview"
mkdir -p "$iconset" "$preview"

# .iconset 的十档：逻辑尺寸 × 1x/2x。@2x 是高密度屏实际取用的图，
# 缺档会让 Retina 上的图标变糊，所以十档一个都不能少。
emit() { # <逻辑尺寸> <倍率>
  local size=$1
  local scale=$2
  local px=$((size * scale))
  local name
  if [ "$scale" -eq 1 ]; then name="icon_${size}x${size}.png"; else name="icon_${size}x${size}@2x.png"; fi
  rsvg-convert -w "$px" -h "$px" "$svg" -o "$iconset/$name"
}

for size in 16 32 128 256 512; do
  emit "$size" 1
  emit "$size" 2
done

iconutil -c icns "$iconset" -o "$assets/AppIcon.icns"
echo "  icns  $assets/AppIcon.icns ($(stat -f%z "$assets/AppIcon.icns") bytes)"

# .ico 用独立的多尺寸（Windows 取 16/20/24/32/40/48/64/256），
# 与 .iconset 的逻辑尺寸不同，单独渲染一遍更准确。
py="$(command -v python3)"
"$py" "$here/make_ico.py" "$svg" "$assets/app.ico"

# 托盘图标：48×48 像素。菜单栏图标是 18pt（Retina 36px），Windows 通知区域
# 按 100%/150%/200% DPI 取 16/24/32px，48 是唯一一档能在所有 DPI 下干净降采
# 的尺寸（36 在 150% 下要插值到 24，笔画会糊）。mygo 会把非正方形图按高度等比
# 缩放，所以只要保证是方的、字形贴边即可。
#
# 注意：tray.svg 是**独立事实来源**（纯黑透明的 template），不是 icon.svg 的
# 派生。两者的字形数据相同，但底板/油墨必须去掉——template 图标只认 alpha。
rsvg-convert -w 48 -h 48 "$assets/tray.svg" -o "$assets/tray.png"

# 预览：设计复核用。16/32 必须在这个尺寸下自己看过，不能只看 1024。
for px in 1024 512 256 128 64 32 16; do
  rsvg-convert -w "$px" -h "$px" "$svg" -o "$preview/icon-$px.png"
done
# 托盘预览：只出真实会被用到的尺寸（16 起，Windows 100% DPI 的极限）
for px in 16 20 24 32 48; do
  rsvg-convert -w "$px" -h "$px" "$assets/tray.svg" -o "$preview/tray-$px.png"
done
"$py" "$here/make_preview.py" "$preview"

# iconset 是中间产物（十张 PNG 加起来比 icns 大得多），只留 icns。
rm -rf "$iconset"
ls -lh "$assets/AppIcon.icns" "$assets/app.ico"
