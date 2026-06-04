package browser

// This file adds vision-search support: instead of hitting a board's scrape
// endpoint over plain HTTP (which gets rate-limited / Cloudflare-blocked), we
// drive a real browser to the board's normal search-results page like a human
// would, screenshot the rendered page, and let a vision-capable AI model read
// the listings off the images. A Session keeps one browser open across several
// page loads (one per board/role), which is far cheaper than relaunching.

import (
	"context"
	"errors"
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
	// Blocked is "captcha", "login" or "cloudflare" when the page was identified
	// as a bot/sign-in wall the load couldn't get past (empty when content loaded
	// normally). BlockReason is a short human description for logging.
	Blocked     string
	BlockReason string
}

// BlockedError is returned by RenderHTML when the page it loaded is a bot/sign-in
// wall rather than real content, so callers can report a clear reason (and skip a
// pointless plain-HTTP retry that the stealth browser already couldn't beat).
type BlockedError struct {
	Host   string
	Kind   string // "captcha" | "login" | "cloudflare"
	Reason string
}

func (e *BlockedError) Error() string {
	host := e.Host
	if host == "" {
		host = "the site"
	}
	return fmt.Sprintf("%s is blocking automated access (%s) — reconnect the account in Settings, switch on \"Show the browser window\" to solve it once, or try again later", host, e.Reason)
}

// IsBlocked reports whether err is (or wraps) a BlockedError.
func IsBlocked(err error) bool {
	var b *BlockedError
	return errors.As(err, &b)
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
	// MaxConcurrency caps how many pages the Python stealth sidecar loads at once
	// (it drives several boards/roles in parallel tabs). <=0 uses a default. The
	// built-in chromedp engine ignores this — it has a single target and stays
	// serial.
	MaxConcurrency int
	// Notify, when set, receives out-of-band progress/diagnostic notes from the
	// engine (e.g. "blocked by a Cloudflare bot check — solve it in the window").
	// level is "info" or "warn". Nil is fine.
	Notify func(level, msg string)
	// OnBlock, when set, is called when a load hits a sign-in/captcha wall the
	// browser can't clear on its own (headful only). It should surface a prompt to
	// the user and BLOCK until they've dealt with it, returning true to keep
	// scraping the page or false to skip it. The page is kept open while it blocks
	// so the user can interact with the visible window. nil = don't wait (just
	// report the block and move on). host/kind/reason describe what was hit.
	OnBlock func(ctx context.Context, host, kind, reason string) bool
}

// Session is a reusable real-browser session. Open it once, load several pages
// with Shots, then Close it.
type Session struct {
	profileDir  string
	persistent  bool // ProfileDir was supplied; don't delete it on Close
	headful     bool
	notify      func(level, msg string)
	onBlock     func(ctx context.Context, host, kind, reason string) bool
	allocCancel context.CancelFunc
	taskCancel  context.CancelFunc
	ctx         context.Context
}

// notifyf reports an out-of-band note via the session's Notify hook, if set.
func (s *Session) notifyf(level, format string, args ...any) {
	if s.notify != nil {
		s.notify(level, fmt.Sprintf(format, args...))
	}
}

// callTimeout bounds a single page load. Normally short, but generous when an
// interactive block handler is wired (headful), since a load may then pause for
// the user to sign in / solve a captcha.
func (s *Session) callTimeout() time.Duration {
	if s.headful && s.onBlock != nil {
		return 17 * time.Minute
	}
	return 70 * time.Second
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
		notify:      opts.Notify,
		onBlock:     opts.OnBlock,
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

// navigate applies the UA/cookie overrides, loads pageURL on runCtx, and waits
// for the page to be past any Cloudflare/JS interstitial. Shared by Shots and
// RenderHTML. UA rules: replay a connected account's UA when given (its
// cf_clearance is bound to it); otherwise, headless, override the UA so it no
// longer advertises "HeadlessChrome" (the cheapest bot tell) with matching
// client-hint metadata. Headful already sends a real browser UA.
func (s *Session) navigate(runCtx context.Context, pageURL, cookieHeader, userAgent string) error {
	setup := []chromedp.Action{network.Enable()}
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
		return fmt.Errorf("load %s: %w", hostOnly(pageURL), err)
	}
	// A headful browser solves the "Just a moment…" challenge on its own within a
	// few seconds; wait for real content before reading the page.
	ready := waitForContent(runCtx, 15*time.Second)

	// If we still landed on a captcha/sign-in wall and a human can see the window,
	// pause so they can solve it once — the persistent profile then remembers it.
	if !ready && s.headful {
		var title, text string
		_ = chromedp.Run(runCtx, chromedp.Evaluate(`document.title || ""`, &title))
		_ = chromedp.Run(runCtx, chromedp.Evaluate(`((document.body && document.body.innerText) || "").replace(/\s+/g, ' ').trim().slice(0, 600)`, &text))
		if kind, reason := classifyBlock(title, text); reason != "" {
			host := hostOnly(pageURL)
			if s.onBlock != nil {
				// Interactive: prompt the user and block until they've signed in / solved
				// it and clicked Continue (or Skip). The browser process stays responsive
				// while we wait (we simply stop issuing commands), so the user can act in
				// the visible window; the persistent profile then remembers it.
				s.notifyf("warn", "%s on %s — sign in / solve it in the browser window, then click Continue", reason, host)
				if s.onBlock(runCtx, host, kind, reason) {
					waitForContent(runCtx, 10*time.Second) // let the post-sign-in page settle
				}
			} else {
				// No interactive handler (e.g. headless wiring absent): brief autonomous
				// wait, then carry on with whatever rendered.
				s.notifyf("warn", "%s on %s — solve it in the browser window; waiting up to 180s…", reason, host)
				if waitForSolve(runCtx, 180*time.Second) {
					s.notifyf("info", "challenge cleared on %s — continuing", host)
				}
			}
		}
	}
	return nil
}

// RenderHTML loads pageURL in this session (replaying cookie + UA, waiting out
// any bot challenge) and returns the fully-rendered page HTML — letting the HTML
// scraping sources reuse the stealth browser instead of plain HTTP.
func (s *Session) RenderHTML(ctx context.Context, pageURL, cookieHeader, userAgent string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	runCtx, cancel := context.WithTimeout(s.ctx, s.callTimeout())
	defer cancel()
	if err := s.navigate(runCtx, pageURL, cookieHeader, userAgent); err != nil {
		return "", err
	}
	var html, title, text string
	if err := chromedp.Run(runCtx, chromedp.OuterHTML("html", &html, chromedp.ByQuery)); err != nil {
		return "", fmt.Errorf("read html %s: %w", hostOnly(pageURL), err)
	}
	// If what rendered is a bot/sign-in wall (not real content), report it as a
	// BlockedError so the source surfaces a clear reason instead of "no results".
	_ = chromedp.Run(runCtx,
		chromedp.Evaluate(`document.title || ""`, &title),
		chromedp.Evaluate(`((document.body && document.body.innerText) || "").replace(/\s+/g, ' ').trim().slice(0, 600)`, &text),
	)
	if kind, reason := classifyBlock(title, text); reason != "" {
		return html, &BlockedError{Host: hostOnly(pageURL), Kind: kind, Reason: reason}
	}
	return html, nil
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
	runCtx, cancel := context.WithTimeout(s.ctx, s.callTimeout())
	defer cancel()

	if err := s.navigate(runCtx, pageURL, cookieHeader, userAgent); err != nil {
		return Shots{}, err
	}

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
	out.Blocked, out.BlockReason = classifyBlock(out.Title, out.Text)
	return out, nil
}

// classifyBlock returns a kind ("captcha"/"login"/"cloudflare") and a short human
// reason when a page title/text matches a known bot-check/captcha interstitial or
// a sign-in wall, else ("",""). Captcha phrases are matched anywhere; sign-in
// phrases only in the title (a real results page carries "sign in" links in body
// text too). Mirrors classify_block in the Python sidecar.
func classifyBlock(title, text string) (kind, reason string) {
	t := strings.ToLower(title)
	hay := strings.ToLower(title + "\n" + text)
	for _, c := range []struct{ needle, kind, reason string }{
		{"just a moment", "cloudflare", "a Cloudflare bot check"},
		{"checking your browser", "cloudflare", "a Cloudflare browser check"},
		{"attention required", "cloudflare", "a Cloudflare block"},
		{"enable javascript and cookies", "cloudflare", "a Cloudflare JS/cookie check"},
		{"verify you are human", "captcha", "a human-verification check"},
		{"verifying you are human", "captcha", "a human-verification check"},
		{"are you a human", "captcha", "a human-verification check"},
		{"are you a robot", "captcha", "a bot check"},
		{"px-captcha", "captcha", "a PerimeterX captcha"},
		{"press & hold", "captcha", "a press-and-hold bot check"},
		{"press and hold", "captcha", "a press-and-hold bot check"},
		{"unusual traffic", "captcha", "a rate-limit / bot check"},
		{"security check", "captcha", "a security check"},
		// Hard IP/bot walls (Imperva/Incapsula, Distil) — unambiguous phrases only,
		// so a real listing that merely mentions "access denied" isn't misflagged.
		{"pardon our interruption", "captcha", "an Imperva bot wall"},
		{"request unsuccessful. incapsula", "captcha", "an Incapsula/Imperva bot wall"},
		{"powered by distil", "captcha", "a Distil bot wall"},
		{"access to this page has been denied", "captcha", "an access-denied block"},
		{"your request has been blocked", "captcha", "an IP/bot block"},
		{"you have been blocked", "captcha", "an IP/bot block"},
	} {
		if strings.Contains(hay, c.needle) {
			return c.kind, c.reason
		}
	}
	for _, c := range []struct{ needle, reason string }{
		{"sign in", "a sign-in wall"},
		{"sign up", "a sign-up wall"},
		{"log in", "a sign-in wall"},
		{"login", "a sign-in wall"},
		{"join linkedin", "a LinkedIn sign-in wall"},
	} {
		if strings.Contains(t, c.needle) {
			return "login", c.reason
		}
	}
	return "", ""
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
  const blocked = /just a moment|attention required|checking your browser|verify (you|that you)|are you a human|enable javascript and cookies|security check|press & hold|verifying you are human/.test(t);
  const len = document.body ? document.body.innerText.length : 0;
  return document.readyState === 'complete' && !blocked && len > 200;
})()`

// waitForContent polls until the page is ready (past any interstitial, real
// content painted) with a brief minimum settle for XHR-rendered listings, or
// until max elapses. It returns whether the page reached the ready state (false
// means it timed out, likely still on an interstitial). Best-effort: the caller
// screenshots whatever rendered regardless.
func waitForContent(ctx context.Context, max time.Duration) bool {
	deadline := time.Now().Add(max)
	minSettle := time.Now().Add(2 * time.Second)
	for {
		var ready bool
		_ = chromedp.Run(ctx, chromedp.Evaluate(contentReadyJS, &ready))
		if ctx.Err() != nil || time.Now().After(deadline) {
			_ = chromedp.Run(ctx, chromedp.Sleep(500*time.Millisecond))
			return ready
		}
		if ready && time.Now().After(minSettle) {
			_ = chromedp.Run(ctx, chromedp.Sleep(500*time.Millisecond))
			return true
		}
		_ = chromedp.Run(ctx, chromedp.Sleep(500*time.Millisecond))
	}
}

// waitForSolve polls until the page reports real content (a human has cleared a
// captcha/sign-in) or max elapses. Returns true if it cleared.
func waitForSolve(ctx context.Context, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		var ready bool
		_ = chromedp.Run(ctx, chromedp.Evaluate(contentReadyJS, &ready))
		if ready {
			_ = chromedp.Run(ctx, chromedp.Sleep(600*time.Millisecond))
			return true
		}
		_ = chromedp.Run(ctx, chromedp.Sleep(1*time.Second))
	}
	return false
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
