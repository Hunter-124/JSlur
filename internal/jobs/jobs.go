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
	// OnBlock, when set, is handed to the browser session so a sign-in/captcha wall
	// hit during a (headful) search pauses for the user to solve it. The engine
	// wires it to an interactive GUI prompt; nil disables interactive waiting.
	OnBlock func(ctx context.Context, host, kind, reason string) bool
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
	"handshake":     &handshake{},
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
	"monster", "craigslist", "handshake", "remotive", "adzuna", "usajobs",
}

// OfficialAPISources are the only boards backed by a first-party public API.
// Every other user-selectable board is treated as scraped in the GUI.
var OfficialAPISources = map[string]bool{
	"adzuna":  true,
	"usajobs": true,
}

// scrapeModeSources are the scraped boards that currently honor the global
// scrape mode by fetching pages through Query.doc (which uses q.Render when the
// engine attached one for blatant/stealth mode).
var scrapeModeSources = map[string]bool{
	"simplyhired": true,
	"monster":     true,
	"handshake":   true,
}

// IsOfficialAPISource reports whether sourceID is an official API integration.
func IsOfficialAPISource(sourceID string) bool { return OfficialAPISources[sourceID] }

// IsScrapedSource reports whether sourceID is treated as scraped in the GUI and
// source metadata. Only Adzuna and USAJobs are official APIs.
func IsScrapedSource(sourceID string) bool { return !IsOfficialAPISource(sourceID) }

// SourceUsesScrapeMode reports whether sourceID currently honors the global
// scrape mode settings. ZipRecruiter participates only when a connected account
// is present, because its fallback mobile API path does not use browser render.
func SourceUsesScrapeMode(sourceID string, creds config.SourcesConfig) bool {
	if scrapeModeSources[sourceID] {
		return true
	}
	if sourceID != "ziprecruiter" {
		return false
	}
	a, ok := creds.Accounts[sourceID]
	return ok && strings.TrimSpace(a.Cookie) != ""
}

// SourceInfo describes a source for the GUI.
type SourceInfo struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	NeedsCredentials bool   `json:"needsCredentials"`
	// OfficialAPI is true only for first-party API integrations (Adzuna/USAJobs).
	OfficialAPI bool `json:"officialApi"`
	// Scrape reports whether the source is treated as scraped in the GUI. This is
	// true for every non-official source, even if its fetch path does not consume
	// the global scrape mode.
	Scrape bool `json:"scrape"`
}

// AvailableSources lists every registered source in display order.
func AvailableSources() []SourceInfo {
	out := make([]SourceInfo, 0, len(Registry))
	for _, id := range sourceOrder {
		if s, ok := Registry[id]; ok {
			official := IsOfficialAPISource(id)
			out = append(out, SourceInfo{
				ID:               id,
				Name:             s.Name(),
				NeedsCredentials: s.NeedsCredentials(),
				OfficialAPI:      official,
				Scrape:           !official,
			})
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
