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
