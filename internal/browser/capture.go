// Package browser drives a real Edge/Chrome session (via the DevTools protocol,
// using the pure-Go chromedp binding — no cgo, no Node) so the user can sign in
// to a job board by hand and the app can harvest the resulting session cookies.
//
// Why a real browser and not stored passwords: the user logs in themselves
// (handling 2FA/captchas), and we never see their password — only the resulting
// cookies. Capturing the browser's cookies *and* its User-Agent also lets later
// plain-HTTP scrapes replay a valid `cf_clearance`, which is what gets past the
// Cloudflare blocks on boards like ZipRecruiter and SimplyHired.
package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// Cookie is a captured browser cookie (the subset we need to replay it).
type Cookie struct {
	Name   string
	Value  string
	Domain string
	Path   string
}

// Result is the outcome of a capture: the cookies plus the browser User-Agent
// they were issued against (cf_clearance is bound to the UA, so we must replay
// the same one).
type Result struct {
	Cookies   []Cookie
	UserAgent string
}

// CookieHeader renders the cookies as a value for the HTTP `Cookie` header.
func (r Result) CookieHeader() string {
	var b strings.Builder
	for i, c := range r.Cookies {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.Name)
		b.WriteByte('=')
		b.WriteString(c.Value)
	}
	return b.String()
}

// Capture opens a visible browser with a throwaway profile at loginURL, waits
// for the user to sign in (detected when any of authCookies appears, or until
// they close the window / the timeout elapses), and returns the cookies + UA.
//
// It runs headful on purpose — the whole point is for a human to log in.
func Capture(ctx context.Context, loginURL string, authCookies []string, timeout time.Duration) (Result, error) {
	return capture(ctx, loginURL, authCookies, timeout, false)
}

// capture is the implementation; headless is used only by tests (a real login
// flow must be headful).
func capture(ctx context.Context, loginURL string, authCookies []string, timeout time.Duration, headless bool) (Result, error) {
	tmp, err := os.MkdirTemp("", "autoapply-profile-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(tmp)

	opts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-sync", true),
		chromedp.UserDataDir(tmp),
		// Note: deliberately NOT headless by default — the user needs to see the
		// page to log in.
	}
	if headless {
		opts = append(opts, chromedp.Headless, chromedp.DisableGPU)
	}
	if p := browserPath(); p != "" {
		opts = append(opts, chromedp.ExecPath(p))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()
	taskCtx, cancelTask := chromedp.NewContext(allocCtx)
	defer cancelTask()
	runCtx, cancelRun := context.WithTimeout(taskCtx, timeout)
	defer cancelRun()

	var ua string
	if err := chromedp.Run(runCtx,
		chromedp.Navigate(loginURL),
		chromedp.Evaluate(`navigator.userAgent`, &ua),
	); err != nil {
		return Result{}, fmt.Errorf("launch browser: %w", err)
	}

	last := Result{UserAgent: ua}
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	for {
		var raw []*network.Cookie
		err := chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
			got, e := network.GetCookies().Do(c)
			raw = got
			return e
		}))
		if err != nil {
			// Window closed or context ended. Return whatever we managed to grab.
			if len(last.Cookies) > 0 {
				return last, nil
			}
			return last, fmt.Errorf("browser closed before a login was detected")
		}
		last.Cookies = convert(raw)
		if hasAny(last.Cookies, authCookies) {
			return last, nil
		}
		select {
		case <-runCtx.Done():
			if len(last.Cookies) > 0 {
				return last, nil
			}
			return last, fmt.Errorf("timed out waiting for sign-in")
		case <-ticker.C:
		}
	}
}

// renderUA is a current desktop-Chrome UA used when rendering pages headlessly.
const renderUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// RenderHTML loads a URL in a headless browser, lets its JavaScript run, and
// returns the rendered HTML. Used for boards that only populate content via JS
// (e.g. Monster). It runs the real browser engine with a normal UA and the
// AutomationControlled flag disabled, which evades the cheapest bot checks.
// userAgent overrides the default when non-empty (e.g. a connected session's UA).
func RenderHTML(ctx context.Context, pageURL, userAgent string, settle time.Duration) (string, error) {
	tmp, err := os.MkdirTemp("", "autoapply-render-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if userAgent == "" {
		userAgent = renderUA
	}
	if settle <= 0 {
		settle = 3500 * time.Millisecond
	}

	opts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Headless,
		chromedp.DisableGPU,
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-extensions", true),
		chromedp.UserAgent(userAgent),
		chromedp.UserDataDir(tmp),
	}
	if p := browserPath(); p != "" {
		opts = append(opts, chromedp.ExecPath(p))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()
	taskCtx, cancelTask := chromedp.NewContext(allocCtx)
	defer cancelTask()
	runCtx, cancelRun := context.WithTimeout(taskCtx, 45*time.Second)
	defer cancelRun()

	var html string
	err = chromedp.Run(runCtx,
		chromedp.Navigate(pageURL),
		chromedp.Sleep(settle),
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
	)
	if err != nil {
		return "", fmt.Errorf("render failed: %w", err)
	}
	return html, nil
}

func convert(raw []*network.Cookie) []Cookie {
	out := make([]Cookie, 0, len(raw))
	for _, c := range raw {
		if c == nil || c.Name == "" {
			continue
		}
		out = append(out, Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path})
	}
	return out
}

func hasAny(cookies []Cookie, names []string) bool {
	if len(names) == 0 {
		return false
	}
	for _, c := range cookies {
		for _, n := range names {
			if strings.EqualFold(c.Name, n) {
				return true
			}
		}
	}
	return false
}

// browserPath returns the path to an installed Edge or Chrome, or "" to let
// chromedp find one itself.
func browserPath() string {
	var candidates []string
	for _, base := range []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("LocalAppData"),
	} {
		if base == "" {
			continue
		}
		candidates = append(candidates,
			filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
		)
	}
	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}
