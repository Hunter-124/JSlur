package jobs

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"autoapply/internal/browser"
	"autoapply/internal/store"
)

// monster scrapes Monster's job search. Monster renders results with JavaScript
// and blocks plain HTTP clients, so we load the page in a real (headless)
// browser to let its scripts run, then read the schema.org JobPosting JSON-LD it
// emits. Falls back to a plain fetch (useful with a connected session) and is
// best-effort either way.
type monster struct{}

func (monster) ID() string             { return "monster" }
func (monster) Name() string           { return "Monster (scraped, JS-rendered)" }
func (monster) NeedsCredentials() bool { return false }

func (m monster) Search(ctx context.Context, q Query) ([]store.Job, error) {
	queries := searchQueries(q.Focus)
	loc := q.Focus.Location.Query()
	if len(queries) == 0 {
		if loc == "" {
			return nil, fmt.Errorf("describe your target roles in Job Focus to search Monster")
		}
		queries = []string{""}
	}
	if len(queries) > 2 {
		queries = queries[:2] // rendering is slow; cap roles for Monster
	}
	perRole := limit(q.Focus)

	// Replay a connected session's User-Agent if present.
	ua := ""
	if a, ok := q.Creds.Accounts[m.ID()]; ok {
		ua = a.UserAgent
	}

	out := map[string]store.Job{}
	var firstErr error
	for _, kw := range queries {
		u := fmt.Sprintf("https://www.monster.com/jobs/search?q=%s&where=%s",
			url.QueryEscape(kw), url.QueryEscape(loc))
		// Render with a real browser so Monster's JS populates the page.
		doc, err := browser.RenderHTML(ctx, u, ua, 4*time.Second)
		if err != nil {
			// Browser unavailable? Try a plain fetch (works with a connected session).
			doc, err = getDoc(ctx, u, accountHeaders(q, m.ID()))
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
		}
		for _, j := range ldJobsToStore(m.ID(), extractJSONLDJobs(doc), perRole) {
			if _, dup := out[j.ID]; !dup {
				out[j.ID] = j
			}
		}
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("no parseable results from Monster (no JSON-LD after rendering)")
	}
	return mapToSlice(out), nil
}
