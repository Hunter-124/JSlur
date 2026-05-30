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
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
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
// links discovered on it.
type Shots struct {
	Images [][]byte
	Links  []Link
}

// SessionOptions configures a vision-search Session.
type SessionOptions struct {
	// Headful shows the browser window while searching. The default (false) runs
	// headless with the AutomationControlled flag disabled, which still looks like
	// a normal browser to the cheap bot checks.
	Headful bool
}

// Session is a reusable real-browser session. Open it once, load several pages
// with Shots, then Close it.
type Session struct {
	tmp         string
	allocCancel context.CancelFunc
	taskCancel  context.CancelFunc
	ctx         context.Context
}

// NewSession launches a browser for vision search. Call Close when finished.
func NewSession(ctx context.Context, opts SessionOptions) (*Session, error) {
	tmp, err := os.MkdirTemp("", "autoapply-vision-*")
	if err != nil {
		return nil, err
	}

	allocOpts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-sync", true),
		chromedp.UserDataDir(tmp),
		chromedp.WindowSize(1280, 1800),
	}
	if !opts.Headful {
		allocOpts = append(allocOpts, chromedp.Headless, chromedp.DisableGPU)
	}
	if p := browserPath(); p != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(p))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, allocOpts...)
	taskCtx, taskCancel := chromedp.NewContext(allocCtx)

	// Boot the browser now so a missing Chrome/Edge surfaces as an error here
	// rather than on the first navigation.
	bootCtx, cancelBoot := context.WithTimeout(taskCtx, 30*time.Second)
	defer cancelBoot()
	if err := chromedp.Run(bootCtx, chromedp.Navigate("about:blank")); err != nil {
		taskCancel()
		allocCancel()
		_ = os.RemoveAll(tmp)
		return nil, fmt.Errorf("launch browser: %w", err)
	}

	return &Session{tmp: tmp, allocCancel: allocCancel, taskCancel: taskCancel, ctx: taskCtx}, nil
}

// Close shuts the browser down and removes its throwaway profile.
func (s *Session) Close() {
	if s.taskCancel != nil {
		s.taskCancel()
	}
	if s.allocCancel != nil {
		s.allocCancel()
	}
	if s.tmp != "" {
		_ = os.RemoveAll(s.tmp)
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
	if userAgent != "" {
		setup = append(setup, emulation.SetUserAgentOverride(userAgent))
	}
	if cookies := parseCookieHeader(cookieHeader, pageURL); len(cookies) > 0 {
		setup = append(setup, network.SetCookies(cookies))
	}
	setup = append(setup,
		chromedp.Navigate(pageURL),
		chromedp.Sleep(3500*time.Millisecond),
	)
	if err := chromedp.Run(runCtx, setup...); err != nil {
		return Shots{}, fmt.Errorf("load %s: %w", hostOnly(pageURL), err)
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
