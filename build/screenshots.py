# /// script
# requires-python = ">=3.10"
# dependencies = ["playwright==1.57.0"]
# ///
"""Takes the README screenshots from the GUI's mock backend (no board needed), in English and Chinese.

    uv run build/screenshots.py            # writes docs/images/*.png

It serves internal/gui/frontend itself and drives Playwright's headless Chromium (a separate binary,
not the desktop Chrome): `uv run --with playwright==1.57.0 playwright install chromium-headless-shell`
once if it is missing.
"""
from __future__ import annotations

import functools
import http.server
import pathlib
import threading

from playwright.sync_api import Page, sync_playwright

ROOT = pathlib.Path(__file__).resolve().parent.parent
WEB = ROOT / "internal" / "gui" / "frontend"
OUT = ROOT / "docs" / "images"
PORT = 8799


class Quiet(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass


def serve() -> http.server.ThreadingHTTPServer:
    handler = functools.partial(Quiet, directory=str(WEB))
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", PORT), handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def start(page: Page, lang: str, extra: str = "") -> None:
    page.goto(f"http://127.0.0.1:{PORT}/index.html?mock&noanim&shot{extra}")
    # The mock asks for the board's SSH password once, like a first connection.
    page.wait_for_selector(".modal [data-bind=value]")
    page.fill(".modal [data-bind=value]", "mock")
    page.click("[data-act=prompt-ok]")
    page.click(f"[data-act=lang][data-lang={lang}]")
    page.wait_for_selector(".app-card")
    page.wait_for_function("window.__quay.S.sys.duck && window.__quay.S.sys.duck.data")  # the update check
    # A board with the China mirrors and the relay on.
    page.evaluate("""() => {
        const d = window.__quay.S.status.duck.data;
        d.mirror = { pip: 'https://repo.huaweicloud.com/repository/pypi/simple', debian: 'https://repo.huaweicloud.com/debian',
                     debianFallback: ['https://mirrors.bfsu.edu.cn/debian', 'https://mirrors.ustc.edu.cn/debian', 'http://deb.debian.org/debian'], managed: true };
        d.relay = { installed: true, active: true };
    }""")
    page.click(f"[data-act=lang][data-lang={lang}]")  # re-render
    page.wait_for_timeout(400)


def shot(page: Page, name: str, lang: str) -> None:
    path = OUT / f"{name}-{lang}.png"
    page.screenshot(path=str(path))
    print("wrote", path.relative_to(ROOT))


def scenes(page: Page, lang: str) -> None:
    start(page, lang)
    shot(page, "overview", lang)

    page.click("[data-act=mirror-test]")
    page.wait_for_timeout(3200)  # the speed test results stream in
    shot(page, "mirrors", lang)
    page.click("[data-act=modal-close]")

    page.click("[data-act=system]")
    page.wait_for_selector("[data-act=sys-download]")
    shot(page, "system-update", lang)
    page.click("[data-act=modal-ok]")
    page.wait_for_selector(".pbar")
    page.wait_for_timeout(3600)  # mid-download
    shot(page, "update-progress", lang)

    start(page, lang)
    page.click("header [data-act=install]")
    page.click("[data-act=pick-cat]")
    page.wait_for_selector(".hooks-box")
    page.wait_for_timeout(300)
    shot(page, "custom-install", lang)

    # The app card of an app that follows GitHub: three new commits, a red dot on Update.
    start(page, lang)
    card = page.locator(".app-card").first
    card.screenshot(path=str(OUT / f"app-update-{lang}.png"))
    print("wrote", (OUT / f"app-update-{lang}.png").relative_to(ROOT))

    # Away from home: everyday Wi-Fi, a phone hotspot and the away plan "both".
    start(page, lang, "&netdemo")
    page.click(".stat[data-act=network]")
    page.wait_for_selector(".hs-box")
    page.wait_for_timeout(300)
    shot(page, "network", lang)

    start(page, lang)
    page.click(".app-card [data-act=policies]")
    page.wait_for_selector(".pol")
    page.wait_for_timeout(300)
    shot(page, "policies", lang)


def main() -> None:
    OUT.mkdir(parents=True, exist_ok=True)
    srv = serve()
    try:
        with sync_playwright() as p:
            browser = p.chromium.launch(headless=True)
            try:
                for lang in ("en", "zh"):
                    page = browser.new_page(viewport={"width": 1280, "height": 800}, device_scale_factor=2,
                                            color_scheme="light", locale="zh-CN" if lang == "zh" else "en-US")
                    scenes(page, lang)
                    page.close()
            finally:
                browser.close()
    finally:
        srv.shutdown()


if __name__ == "__main__":
    main()
