// Package jobs searches US job boards. The user describes the roles they want
// in free text; for each enabled board the engine asks the active AI model to
// map that description onto the board's own category taxonomy (fetched from its
// API), and the board is then queried by those categories plus the user's
// location and radius. The Aggregator merges, de-duplicates and US-filters the
// results.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"autoapply/internal/config"
	"autoapply/internal/store"
)

// ErrNotConfigured is returned by a source that needs credentials it doesn't
// have. The engine reports it as an informational note, not a failure.
var ErrNotConfigured = errors.New("not configured — add credentials in Settings")

// Category is one entry in a board's taxonomy. ID is the value used in the
// board's query (a tag, code or name); Label is the human text the AI reasons
// over when matching the user's interest.
type Category struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// CategorySelector chooses the subset of a board's categories most relevant to
// the user's free-text interest. The engine provides an AI-backed implementation
// with a heuristic fallback. sourceID is used for logging only.
type CategorySelector func(ctx context.Context, sourceID string, available []Category) ([]Category, error)

// VisionFunc reads job listings off rendered-page screenshots using a
// vision-capable AI model: prompt guides the extraction, images are the PNG
// screenshots, and the return is the model's raw text reply (expected to be
// JSON). The engine injects it from the active provider; it is nil when no AI is
// configured, and the vision source then reports a friendly note.
type VisionFunc func(ctx context.Context, prompt string, images [][]byte) (string, error)

// Query is everything a source needs to run one search.
type Query struct {
	Focus  config.JobFocus
	Creds  config.SourcesConfig
	Select CategorySelector
	Vision VisionFunc
	// Log, when set, receives human-readable progress notes from slow multi-step
	// sources (currently AI Browser Search) so the user can watch what the browser
	// and vision model are doing. level is "info" or "warn". Nil is fine — sources
	// must tolerate it.
	Log func(level, msg string)
	// BrowserProfileDir, when set, is a persistent Chrome user-data dir the vision
	// browser reuses across searches so a solved Cloudflare challenge survives.
	// Empty uses a throwaway profile.
	BrowserProfileDir string
	// Render, when set, fetches a URL's fully-rendered HTML through a real/stealth
	// browser (the engine wires it to the Python sidecar or chromedp). The HTML
	// scraping sources prefer it over plain HTTP — it runs the page's JS and
	// carries the humanized fingerprint that gets past bot walls. The cookie +
	// User-Agent in the request headers are replayed. nil = plain HTTP only.
	Render func(ctx context.Context, url, cookie, userAgent string) (string, error)
}

// logf reports a progress note via the query's Log hook, if one is set.
func (q Query) logf(level, format string, args ...any) {
	if q.Log != nil {
		q.Log(level, fmt.Sprintf(format, args...))
	}
}

// doc fetches a page for the scraping sources: through the stealth/browser
// renderer when one is configured (q.Render), otherwise plain HTTP. The board's
// browser UA + any connected-session Cookie ride along either way.
func (q Query) doc(ctx context.Context, url string, headers map[string]string) (string, error) {
	if q.Render != nil {
		return q.Render(ctx, url, headers["Cookie"], headers["User-Agent"])
	}
	return getDoc(ctx, url, headers)
}

// Source is a single job board.
type Source interface {
	ID() string
	Name() string
	// NeedsCredentials reports whether the source requires API keys (for GUI hints).
	NeedsCredentials() bool
	Search(ctx context.Context, q Query) ([]store.Job, error)
}

// Registry lists all known sources keyed by id.
var Registry = map[string]Source{
	"themuse":       &themuse{},
	"linkedin":      &linkedin{},
	"indeed":        &indeed{},
	"ziprecruiter":  &ziprecruiter{},
	"simplyhired":   &simplyhired{},
	"monster":       &monster{},
	"craigslist":    &craigslist{},
	"visionbrowser": &visionBrowser{},
	"remotive":      &remotive{},
	"adzuna":        &adzuna{},
	"usajobs":       &usajobs{},
}

// sourceOrder fixes the GUI display order of sources. Keyless scraped boards
// come first, then the keyed APIs. "visionbrowser" is intentionally absent: it
// is no longer a per-board source the user ticks, but the global "vision" scrape
// mode (config.ScrapeVision), wired in by the engine.
var sourceOrder = []string{
	"themuse", "linkedin", "indeed", "ziprecruiter", "simplyhired",
	"monster", "craigslist", "remotive", "adzuna", "usajobs",
}

// BrowserScrapeSources are the boards fetched by rendering their HTML search
// page through a real browser, so they honour the global scrape mode (blatant /
// stealth). Every other board hits an API/feed and ignores the mode. LinkedIn
// (guest XHR fragment) and Craigslist (RSS) keep their specialized fetches and
// are excluded — browser-rendering those would change/break the response.
var BrowserScrapeSources = map[string]bool{"ziprecruiter": true, "simplyhired": true, "monster": true}

// SourceInfo describes a source for the GUI.
type SourceInfo struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	NeedsCredentials bool   `json:"needsCredentials"`
	// Scrape reports whether the source is fetched by browser-rendering its page
	// (so it obeys the global scrape mode). False means it uses an API/feed.
	Scrape bool `json:"scrape"`
}

// AvailableSources lists every registered source in display order.
func AvailableSources() []SourceInfo {
	out := make([]SourceInfo, 0, len(Registry))
	for _, id := range sourceOrder {
		if s, ok := Registry[id]; ok {
			out = append(out, SourceInfo{ID: id, Name: s.Name(), NeedsCredentials: s.NeedsCredentials(), Scrape: BrowserScrapeSources[id]})
		}
	}
	return out
}

// SearchResult is the per-source outcome of a search.
type SearchResult struct {
	Source string
	Jobs   []store.Job
	Err    error
}

// Aggregator fans a search out across the focus's enabled sources.
type Aggregator struct{}

// Search runs all enabled sources concurrently and returns filtered, de-duped
// jobs plus the per-source outcomes.
func (Aggregator) Search(ctx context.Context, q Query) ([]store.Job, []SearchResult) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make([]SearchResult, 0, len(q.Focus.Sources))

	for _, id := range q.Focus.Sources {
		src, ok := Registry[id]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(src Source) {
			defer wg.Done()
			j, err := src.Search(ctx, q)
			mu.Lock()
			results = append(results, SearchResult{Source: src.ID(), Jobs: j, Err: err})
			mu.Unlock()
		}(src)
	}
	wg.Wait()

	seenURL := map[string]bool{}
	seenJob := map[string]bool{} // normalized company|title, to drop cross-board dups
	var jobs []store.Job
	for _, r := range results {
		for _, j := range r.Jobs {
			key := normKey(j.Company, j.Title)
			if seenURL[j.ID] || seenJob[key] || !accept(j, q.Focus) {
				continue
			}
			seenURL[j.ID] = true
			seenJob[key] = true
			jobs = append(jobs, j)
		}
	}
	return jobs, results
}

func normKey(company, title string) string {
	return strings.ToLower(strings.TrimSpace(company)) + "|" + strings.ToLower(strings.TrimSpace(title))
}

// accept applies the post-fetch filters: exclude keywords, location (remote when
// allowed, otherwise within the search radius), and minimum salary.
func accept(j store.Job, focus config.JobFocus) bool {
	hay := strings.ToLower(j.Title + " " + j.Description + " " + strings.Join(j.Tags, " "))
	for _, ex := range focus.ExcludeKeywords {
		if ex = strings.ToLower(strings.TrimSpace(ex)); ex != "" && strings.Contains(hay, ex) {
			return false
		}
	}

	if j.Remote {
		// Remote roles have no location to measure against, so they're kept only
		// when the user opted into remote.
		if !focus.IncludeRemote {
			return false
		}
	} else if !withinSearchArea(j.Location, focus) {
		// Non-remote jobs must fall inside the configured radius (this is the
		// backstop for sources that ignore location server-side).
		return false
	}

	if focus.MinSalary > 0 && j.SalaryMin > 0 && j.SalaryMin < focus.MinSalary {
		return false
	}
	return true
}

// mapToSlice flattens a de-dup map into a slice.
func mapToSlice(m map[string]store.Job) []store.Job {
	out := make([]store.Job, 0, len(m))
	for _, j := range m {
		out = append(out, j)
	}
	return out
}

// limit returns the configured cap or a sane default.
func limit(focus config.JobFocus) int {
	if focus.MaxResultsPerSource > 0 {
		return focus.MaxResultsPerSource
	}
	return 25
}

// chooseCategories runs the query's selector over a taxonomy, tolerating a nil
// selector (returns everything, capped).
func chooseCategories(ctx context.Context, q Query, sourceID string, available []Category) []Category {
	if len(available) == 0 {
		return nil
	}
	if q.Select == nil {
		if len(available) > 4 {
			return available[:4]
		}
		return available
	}
	chosen, err := q.Select(ctx, sourceID, available)
	if err != nil || len(chosen) == 0 {
		return nil
	}
	return chosen
}
