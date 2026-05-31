package browser

// This file adds vision-search support: instead of hitting a board's scrape
// endpoint over plain HTTP (which gets rate-limited / Cloudflare-blocked), we
// drive a real browser to the board's normal search-results page like a human
// would, screenshot the rendered page, and let a vision-capable AI model read
// the listings off the images. A Session keeps one browser open across several
// page loads (one per board/role), which is far cheaper than relaunching.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Link is a hyperlink found on a rendered page: its visible text and absolute
// URL. The vision source passes these to the model so it can attach a real
// posting URL to each listing it reads off a screenshot.
type Link struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

// Shots is the visual capture of one page: the screenshots (PNG bytes) plus the
// links discovered on it and a little diagnostic context (page title + a snippet
// of visible text) the caller can use to recognise a block/login wall.
type Shots struct {
	Images [][]byte
	Links  []Link
	Title  string // document.title of the loaded page
	Text   string // short snippet of visible body text, for diagnostics
}

// SessionOptions configures a vision-search Session.
type SessionOptions struct {
	// Headful shows the browser window while searching. Headful is markedly harder
	// for boards to detect than headless (a real window, a real UA, GPU, etc.), so
	// it's the better choice for bot-walled boards like Indeed/Google/LinkedIn.
	Headful bool
	// ProfileDir, when set, is a persistent Chrome user-data dir reused across runs
	// instead of a throwaway one. This lets a solved Cloudflare challenge
	// (cf_clearance) and a warmed-up fingerprint survive between searches, which is
	// what gets past bot walls on boards with no connected account. Empty uses a
	// throwaway profile that's deleted on Close. (Built-in chromedp engine only.)
	ProfileDir string
	// Engine selects the page-capture backend via NewShooter: "" / "chromedp" uses
	// the built-in browser; "python" uses the Playwright stealth sidecar (stronger
	// anti-bot, needs local Python + playwright + pw-stealth-enhanced).
	Engine string
	// PythonPath is the Python executable for the stealth sidecar. Empty = "python".
	PythonPath string
}

// Session is a reusable real-browser session. Open it once, load several pages
// with Shots, then Close it.
type Session struct {
	profileDir  string
	persistent  bool // ProfileDir was supplied; don't delete it on Close
	headful     bool
	allocCancel context.CancelFunc
	taskCancel  context.CancelFunc
	ctx         context.Context
}

// NewSession launches a browser for vision search. Call Close when finished.
func NewSession(ctx context.Context, opts SessionOptions) (*Session, error) {
	profileDir, persistent := opts.ProfileDir, opts.ProfileDir != ""
	if persistent {
		if err := os.MkdirAll(profileDir, 0o755); err != nil {
			return nil, err
		}
		// Clear stale Chrome singleton locks. We reuse this profile for every
		// search; a lock left behind by a crash (no clean Close) would otherwise
		// make every future launch fail. Searches run one at a time, so no live
		// browser owns these.
		for _, name := range []string{"SingletonLock", "SingletonCookie", "SingletonSocket"} {
			_ = os.Remove(filepath.Join(profileDir, name))
		}
	} else {
		tmp, err := os.MkdirTemp("", "autoapply-vision-*")
		if err != nil {
			return nil, err
		}
		profileDir = tmp
	}

	allocOpts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		// The single biggest anti-detection win: drop the navigator.webdriver flag.
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("lang", "en-US"),
		chromedp.UserDataDir(profileDir),
		chromedp.WindowSize(1280, 1800),
	}
	if !opts.Headful {
		// New headless is far less fingerprintable than the legacy mode (and pairs
		// with the UA + client-hint override in Shots to hide the "Headless" tell).
		allocOpts = append(allocOpts, chromedp.Flag("headless", "new"), chromedp.DisableGPU)
	}
	if p := browserPath(); p != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(p))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, allocOpts...)
	taskCtx, taskCancel := chromedp.NewContext(allocCtx)

	cleanup := func() {
		taskCancel()
		allocCancel()
		if !persistent {
			_ = os.RemoveAll(profileDir)
		}
	}

	// Boot the browser now so a missing Chrome/Edge surfaces as an error here
	// rather than on the first navigation. Crucially, boot on the session's own
	// long-lived context (taskCtx): chromedp ties the browser process to the
	// context that first triggers allocation, so booting on a short-lived child
	// and cancelling it (as this used to) kills the browser and makes every later
	// Shots fail with "context canceled". A watchdog bounds the boot instead.
	booted := make(chan error, 1)
	go func() { booted <- chromedp.Run(taskCtx, chromedp.Navigate("about:blank")) }()
	select {
	case err := <-booted:
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("launch browser: %w", err)
		}
	case <-time.After(45 * time.Second):
		cleanup()
		return nil, fmt.Errorf("launch browser: timed out starting Edge/Chrome")
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	}

	// Install the stealth script so it runs before any page's own scripts on every
	// later navigation (masks the automation tells bot checks look for). Best-effort.
	_ = chromedp.Run(taskCtx, chromedp.ActionFunc(func(c context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(stealthJS).Do(c)
		return err
	}))

	return &Session{
		profileDir:  profileDir,
		persistent:  persistent,
		headful:     opts.Headful,
		allocCancel: allocCancel,
		taskCancel:  taskCancel,
		ctx:         taskCtx,
	}, nil
}

// Close shuts the browser down and removes its throwaway profile (a persistent
// ProfileDir is kept so its Cloudflare clearance can be reused next time).
func (s *Session) Close() {
	if s.taskCancel != nil {
		s.taskCancel()
	}
	if s.allocCancel != nil {
		s.allocCancel()
	}
	if s.profileDir != "" && !s.persistent {
		_ = os.RemoveAll(s.profileDir)
	}
}

// Shots loads pageURL, replaying the given cookie header + User-Agent so the
// request looks like a signed-in human, waits for the page to settle, then
// scrolls while capturing up to maxScreens viewport screenshots and finally
// collects the page's links. cookieHeader and userAgent may be empty.
func (s *Session) Shots(ctx context.Context, pageURL, cookieHeader, userAgent string, maxScreens int) (Shots, error) {
	if err := ctx.Err(); err != nil {
		return Shots{}, err
	}
	if maxScreens <= 0 {
		maxScreens = 3
	}

	// chromedp actions must run against the session's browser target, so derive
	// the run context from s.ctx (itself bounded by the caller's search context).
	runCtx, cancel := context.WithTimeout(s.ctx, 70*time.Second)
	defer cancel()

	setup := []chromedp.Action{network.Enable()}
	// User-Agent: replay a connected account's UA when we have one (its cf_clearance
	// is bound to that UA). Otherwise, in headless mode, override the UA so it no
	// longer advertises "HeadlessChrome" — the cheapest bot tell of all — with
	// matching client-hint metadata. Headful already sends a real browser UA.
	if userAgent != "" {
		setup = append(setup, emulation.SetUserAgentOverride(userAgent))
	} else if !s.headful {
		setup = append(setup, emulation.SetUserAgentOverride(stealthUA).
			WithAcceptLanguage("en-US,en;q=0.9").
			WithUserAgentMetadata(stealthUAMetadata()))
	}
	if cookies := parseCookieHeader(cookieHeader, pageURL); len(cookies) > 0 {
		setup = append(setup, network.SetCookies(cookies))
	}
	setup = append(setup, chromedp.Navigate(pageURL))
	if err := chromedp.Run(runCtx, setup...); err != nil {
		return Shots{}, fmt.Errorf("load %s: %w", hostOnly(pageURL), err)
	}

	// Wait for the page to actually be ready — past any Cloudflare/JS interstitial
	// and showing real content — before screenshotting. A headful browser solves
	// the "Just a moment…" challenge on its own within a few seconds; the old fixed
	// 3.5s sleep often captured the challenge page itself.
	waitForContent(runCtx, 15*time.Second)

	var out Shots
	for i := 0; i < maxScreens; i++ {
		var buf []byte
		if err := chromedp.Run(runCtx, chromedp.CaptureScreenshot(&buf)); err != nil {
			break
		}
		if len(buf) > 0 {
			out.Images = append(out.Images, buf)
		}
		if i == maxScreens-1 {
			break
		}
		// Scroll one viewport; stop early if we've reached the bottom.
		var atBottom bool
		if err := chromedp.Run(runCtx, chromedp.Evaluate(`(() => {
			const before = window.scrollY;
			window.scrollBy(0, Math.round(window.innerHeight * 0.9));
			return Math.abs(window.scrollY - before) < 4;
		})()`, &atBottom)); err != nil {
			break
		}
		_ = chromedp.Run(runCtx, chromedp.Sleep(1200*time.Millisecond))
		if atBottom {
			break
		}
	}

	// Best-effort: collect links so the vision source can attach real URLs to the
	// listings the model reads off the screenshots. Failure here is non-fatal.
	_ = chromedp.Run(runCtx, chromedp.Evaluate(linkCollectorJS, &out.Links))

	// Best-effort diagnostics: the page title and a snippet of visible text let
	// the caller spot a Cloudflare/captcha interstitial or a sign-in wall (which
	// otherwise just look like "0 jobs found"). Non-fatal.
	_ = chromedp.Run(runCtx, chromedp.Evaluate(`document.title || ""`, &out.Title))
	_ = chromedp.Run(runCtx, chromedp.Evaluate(`((document.body && document.body.innerText) || "").replace(/\s+/g, ' ').trim().slice(0, 600)`, &out.Text))
	return out, nil
}

// linkCollectorJS returns up to 400 unique {text,url} links on the page.
const linkCollectorJS = `(() => {
  const seen = new Set(), out = [];
  for (const a of document.querySelectorAll('a[href]')) {
    const url = a.href;
    if (!url || seen.has(url)) continue;
    seen.add(url);
    const text = (a.innerText || a.getAttribute('aria-label') || '').replace(/\s+/g, ' ').trim().slice(0, 120);
    out.push({text, url});
    if (out.length >= 400) break;
  }
  return out;
})()`

// stealthUA is a current, real desktop-Chrome User-Agent used for headless
// searches so the request never advertises "HeadlessChrome". It is paired with
// stealthUAMetadata so the sec-ch-ua client hints stay consistent with it.
const stealthUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// stealthUAMetadata returns client-hint metadata matching stealthUA. A UA string
// without matching client hints is itself a detection signal, so we set both.
func stealthUAMetadata() *emulation.UserAgentMetadata {
	return &emulation.UserAgentMetadata{
		Brands: []*emulation.UserAgentBrandVersion{
			{Brand: "Chromium", Version: "131"},
			{Brand: "Google Chrome", Version: "131"},
			{Brand: "Not_A Brand", Version: "24"},
		},
		FullVersionList: []*emulation.UserAgentBrandVersion{
			{Brand: "Chromium", Version: "131.0.6778.86"},
			{Brand: "Google Chrome", Version: "131.0.6778.86"},
			{Brand: "Not_A Brand", Version: "24.0.0.0"},
		},
		Platform:        "Windows",
		PlatformVersion: "10.0.0",
		Architecture:    "x86",
		Bitness:         "64",
		Mobile:          false,
	}
}

// stealthJS runs before every page's own scripts and masks the tells bot checks
// probe for in headless/automated browsers (webdriver flag, missing chrome
// runtime, empty plugin/permission/WebGL data). Each tweak is wrapped in
// try/catch so a failure never breaks the page.
const stealthJS = `
(() => {
  try { Object.defineProperty(navigator, 'webdriver', { get: () => undefined }); } catch (e) {}
  try { window.chrome = window.chrome || { runtime: {} }; } catch (e) {}
  try { Object.defineProperty(navigator, 'languages', { get: () => ['en-US', 'en'] }); } catch (e) {}
  try {
    Object.defineProperty(navigator, 'plugins', { get: () => [1, 2, 3, 4, 5].map((i) => ({ name: 'Plugin ' + i })) });
  } catch (e) {}
  try {
    const q = navigator.permissions && navigator.permissions.query;
    if (q) navigator.permissions.query = (p) =>
      p && p.name === 'notifications' ? Promise.resolve({ state: Notification.permission }) : q(p);
  } catch (e) {}
  try {
    const gp = WebGLRenderingContext.prototype.getParameter;
    WebGLRenderingContext.prototype.getParameter = function (p) {
      if (p === 37445) return 'Intel Inc.';
      if (p === 37446) return 'Intel Iris OpenGL Engine';
      return gp.call(this, p);
    };
  } catch (e) {}
})();
`

// contentReadyJS reports whether the page is past a bot/JS interstitial and
// showing real content (used by waitForContent to time the screenshot right).
const contentReadyJS = `(() => {
  const t = (document.title || '').toLowerCase();
  const blocked = /just a moment|attention required|checking your browser|verify (you|that you)|are you a human|enable javascript and cookies/.test(t);
  const len = document.body ? document.body.innerText.length : 0;
  return document.readyState === 'complete' && !blocked && len > 200;
})()`

// waitForContent polls until the page is ready (past any interstitial, real
// content painted) with a brief minimum settle for XHR-rendered listings, or
// until max elapses. Best-effort: on timeout it returns and the caller
// screenshots whatever rendered.
func waitForContent(ctx context.Context, max time.Duration) {
	deadline := time.Now().Add(max)
	minSettle := time.Now().Add(2500 * time.Millisecond)
	for {
		var ready bool
		_ = chromedp.Run(ctx, chromedp.Evaluate(contentReadyJS, &ready))
		if ctx.Err() != nil || time.Now().After(deadline) || (ready && time.Now().After(minSettle)) {
			_ = chromedp.Run(ctx, chromedp.Sleep(800*time.Millisecond))
			return
		}
		_ = chromedp.Run(ctx, chromedp.Sleep(600*time.Millisecond))
	}
}

// parseCookieHeader turns a "name=value; name2=value2" Cookie header into
// chromedp cookie params bound to pageURL (so Chrome assigns the right domain,
// which is what makes a replayed cf_clearance valid).
func parseCookieHeader(header, pageURL string) []*network.CookieParam {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil
	}
	var out []*network.CookieParam
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		eq := strings.IndexByte(part, '=')
		if eq <= 0 {
			continue
		}
		name := strings.TrimSpace(part[:eq])
		if name == "" {
			continue
		}
		out = append(out, &network.CookieParam{
			Name:  name,
			Value: strings.TrimSpace(part[eq+1:]),
			URL:   pageURL,
		})
	}
	return out
}

// hostOnly returns the host of a URL for error messages, tolerating bad input.
func hostOnly(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}
