package browser

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestLivePyStealthInteractiveBlock exercises the full interactive block seam of
// the Python stealth sidecar: a page that classifies as a Cloudflare wall should
// raise an OnBlock prompt, and answering it (continue) should let RenderHTML
// finish. Skipped unless AUTOAPPLY_LIVE=1 (needs python + playwright + a headful
// browser). It proves: sidecar attention event -> Go event routing -> OnBlock ->
// writeControl -> sidecar resume -> final response.
func TestLivePyStealthInteractiveBlock(t *testing.T) {
	if os.Getenv("AUTOAPPLY_LIVE") == "" {
		t.Skip("set AUTOAPPLY_LIVE=1 to run the live interactive-block test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var blockedCalls int32
	var gotKind, gotHost atomic.Value
	gotKind.Store("")
	gotHost.Store("")

	sh, err := newPyShooter(ctx, SessionOptions{
		Headful:    true,
		ProfileDir: t.TempDir(),
		OnBlock: func(_ context.Context, host, kind, reason string) bool {
			atomic.AddInt32(&blockedCalls, 1)
			gotKind.Store(kind)
			gotHost.Store(host)
			t.Logf("OnBlock: host=%q kind=%q reason=%q -> continue", host, kind, reason)
			return true // answer "continue"
		},
	})
	if err != nil {
		t.Skipf("python stealth sidecar unavailable: %v", err)
	}
	defer sh.Close()

	// A page whose title trips the Cloudflare classifier and whose body is too
	// short to count as "ready" — so it's treated as a block. It's a static page
	// that can't actually be "solved", so after we answer "continue" it is still
	// blocked and RenderHTML returns a BlockedError — that's the expected outcome
	// here; the point of the test is that the OnBlock seam fired.
	blockURL := "data:text/html,<title>Just a moment...</title><body>blocked</body>"
	html, err := sh.RenderHTML(ctx, blockURL, "", "")
	if atomic.LoadInt32(&blockedCalls) != 1 {
		t.Fatalf("expected OnBlock to be called once, got %d", blockedCalls)
	}
	if k := gotKind.Load().(string); k != "cloudflare" {
		t.Errorf("block kind = %q, want cloudflare", k)
	}
	if !IsBlocked(err) {
		t.Errorf("expected a BlockedError after the (unsolvable static) block page, got err=%v html=%q", err, truncate(html, 120))
	}
	t.Log("interactive block seam OK (OnBlock fired; page remained blocked as expected)")
}
