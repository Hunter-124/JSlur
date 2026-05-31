package engine

// Optional stealth/browser HTML rendering for the scraping sources. When the
// user enables it, the full-HTML job boards fetch their search pages through the
// same stealth browser the vision search uses (Python sidecar or chromedp)
// instead of plain HTTP — so they get past bot walls and read precise JSON-LD
// results without spending vision tokens.

import (
	"context"
	"path/filepath"
	"sync"

	"autoapply/internal/browser"
	"autoapply/internal/config"
	"autoapply/internal/jobs"
)

// serialRenderer serializes concurrent RenderHTML calls onto one browser. The
// scraping sources run in parallel, but a single browser session (especially
// chromedp's lone target) must be driven one request at a time.
type serialRenderer struct {
	mu sync.Mutex
	r  browser.Renderer
}

func (s *serialRenderer) render(ctx context.Context, url, cookie, ua string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.RenderHTML(ctx, url, cookie, ua)
}

// attachRenderer wires q.Render to a shared HTML renderer when the user enabled
// stealth scraping and at least one capable source is on. It returns a close
// func to run when the search finishes, or nil when no renderer was started (the
// scrapers then fall back to plain HTTP inside Query.doc).
func (e *Engine) attachRenderer(ctx context.Context, cfg config.Config, q *jobs.Query) func() {
	if !cfg.Sources.Browser.RendersScrapes() {
		return nil
	}
	enabled := false
	for _, id := range cfg.Focus.Sources {
		if jobs.BrowserScrapeSources[id] {
			enabled = true
			break
		}
	}
	if !enabled {
		return nil
	}

	r, note, err := browser.NewRenderer(ctx, browser.SessionOptions{
		Headful:    cfg.Sources.Browser.Headful,
		ProfileDir: e.scrapeProfileDir(),
		Engine:     cfg.Sources.Browser.Engine(),
		PythonPath: cfg.Sources.Browser.PythonPath,
	})
	if err != nil {
		e.logf("warn", "%s scraping: couldn't start a browser (%v) — scrapers will use plain HTTP", cfg.Sources.Browser.Mode(), err)
		return nil
	}
	e.logf("info", "%s scraping: ZipRecruiter/SimplyHired/Monster will render through the %s", cfg.Sources.Browser.Mode(), note)
	sr := &serialRenderer{r: r}
	q.Render = sr.render
	return r.Close
}

// scrapeProfileDir is the persistent Chrome profile for the stealth scraping
// renderer, kept separate from the vision profile so both can run at once
// (chromedp can't share one user-data dir between two live browsers).
func (e *Engine) scrapeProfileDir() string {
	if e.dataDir == "" {
		return ""
	}
	return filepath.Join(e.dataDir, "scrape-profile")
}
