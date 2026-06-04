package engine

// Optional stealth/browser HTML rendering for the scraping sources. When the
// user enables it, the full-HTML job boards fetch their search pages through the
// same stealth browser the vision search uses (Python sidecar or chromedp)
// instead of plain HTTP — so they get past bot walls and read precise JSON-LD
// results without spending vision tokens.

import (
	"context"
	"path/filepath"
	"strings"

	"autoapply/internal/browser"
	"autoapply/internal/config"
	"autoapply/internal/jobs"
)

// boundedRenderer gates concurrent RenderHTML calls onto one browser. The
// scraping sources run in parallel; the chromedp engine has a single target and
// must be driven one request at a time (slots == 1), while the Python stealth
// sidecar drives several tabs at once (slots == configured concurrency).
type boundedRenderer struct {
	r    browser.Renderer
	slot chan struct{}
}

func newBoundedRenderer(r browser.Renderer, concurrency int) *boundedRenderer {
	if concurrency < 1 {
		concurrency = 1
	}
	return &boundedRenderer{r: r, slot: make(chan struct{}, concurrency)}
}

func (s *boundedRenderer) render(ctx context.Context, url, cookie, ua string) (string, error) {
	select {
	case s.slot <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.slot }()
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
	var enabledSources []string
	for _, id := range cfg.Focus.Sources {
		if jobs.SourceUsesScrapeMode(id, cfg.Sources) {
			enabledSources = append(enabledSources, id)
		}
	}
	if len(enabledSources) == 0 {
		return nil
	}

	// The chromedp engine has one browser target and must stay serial; the Python
	// stealth sidecar loads several boards in parallel tabs.
	concurrency := 1
	if cfg.Sources.Browser.Engine() == "python" {
		concurrency = cfg.Sources.Browser.ScrapeConcurrency()
	}

	r, note, err := browser.NewRenderer(ctx, browser.SessionOptions{
		Headful:        cfg.Sources.Browser.Headful,
		ProfileDir:     e.scrapeProfileDir(),
		Engine:         cfg.Sources.Browser.Engine(),
		PythonPath:     cfg.Sources.Browser.PythonPath,
		MaxConcurrency: concurrency,
		Notify:         func(level, msg string) { e.logf(level, "scrape browser: %s", msg) },
		OnBlock:        e.blockHandler(cfg),
	})
	if err != nil {
		e.logf("warn", "%s scraping: couldn't start a browser (%v) — scrapers will use plain HTTP", cfg.Sources.Browser.Mode(), err)
		return nil
	}
	if concurrency > 1 {
		e.logf("info", "%s scraping: %s will render through the %s (up to %d in parallel)", cfg.Sources.Browser.Mode(), strings.Join(enabledSources, ", "), note, concurrency)
	} else {
		e.logf("info", "%s scraping: %s will render through the %s", cfg.Sources.Browser.Mode(), strings.Join(enabledSources, ", "), note)
	}
	br := newBoundedRenderer(r, concurrency)
	q.Render = br.render
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
