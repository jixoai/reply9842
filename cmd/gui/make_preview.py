#!/usr/bin/env python3
"""拼一张尺寸阶梯预览图，用于人工核查小尺寸可辨认性。

用法：make_preview.py <preview 目录>

为什么必须看图而不是只跑脚本：图标最容易骗人的地方就是 1024 看着漂亮、
16 像素糊成一团。这张图把各档并排、并把 16/32 放大到肉眼可判的尺寸。
"""
import sys
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

SIZES = (1024, 512, 256, 128, 64, 32, 16)
UPSCALED = (16, 32)   # 这两档额外放大，用来判定"还认得出吗"


def label_font(size=13):
    """挑一个能显示中文的字体；标签里有中文，纯 Arial 会渲染成方块。"""
    for p in ("/System/Library/Fonts/PingFang.ttc",
              "/System/Library/Fonts/STHeiti Light.ttc",
              "/System/Library/Fonts/Hiragino Sans GB.ttc",
              "/Library/Fonts/Arial Unicode.ttf",
              "/System/Library/Fonts/Supplemental/Arial Unicode.ttf",
              "/System/Library/Fonts/Supplemental/Arial.ttf",
              "/System/Library/Fonts/Helvetica.ttc"):
        if Path(p).exists():
            try:
                return ImageFont.truetype(p, size)
            except OSError:
                continue
    return ImageFont.load_default()


def main() -> int:
    if len(sys.argv) != 2:
        print(__doc__, file=sys.stderr)
        return 2
    d = Path(sys.argv[1])
    font = label_font()

    cell = 210
    cols = len(SIZES)
    big = 150
    sheet = Image.new("RGB", (cols * cell, big + cell + 60), (247, 247, 249))
    draw = ImageDraw.Draw(sheet)

    for i, px in enumerate(SIZES):
        im = Image.open(d / f"icon-{px}.png").convert("RGB")
        shown = im.resize((big, big), Image.LANCZOS)
        x = i * cell + (cell - big) // 2
        sheet.paste(shown, (x, 10))
        draw.text((i * cell + 10, big + 20), f"{px}px", font=font, fill=(30, 30, 30))

    for i, px in enumerate(UPSCALED):
        im = Image.open(d / f"icon-{px}.png").convert("RGB")
        shown = im.resize((big, big), Image.NEAREST)
        x = i * cell + (cell - big) // 2
        sheet.paste(shown, (x, big + 44))
        draw.text((i * cell + 10, big + cell + 36),
                  f"{px}px 放大（真实像素）", font=font, fill=(120, 30, 30))

    out = d / "preview-sheet.png"
    sheet.save(out)
    print(f"  sheet {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
