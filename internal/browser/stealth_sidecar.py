"""AutoApply vision-search stealth sidecar.

A long-lived helper the Go app launches when the user selects the Python browser
engine. It drives Playwright + pw-stealth-enhanced (stronger anti-bot evasions
than the built-in chromedp path) and speaks a tiny line-delimited JSON protocol
over stdin/stdout.

Protocol (one JSON object per line):

  Go -> us:
      {"id": 7, "url": "...", "cookie": "k=v; ...", "ua": "...", "maxScreens": 3}
      {"id": 8, "url": "...", "cookie": "...", "ua": "...", "mode": "html"}
      {"quit": true}
  us -> Go:
      {"ready": true}                                  (once, after the browser boots)
      {"id": 7, "images": ["<b64 png>", ...], "links": [...], "title": "", "text": "",
       "blocked": "", "blockReason": ""}
      {"id": 8, "html": "...", "title": "", "blocked": "", "blockReason": ""}
      {"id": 7, "error": "..."}
      {"note": "...", "level": "info|warn", "id": 7}     (out-of-band progress/diagnostics)

Two upgrades over the original sidecar:

  * Persistent profile: launched with --profile <dir>, the browser uses one
    on-disk user-data dir, so a solved Cloudflare challenge, warmed fingerprint
    and any sign-in survive across requests AND across runs. No re-login.
  * Concurrency: requests carry an "id" and run as independent tasks (each in its
    own tab) up to --concurrency, so several boards/roles load at once. Responses
    echo the id so the Go side can match them.

Block detection: every load is classified (captcha / login / cloudflare). In
headful mode a detected block pauses that request so a human can solve it once —
the persistent profile then remembers it. In headless mode it returns promptly
with the blocked reason so the caller can fall back or warn.

Anything that isn't protocol (Playwright noise, tracebacks) goes to stderr, which
the Go side captures for diagnostics. Run with --headful to show the window.
"""

import argparse
import asyncio
import base64
import json
import sys
import time
from urllib.parse import urlparse

# In-flight requests that may need a mid-request control message ("resume"/"skip"
# from the user via the Go side), keyed by request id -> asyncio.Queue.
CONTROLS = {}

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

# Reports whether the page is past any JS/Cloudflare interstitial and showing
# real content. Used to time the screenshot and to know when a human has solved
# a challenge in headful mode.
READY_JS = r"""(() => {
  const t = (document.title || '').toLowerCase();
  const blocked = /just a moment|attention required|checking your browser|verify (you|that you)|are you a human|enable javascript and cookies|security check|press & hold|verifying you are human/.test(t);
  const len = document.body ? document.body.innerText.length : 0;
  return document.readyState === 'complete' && !blocked && len > 200;
})()"""

SCROLL_JS = r"""(() => { const b = window.scrollY; window.scrollBy(0, Math.round(window.innerHeight * 0.9)); return Math.abs(window.scrollY - b) < 4; })()"""

# Bot/captcha interstitial phrases (matched anywhere in title+text) and sign-in
# wall phrases (title only, since real result pages carry "sign in" links too).
CAPTCHA_NEEDLES = [
    ("just a moment", "cloudflare", "a Cloudflare bot check"),
    ("checking your browser", "cloudflare", "a Cloudflare browser check"),
    ("attention required", "cloudflare", "a Cloudflare block"),
    ("verify you are human", "captcha", "a human-verification check"),
    ("verifying you are human", "captcha", "a human-verification check"),
    ("are you a human", "captcha", "a human-verification check"),
    ("are you a robot", "captcha", "a bot check"),
    ("px-captcha", "captcha", "a PerimeterX captcha"),
    ("press & hold", "captcha", "a press-and-hold bot check"),
    ("press and hold", "captcha", "a press-and-hold bot check"),
    ("unusual traffic", "captcha", "a rate-limit / bot check"),
    ("security check", "captcha", "a security check"),
    ("enable javascript and cookies", "cloudflare", "a Cloudflare JS/cookie check"),
    # Hard IP/bot walls (Imperva/Incapsula, Distil) — unambiguous phrases only.
    ("pardon our interruption", "captcha", "an Imperva bot wall"),
    ("request unsuccessful. incapsula", "captcha", "an Incapsula/Imperva bot wall"),
    ("powered by distil", "captcha", "a Distil bot wall"),
    ("access to this page has been denied", "captcha", "an access-denied block"),
    ("your request has been blocked", "captcha", "an IP/bot block"),
    ("you have been blocked", "captcha", "an IP/bot block"),
]
LOGIN_NEEDLES = [
    ("sign in", "a sign-in wall"),
    ("sign up", "a sign-up wall"),
    ("log in", "a sign-in wall"),
    ("login", "a sign-in wall"),
    ("join linkedin", "a LinkedIn sign-in wall"),
]


def classify_block(title, text):
    """Return (kind, reason) describing a captcha/login wall, or ("", "")."""
    t = (title or "").lower()
    hay = (t + "\n" + (text or "")).lower()
    for needle, kind, reason in CAPTCHA_NEEDLES:
        if needle in hay:
            return kind, reason
    for needle, reason in LOGIN_NEEDLES:
        if needle in t:
            return "login", reason
    return "", ""


# One global lock so concurrent request tasks never interleave a partial line on
# stdout (each emit writes one complete JSON line).
_emit_lock = asyncio.Lock()


def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def note(msg, level="info", req_id=None):
    obj = {"note": str(msg), "level": level}
    if req_id is not None:
        obj["id"] = req_id
    emit(obj)


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


def registrable_domain(host):
    """Best-effort eTLD+1 (last two labels) of a host. Good enough for the .com
    job boards we connect (monster.com, ziprecruiter.com, joinhandshake.com); it
    groups sibling subdomains (login./www./api.) under one site, so a post-SSO
    redirect's cookies are still captured under the board's domain."""
    host = (host or "").lower().strip().strip(".").split(":")[0]
    parts = [p for p in host.split(".") if p]
    if len(parts) <= 2:
        return host
    return ".".join(parts[-2:])


def cookie_in_site(cookie_domain, reg):
    """Whether a cookie's domain belongs to the registrable domain `reg`."""
    dom = (cookie_domain or "").lower().lstrip(".").rstrip(".")
    return bool(dom) and bool(reg) and (dom == reg or dom.endswith("." + reg))


def cookies_for_host(cookies, host):
    """Render the context cookies for `host`'s registrable domain as a Cookie
    header ("a=1; b=2"). Scoping to the login site keeps unrelated SSO-provider
    cookies (google.com, microsoftonline.com) — and, crucially, a cf_clearance
    left in the shared profile by a DIFFERENT board — out of the header we replay
    on this board's own requests."""
    reg = registrable_domain(host)
    parts = []
    for c in cookies:
        name = c.get("name") or ""
        if name and cookie_in_site(c.get("domain"), reg):
            parts.append("%s=%s" % (name, c.get("value", "")))
    return "; ".join(parts)


async def handle_connect(context, req):
    """Connect-account capture: open the board's login URL in a real, headful
    stealth window so the user can sign in / create an account / clear a check by
    hand, then capture the resulting session (cookie header for the board's site +
    the browser's User-Agent). Finishes as soon as one of authCookies appears (a
    genuine post-login cookie) or, when none is given, when the user closes the
    window. Driven through the same browser the scrapers use, so a solved
    Cloudflare clearance is issued to a fingerprint the scrape can reproduce."""
    url = req.get("url", "")
    auth = set(req.get("authCookies") or [])
    try:
        timeout = float(req.get("timeout") or 300)
    except (TypeError, ValueError):
        timeout = 300.0
    host = urlparse(url).netloc or url
    reg = registrable_domain(host)

    def board_auth_names(cookies):
        # auth cookies that belong to THIS board's site — never a cf_clearance
        # left in the shared profile by a different board (which would otherwise
        # make the window "complete" the instant it opens).
        return {c.get("name") for c in cookies
                if c.get("name") in auth and cookie_in_site(c.get("domain"), reg)}

    page = await context.new_page()
    ua = ""
    captured = ""
    try:
        # Snapshot the board's auth cookies already present (persisted in the
        # profile, or set instantly on load) so we only finish on a NEW one set
        # by THIS sign-in — never the instant the window opens.
        try:
            already = board_auth_names(await context.cookies())
        except Exception:
            already = set()

        try:
            await page.goto(url, wait_until="domcontentloaded", timeout=45000)
        except Exception as e:
            log("connect goto %s: %s" % (url, e))
        try:
            ua = await page.evaluate("navigator.userAgent")
        except Exception:
            ua = ""
        if auth:
            note("complete sign-in / clear any human check on %s in the browser "
                 "window — the session is captured automatically when it's done "
                 "(or just close the window when finished)" % host, "info")
        else:
            note("sign in / create your account on %s, then close the browser "
                 "window when you're done" % host, "info")

        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                cookies = await context.cookies()
            except Exception:
                # Context/window closed (the user finished and closed it) — stop
                # with whatever we last captured.
                break
            fresh = cookies_for_host(cookies, host)
            if fresh:
                captured = fresh
            # Done the moment a NEW board-scoped post-login cookie shows up.
            if auth and (board_auth_names(cookies) - already):
                break
            if page.is_closed():
                break
            await asyncio.sleep(1.5)
        return {"cookies": captured, "ua": ua}
    finally:
        try:
            if not page.is_closed():
                await page.close()
        except Exception:
            pass


async def wait_for_content(page, max_seconds, min_settle=1.2):
    """Wait until the page is past any JS/Cloudflare interstitial and shows real
    content, with a short minimum settle for XHR-rendered listings."""
    deadline = time.monotonic() + max_seconds
    settle_until = time.monotonic() + min_settle
    while True:
        try:
            ready = await page.evaluate(READY_JS)
        except Exception:
            ready = False
        if time.monotonic() > deadline or (ready and time.monotonic() > settle_until):
            await page.wait_for_timeout(400)
            return ready
        await page.wait_for_timeout(400)


async def solve_block_headful(page, req_id, reason, max_seconds=180.0):
    """In headful mode, give a human time to solve a captcha/login. Polls until
    the page reports real content (challenge cleared) or the budget runs out. The
    persistent profile then remembers the solved state for future requests."""
    note("blocked by %s — solve it in the browser window; waiting up to %ds…"
         % (reason, int(max_seconds)), "warn", req_id)
    deadline = time.monotonic() + max_seconds
    while time.monotonic() < deadline:
        try:
            if await page.evaluate(READY_JS):
                note("challenge cleared — continuing", "info", req_id)
                await page.wait_for_timeout(600)
                return True
        except Exception:
            pass
        await page.wait_for_timeout(1000)
    return False


async def solve_block_interactive(page, req_id, kind, reason, host, control_q, max_seconds=900.0):
    """Interactive block handling: raise an `attention` event so the Go side can
    prompt the user, then wait until EITHER the page clears on its own (user
    signed in / solved the check) OR the user answers via a control message
    ("resume" to continue, "skip" to give up). Returns "ready"/"resume"/"skip"/
    "timeout". The tab stays open the whole time so the user can interact with it."""
    async with _emit_lock:
        emit({"id": req_id, "event": "attention", "kind": kind, "reason": reason, "host": host})
    note("blocked by %s on %s — waiting for you to sign in / solve it in the window…"
         % (reason, host), "warn", req_id)
    deadline = time.monotonic() + max_seconds
    while time.monotonic() < deadline:
        try:
            if await page.evaluate(READY_JS):
                note("%s: looks signed in — continuing" % host, "info", req_id)
                await page.wait_for_timeout(600)
                return "ready"
        except Exception:
            pass
        if control_q is not None:
            try:
                msg = await asyncio.wait_for(control_q.get(), timeout=1.0)
                action = (msg or {}).get("control")
                if action == "resume":
                    await page.wait_for_timeout(400)
                    return "resume"
                if action == "skip":
                    return "skip"
            except asyncio.TimeoutError:
                pass
        else:
            await asyncio.sleep(1.0)
    return "timeout"


async def handle(context, headful, req, control_q=None):
    # Connect-account capture is its own flow: it keeps the page open while the
    # user signs in, then returns the captured session instead of screenshots/HTML.
    if (req.get("mode") or "") == "connect":
        return await handle_connect(context, req)

    url = req.get("url", "")
    cookie = req.get("cookie", "") or ""
    req_id = req.get("id")
    try:
        max_screens = int(req.get("maxScreens") or 3)
    except (TypeError, ValueError):
        max_screens = 3
    if max_screens < 1:
        max_screens = 1

    # Per-request cookies are added to the (shared persistent) context. They are
    # domain-scoped by url, so concurrent requests to different boards don't leak
    # cookies into each other.
    if cookie:
        try:
            await context.add_cookies(parse_cookies(cookie, url))
        except Exception as e:
            log("add_cookies: %s" % e)

    page = await context.new_page()
    try:
        try:
            await page.goto(url, wait_until="domcontentloaded", timeout=45000)
        except Exception as e:
            # Some anti-bot setups abort the main navigation (net::ERR_ABORTED) or
            # redirect mid-load; the page often still has usable content. Retry once
            # with the more tolerant "commit" wait, then read whatever loaded rather
            # than failing the whole request.
            log("goto %s: %s" % (url, e))
            try:
                await page.goto(url, wait_until="commit", timeout=30000)
            except Exception as e2:
                log("goto retry %s: %s" % (url, e2))
        # Give a JS bot-challenge (Cloudflare "Just a moment", Indeed security
        # check) generous time to auto-solve in a real browser before reading.
        ready = await wait_for_content(page, 18.0)

        try:
            title = await page.title()
        except Exception:
            title = ""
        try:
            body_text = await page.evaluate(TEXT_JS)
        except Exception:
            body_text = ""

        kind, reason = classify_block(title, body_text)
        # A page that ended up "ready" with real content isn't really blocked even
        # if the title still carries a stray "sign in" link — trust ready there.
        if kind and not ready:
            host = urlparse(url).netloc or url
            if headful and req.get("interactive"):
                # Prompt the user and wait (the tab stays open) until they sign in /
                # solve it, then re-read the page. Skip/timeout leaves it blocked.
                result = await solve_block_interactive(page, req_id, kind, reason, host, control_q)
                if result in ("ready", "resume"):
                    try:
                        title = await page.title()
                    except Exception:
                        pass
                    try:
                        body_text = await page.evaluate(TEXT_JS)
                    except Exception:
                        pass
                    kind, reason = classify_block(title, body_text)
            elif headful:
                # No interactive prompt wired: brief autonomous wait for a self-solve.
                if await solve_block_headful(page, req_id, reason):
                    kind, reason = "", ""
                    try:
                        title = await page.title()
                    except Exception:
                        pass
            else:
                note("%s detected (%r) — headless can't solve it; "
                     "connect this account or switch to headful/visible mode"
                     % (reason, title), "warn", req_id)

        # HTML mode: return the fully-rendered page source for the HTML scrapers
        # to parse (JSON-LD etc.) — no screenshots, much cheaper than vision.
        if (req.get("mode") or "shots") == "html":
            # Many boards lazy-load listings as you scroll, so a fresh page.content()
            # only carries the first screenful. Scroll a few times (stopping early at
            # the bottom) to pull deeper results into the DOM before reading it.
            try:
                for _ in range(int(req.get("scrolls") or 4)):
                    at_bottom = await page.evaluate(SCROLL_JS)
                    await page.wait_for_timeout(600)
                    if at_bottom:
                        break
            except Exception:
                pass
            try:
                html = await page.content()
            except Exception:
                html = ""
            return {"html": html, "title": title, "blocked": kind, "blockReason": reason}

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
            await page.wait_for_timeout(900)
            if at_bottom:
                break

        try:
            links = await page.evaluate(LINK_JS)
        except Exception:
            links = []
        try:
            text = await page.evaluate(TEXT_JS)
        except Exception:
            text = body_text
        return {"images": images, "links": links, "title": title, "text": text,
                "blocked": kind, "blockReason": reason}
    finally:
        try:
            await page.close()
        except Exception:
            pass


async def run_request(context, headful, sem, req):
    """Run one request under the concurrency semaphore and emit its response. A
    per-request control queue lets the Go side deliver mid-request resume/skip
    answers to an interactive block prompt."""
    req_id = req.get("id")
    control_q = None
    if req_id is not None:
        control_q = asyncio.Queue()
        CONTROLS[req_id] = control_q
    try:
        async with sem:
            try:
                resp = await handle(context, headful, req, control_q)
            except Exception as e:
                resp = {"error": str(e)}
    finally:
        if req_id is not None:
            CONTROLS.pop(req_id, None)
    if req_id is not None:
        resp["id"] = req_id
    async with _emit_lock:
        emit(resp)


def find_real_browsers():
    """Locate installed consumer Chromium browsers to drive instead of Playwright's
    bundled Chromium. A real, user-installed browser carries a genuine fingerprint
    (TLS/JA3, fonts, GPU, a real UA + matching client hints) that anti-bot walls
    and SSO "is this browser secure?" checks trust far more than headless Chromium.

    Preference: Google Chrome, then Microsoft Edge (always present on Windows).
    **Brave is deliberately NOT used**: its privacy Shields block third-party
    cookies and challenge scripts by default, which makes a Cloudflare Turnstile /
    managed challenge unsolvable — it just re-prompts forever (the "infinite
    captcha" seen on ZipRecruiter). Chrome/Edge solve those challenges normally.
    We drive our own user-data dir, so this never touches the everyday profile.
    Returns an ordered preference list of (label, executable_path)."""
    import os.path

    out = []
    seen = set()

    def add(label, path):
        if path and path not in seen and os.path.isfile(path):
            seen.add(path)
            out.append((label, path))

    # Windows install locations (Program Files / x86 / per-user LocalAppData).
    for base in (os.environ.get("ProgramFiles", ""),
                 os.environ.get("ProgramFiles(x86)", ""),
                 os.environ.get("LOCALAPPDATA", "")):
        if not base:
            continue
        add("Google Chrome", os.path.join(base, "Google", "Chrome", "Application", "chrome.exe"))
        add("Microsoft Edge", os.path.join(base, "Microsoft", "Edge", "Application", "msedge.exe"))

    # macOS / Linux fallbacks.
    add("Google Chrome", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
    add("Microsoft Edge", "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge")
    add("Google Chrome", "/usr/bin/google-chrome")
    add("Google Chrome", "/usr/bin/google-chrome-stable")
    add("Microsoft Edge", "/usr/bin/microsoft-edge")
    return out


async def launch_context(pw, headful, profile_dir):
    """Build the browser context: a persistent on-disk profile when profile_dir is
    given (so logins / Cloudflare clearance survive), else an ephemeral one.

    Drives a real installed browser (Chrome > Edge) when one is found, falling
    back to Playwright's bundled Chromium (Brave is excluded — see
    find_real_browsers). With a real browser we keep its native User-Agent +
    client hints (overriding them would only create a tell); only the
    bundled-Chromium fallback gets the DEFAULT_UA mask so it never advertises
    "HeadlessChrome"."""
    # --disable-blink-features=AutomationControlled drops the navigator.webdriver
    # flag; dropping the --enable-automation default switch removes the other tell
    # SSO/bot checks read (and the "controlled by automated software" infobar).
    args = ["--disable-blink-features=AutomationControlled", "--lang=en-US",
            "--no-default-browser-check", "--no-first-run"]
    ignore = ["--enable-automation"]
    common = dict(
        locale="en-US",
        timezone_id="America/New_York",
        extra_http_headers={"Accept-Language": "en-US,en;q=0.9"},
    )
    if headful:
        # In a REAL (headful) window the page must track the OS window, not a fixed
        # viewport. A forced viewport in headful breaks DPI scaling, hides the
        # scrollbar, and — worst — sizes SSO OAuth popups wrong so you can't reach
        # the sign-in button (the bug that blocked Monster's SSO). no_viewport +
        # a sane default window size behaves like a normal browser.
        common["no_viewport"] = True
        args.append("--window-size=1280,1000")
    else:
        # Headless: a tall fixed viewport captures more of the page per screenshot.
        common["viewport"] = {"width": 1280, "height": 1800}

    # Try each real browser in turn, then bundled Chromium. Real browsers keep
    # their native UA; only Chromium is masked with DEFAULT_UA.
    attempts = [(label, {"executable_path": path}, False) for label, path in find_real_browsers()]
    attempts.append(("bundled Chromium", {}, True))

    last_err = None
    for label, extra, mask_ua in attempts:
        kwargs = dict(common)
        if mask_ua:
            kwargs["user_agent"] = DEFAULT_UA
        try:
            if profile_dir:
                context = await pw.chromium.launch_persistent_context(
                    profile_dir, headless=not headful, args=args,
                    ignore_default_args=ignore, **extra, **kwargs
                )
                note("stealth browser: driving %s" % label, "info")
                return None, context
            browser = await pw.chromium.launch(
                headless=not headful, args=args, ignore_default_args=ignore, **extra
            )
            context = await browser.new_context(**kwargs)
            note("stealth browser: driving %s" % label, "info")
            return browser, context
        except Exception as e:
            last_err = e
            log("launch via %s failed: %s" % (label, e))
    raise last_err or RuntimeError("no browser could be launched")


async def serve(headful, profile_dir, concurrency):
    try:
        from playwright.async_api import async_playwright
        from pw_stealth_enhanced import apply_stealth, StealthConfig
    except Exception as e:
        emit({"error": "import failed (install playwright + pw-stealth-enhanced): %s" % e})
        return

    pw = await async_playwright().start()
    try:
        browser, context = await launch_context(pw, headful, profile_dir)
    except Exception as e:
        emit({"error": "browser launch failed (run: playwright install chromium): %s" % e})
        await pw.stop()
        return

    try:
        await apply_stealth(context, config=StealthConfig(locale="en-US", timezone_id="America/New_York"))
    except Exception as e:
        log("apply_stealth: %s" % e)

    emit({"ready": True})

    sem = asyncio.Semaphore(max(1, concurrency))
    tasks = set()
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
            # A control message answers an in-flight request's block prompt; route
            # it to that request's queue rather than starting a new request.
            if "control" in req:
                q = CONTROLS.get(req.get("id"))
                if q is not None:
                    q.put_nowait(req)
                continue
            # Fire-and-forget: the task emits its own id-tagged response, so many
            # requests run concurrently (each in its own tab, bounded by sem).
            t = asyncio.create_task(run_request(context, headful, sem, req))
            tasks.add(t)
            t.add_done_callback(tasks.discard)
    finally:
        for t in list(tasks):
            t.cancel()
        if tasks:
            await asyncio.gather(*tasks, return_exceptions=True)
        try:
            await context.close()
        except Exception:
            pass
        if browser is not None:
            try:
                await browser.close()
            except Exception:
                pass
        try:
            await pw.stop()
        except Exception:
            pass


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--headful", action="store_true")
    parser.add_argument("--profile", default="")
    parser.add_argument("--concurrency", type=int, default=4)
    args, _ = parser.parse_known_args()
    try:
        asyncio.run(serve(args.headful, args.profile.strip(), args.concurrency))
    except KeyboardInterrupt:
        pass
    except Exception as e:
        emit({"error": "sidecar crashed: %s" % e})


if __name__ == "__main__":
    main()
