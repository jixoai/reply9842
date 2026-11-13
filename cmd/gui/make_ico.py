#!/usr/bin/env python3
"""把 icon.svg 栅格化成多尺寸 .ico，并给 Windows 版加圆角底板。

用法：make_ico.py <icon.svg> <out.ico>

为什么 .ico 需要单独的圆角处理：

  macOS 会自己把满幅（edge-to-edge）图标裁成 squircle 并补边缘高光，所以
  源 SVG 是满幅的、不做圆角。Windows 不裁切任何东西——PE 资源里的图标位图
  是什么形状就画成什么形状。于是同一个满幅源在 Windows 上变成一块方砖，
  在 Win11 的圆角图标群（开始菜单/任务栏/资源管理器）里显得是「旧式图标」。

  所以这里对 .ico 的每一帧做两件事：
    1. 圆角蒙版：半径 ≈ 边长的 8%（Win11 圆角图标的观感比例），四角透明
    2. 1px 亮边：沿圆角轮廓描一条极淡的亮线，模拟 Win11 图标的边缘受光

  这是**渲染期处理**，不改 icon.svg。源 SVG 必须保持满幅，否则 macOS 会把
  它当旧式图标、在外面套一块浅灰底板（见 icon.svg 头部注释）。

Windows 的图标选择规则和 macOS 不同：任务栏用 32、桌面用 48、Alt-Tab 用
32/48、大图标视图用 256。这里把 16 到 256 都塞进去，让壳层自己挑；少了
某一档 Windows 会缩放最接近的那张，边缘会糊。
"""
import subprocess
import sys

from PIL import Image, ImageDraw

# Windows 实际会取的尺寸；256 是 Vista 起的「超大图标」档
SIZES = (16, 20, 24, 32, 40, 48, 64, 128, 256)

# 圆角半径 = 边长 × CORNER_RATIO。Win11 的应用图标圆角约为边长的 8%。
CORNER_RATIO = 0.08

# 边缘亮线的强度。太强会在小尺寸糊成白圈，0.10 实测在各档都只是「有边」。
RIM_ALPHA = 0.10


def rounded(im: Image.Image) -> Image.Image:
    """把方图裁成圆角，并在轮廓内侧描一圈极淡的亮边。

    用 4x 超采样画蒙版再缩回：直接按目标尺寸画圆角，16/20/24 这几档的
    圆角只有 1~2 像素，pillow 的 arc 会画出硬锯齿，小图标上一眼能看出来。
    """
    w, h = im.size
    r = max(1.0, w * CORNER_RATIO)
    ss = 4

    mask = Image.new("L", (w * ss, h * ss), 0)
    ImageDraw.Draw(mask).rounded_rectangle(
        (0, 0, w * ss - 1, h * ss - 1), radius=r * ss, fill=255
    )
    mask = mask.resize((w, h), Image.LANCZOS)

    out = Image.new("RGBA", (w, h), (0, 0, 0, 0))
    body = im.convert("RGBA")
    body.putalpha(mask)
    out.alpha_composite(body)

    # 亮边：整幅圆角蒙版减去向内缩 1px 的蒙版，得到沿轮廓的 1px 环
    ring = Image.new("L", (w * ss, h * ss), 0)
    ring.paste(255, (0, 0, w * ss, h * ss), mask=mask.resize((w * ss, h * ss), Image.NEAREST))
    inner = Image.new("L", (w * ss, h * ss), 0)
    inset = max(1.0, ss * 1.0)
    ImageDraw.Draw(inner).rounded_rectangle(
        (inset, inset, w * ss - 1 - inset, h * ss - 1 - inset),
        radius=max(1.0, (r - 1) * ss),
        fill=255,
    )
    ring = Image.composite(Image.new("L", ring.size, 0), ring, inner)
    rim = Image.new("RGBA", (w, h), (255, 255, 255, 0))
    rim.putalpha(ring.resize((w, h), Image.LANCZOS).point(lambda v: int(v * RIM_ALPHA)))
    out.alpha_composite(rim)
    return out


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    svg, out = sys.argv[1], sys.argv[2]

    images = []
    for px in SIZES:
        png = f"/tmp/.rfc9842-ico-{px}.png"
        subprocess.run(
            ["rsvg-convert", "-w", str(px), "-h", str(px), svg, "-o", png],
            check=True,
        )
        images.append(rounded(Image.open(png).convert("RGBA")))

    # 最大的那张作基底，sizes 指定全部档位，Pillow 会各自压缩成 PNG 帧
    base = images[-1]
    base.save(out, format="ICO", sizes=[(px, px) for px in SIZES],
              append_images=images[:-1])
    print(f"  ico   {out}  ({len(SIZES)} 帧，圆角 {CORNER_RATIO:.0%}，亮边 {RIM_ALPHA:.0%})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
