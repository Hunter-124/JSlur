"""AutoApply vision-search stealth sidecar.

A long-lived helper the Go app launches when the user selects the Python browser
engine. It drives Playwright + pw-stealth-enhanced (stronger anti-bot evasions
than the built-in chromedp path) and speaks a tiny line-delimited JSON protocol
over stdin/stdout so it mirrors the Go Session.Shots contract:

  Go -> us  (one JSON object per line):
      {"url": "...", "cookie": "k=v; ...", "ua": "...", "maxScreens": 3}
      {"quit": true}
  us -> Go  (one JSON object per line):
      {"ready": true}                                  (once, after the browser boots)
      {"images": ["<base64 png>", ...], "links": [{"text","url"}], "title": "", "text": ""}
      {"error": "..."}

Anything that isn't protocol (Playwright noise, tracebacks) goes to stderr, which
the Go side captures for diagnostics. Run with --headful to show the window.
"""

import asyncio
import base64
import json
import sys
from urllib.parse import urlparse

# A real, current desktop-Chrome UA used when the request doesn't supply one
# (e.g. a board with no connected account). Never advertises HeadlessChrome.
DEFAULT_UA = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

LINK_JS = r"""(() => {
  const seen = new Set(), out = [];
  for (const a of document.querySelectorAll('a[href]')) {
    const u = a.href;
    if (!u || seen.has(u)) continue;
    seen.add(u);
    const t = (a.innerText || a.getAttribute('aria-label') || '').replace(/\s+/g, ' ').trim().slice(0, 120);
    out.push({ text: t, url: u });
    if (out.length >= 400) break;
  }
  return out;
})()"""

TEXT_JS = r"""(() => ((document.body && document.body.innerText) || '').replace(/\s+/g, ' ').trim().slice(0, 600))()"""

READY_JS = r"""(() => {
  const t = (document.title || '').toLowerCase();
  const blocked = /just a moment|attention required|checking your browser|verify (you|that you)|are you a human|enable javascript and cookies|security check|press & hold|verifying you are human/.test(t);
  const len = document.body ? document.body.innerText.length : 0;
  return document.readyState === 'complete' && !blocked && len > 200;
})()"""

SCROLL_JS = r"""(() => { const b = window.scrollY; window.scrollBy(0, Math.round(window.innerHeight * 0.9)); return Math.abs(window.scrollY - b) < 4; })()"""


def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def log(msg):
    sys.stderr.write(str(msg) + "\n")
    sys.stderr.flush()


def parse_cookies(header, url):
    out = []
    for part in (header or "").split(";"):
        part = part.strip()
        if "=" not in part:
            continue
        name, value = part.split("=", 1)
        name = name.strip()
        if not name:
            continue
        out.append({"name": name, "value": value.strip(), "url": url})
    return out


async def wait_for_content(page, max_seconds):
    """Wait until the page is past any JS/Cloudflare interstitial and shows real
    content, with a short minimum settle for XHR-rendered listings."""
    import time
    deadline = time.monotonic() + max_seconds
    min_settle = time.monotonic() + 2.5
    while True:
        try:
            ready = await page.evaluate(READY_JS)
        except Exception:
            ready = False
        if time.monotonic() > deadline or (ready and time.monotonic() > min_settle):
            await page.wait_for_timeout(800)
            return
        await page.wait_for_timeout(600)


async def handle(browser, apply_stealth, StealthConfig, req):
    url = req.get("url", "")
    cookie = req.get("cookie", "") or ""
    ua = req.get("ua") or DEFAULT_UA
    try:
        max_screens = int(req.get("maxScreens") or 3)
    except (TypeError, ValueError):
        max_screens = 3
    if max_screens < 1:
        max_screens = 1

    # Set UA/viewport/locale on the context ourselves — apply_stealth only injects
    # JS evasions, it does NOT set these — then layer the stealth patches on top.
    context = await browser.new_context(
        user_agent=ua,
        viewport={"width": 1280, "height": 1800},
        locale="en-US",
        timezone_id="America/New_York",
        extra_http_headers={"Accept-Language": "en-US,en;q=0.9"},
    )
    try:
        await apply_stealth(context, config=StealthConfig(locale="en-US", timezone_id="America/New_York"))
        if cookie:
            try:
                await context.add_cookies(parse_cookies(cookie, url))
            except Exception as e:
                log("add_cookies: %s" % e)
        page = await context.new_page()
        await page.goto(url, wait_until="domcontentloaded", timeout=45000)
        # Give a JS bot-challenge (Cloudflare "Just a moment", Indeed security
        # check) generous time to auto-solve in a real browser before shooting.
        await wait_for_content(page, 20.0)

        images = []
        for i in range(max_screens):
            png = await page.screenshot(type="png")
            images.append(base64.b64encode(png).decode("ascii"))
            if i == max_screens - 1:
                break
            try:
                at_bottom = await page.evaluate(SCROLL_JS)
            except Exception:
                break
            await page.wait_for_timeout(1200)
            if at_bottom:
                break

        try:
            links = await page.evaluate(LINK_JS)
        except Exception:
            links = []
        try:
            title = await page.title()
        except Exception:
            title = ""
        try:
            text = await page.evaluate(TEXT_JS)
        except Exception:
            text = ""
        return {"images": images, "links": links, "title": title, "text": text}
    finally:
        try:
            await context.close()
        except Exception:
            pass


async def serve(headful):
    try:
        from playwright.async_api import async_playwright
        from pw_stealth_enhanced import apply_stealth, StealthConfig
    except Exception as e:
        emit({"error": "import failed (install playwright + pw-stealth-enhanced): %s" % e})
        return

    pw = await async_playwright().start()
    try:
        browser = await pw.chromium.launch(
            headless=not headful,
            args=["--disable-blink-features=AutomationControlled", "--lang=en-US"],
        )
    except Exception as e:
        emit({"error": "browser launch failed (run: playwright install chromium): %s" % e})
        await pw.stop()
        return

    emit({"ready": True})

    loop = asyncio.get_event_loop()
    try:
        while True:
            # Block off-thread so we don't stall the event loop between requests.
            line = await loop.run_in_executor(None, sys.stdin.readline)
            if not line:
                break  # stdin closed -> shut down
            line = line.strip()
            if not line:
                continue
            try:
                req = json.loads(line)
            except Exception as e:
                emit({"error": "bad request json: %s" % e})
                continue
            if req.get("quit"):
                break
            try:
                emit(await handle(browser, apply_stealth, StealthConfig, req))
            except Exception as e:
                emit({"error": str(e)})
    finally:
        try:
            await browser.close()
        except Exception:
            pass
        try:
            await pw.stop()
        except Exception:
            pass


def main():
    headful = "--headful" in sys.argv[1:]
    try:
        asyncio.run(serve(headful))
    except KeyboardInterrupt:
        pass
    except Exception as e:
        emit({"error": "sidecar crashed: %s" % e})


if __name__ == "__main__":
    main()
