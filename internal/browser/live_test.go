package browser

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveCaptureLaunch proves the chromedp plumbing works: it launches the
// installed browser (headless), loads a page, and reads back the User-Agent and
// cookies. Skipped unless AUTOAPPLY_LIVE=1, since it needs a real browser.
func TestLiveCaptureLaunch(t *testing.T) {
	if os.Getenv("AUTOAPPLY_LIVE") == "" {
		t.Skip("set AUTOAPPLY_LIVE=1 to run the live browser-capture test")
	}
	if browserPath() == "" {
		t.Skip("no Edge/Chrome found; capture flow unavailable on this machine")
	}
	// "__never__" never appears, so it polls until the (short) timeout, returning
	// whatever cookies/UA the page set — enough to prove the browser drove.
	res, err := capture(context.Background(), "https://www.bing.com/", []string{"__never__"}, 12*time.Second, true)
	t.Logf("userAgent=%q cookies=%d err=%v", res.UserAgent, len(res.Cookies), err)
	if err != nil && strings.Contains(err.Error(), "launch browser") {
		t.Fatalf("browser failed to launch: %v", err)
	}
	if res.UserAgent == "" {
		t.Fatalf("expected a captured User-Agent (browser likely did not drive)")
	}
}

// TestLiveZipConnectStaysOpen guards the ZipRecruiter "closes instantly" bug:
// because its auth-cookie list is now empty (no reliable logged-in cookie), the
// capture must wait for the user (until window-close / timeout) rather than
// returning the moment Cloudflare/visitor cookies appear. We give it a short
// timeout and assert it did NOT return early. Skipped unless AUTOAPPLY_LIVE=1.
func TestLiveZipConnectStaysOpen(t *testing.T) {
	if os.Getenv("AUTOAPPLY_LIVE") == "" {
		t.Skip("set AUTOAPPLY_LIVE=1 to run the live ZipRecruiter connect test")
	}
	if browserPath() == "" {
		t.Skip("no Edge/Chrome found")
	}
	const timeout = 6 * time.Second
	start := time.Now()
	// nil authCookies mirrors the ZipRecruiter AccountSpec (manual close).
	res, _ := capture(context.Background(), "https://www.ziprecruiter.com/login", nil, timeout, true)
	elapsed := time.Since(start)
	t.Logf("elapsed=%s cookies=%d", elapsed, len(res.Cookies))
	if elapsed < timeout-2*time.Second {
		t.Fatalf("capture returned after %s — it closed early instead of waiting for sign-in (the instant-close bug)", elapsed)
	}
}
