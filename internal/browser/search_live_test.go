package browser

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveSessionShots isolates the vision-search browser path: open a Session,
// load a page, and capture screenshots + links. Skipped unless AUTOAPPLY_LIVE=1.
func TestLiveSessionShots(t *testing.T) {
	if os.Getenv("AUTOAPPLY_LIVE") == "" {
		t.Skip("set AUTOAPPLY_LIVE=1 to run the live session/shots test")
	}
	if browserPath() == "" {
		t.Skip("no Edge/Chrome found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sess, err := NewSession(ctx, SessionOptions{Headful: false})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()
	t.Log("session opened")

	shots, err := sess.Shots(ctx, "https://www.bing.com/search?q=Victaulic+careers", "", "", 2)
	if err != nil {
		t.Fatalf("Shots: %v", err)
	}
	t.Logf("images=%d links=%d title=%q", len(shots.Images), len(shots.Links), shots.Title)
	if len(shots.Images) == 0 {
		t.Error("expected at least one screenshot")
	}
	// Show a few links to confirm collection works.
	for i, l := range shots.Links {
		if i >= 5 {
			break
		}
		t.Logf("  link: %q -> %s", l.Text, l.URL)
	}
}
