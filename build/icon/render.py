# /// script
# requires-python = ">=3.10"
# dependencies = ["playwright==1.57.0", "pillow"]
# ///
"""Renders the app icon from icon.svg, keeping the transparent margin around the rounded square.

    uv run build/icon/render.py    # writes icon-{256,512,1024}.png, icon.icns, internal/gui/icon.png

Uses Playwright's headless Chromium (a separate binary, not the desktop Chrome) because it draws the
SVG's drop shadow exactly; iconutil (macOS) packs the .icns.
"""
from __future__ import annotations

import pathlib
import shutil
import subprocess
import tempfile

from PIL import Image
from playwright.sync_api import sync_playwright

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parent.parent


def render() -> Image.Image:
    svg = (HERE / "icon.svg").read_text()
    with sync_playwright() as p:
        browser = p.chromium.launch()
        page = browser.new_page(viewport={"width": 1024, "height": 1024})
        page.set_content(f"<html><body style='margin:0;background:transparent'>{svg}</body></html>")
        png = page.locator("svg").screenshot(omit_background=True)
        browser.close()
    with tempfile.NamedTemporaryFile(suffix=".png") as f:
        f.write(png)
        f.flush()
        im = Image.open(f.name).convert("RGBA")
        im.load()
    assert im.size == (1024, 1024) and im.getpixel((0, 0))[3] == 0, "the corners must be transparent"
    return im


def main() -> None:
    big = render()
    size = lambda n: big if n == 1024 else big.resize((n, n), Image.LANCZOS)
    for n in (256, 512, 1024):
        size(n).save(HERE / f"icon-{n}.png", optimize=True)
    size(256).save(ROOT / "internal" / "gui" / "icon.png", optimize=True)

    with tempfile.TemporaryDirectory() as tmp:
        iconset = pathlib.Path(tmp) / "icon.iconset"
        iconset.mkdir()
        for n in (16, 32, 128, 256, 512):
            size(n).save(iconset / f"icon_{n}x{n}.png")
            size(n * 2).save(iconset / f"icon_{n}x{n}@2x.png")
        if shutil.which("iconutil"):
            subprocess.run(["iconutil", "-c", "icns", "-o", str(HERE / "icon.icns"), str(iconset)], check=True)
        else:
            print("iconutil not found (macOS only): icon.icns left as it was")
    print("wrote build/icon/icon-{256,512,1024}.png, icon.icns, internal/gui/icon.png")


if __name__ == "__main__":
    main()
