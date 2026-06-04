package jobs

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"autoapply/internal/store"
)

// handshake scrapes Handshake (joinhandshake.com), the university career network
// — a strong source for student / early-career and internship roles, which fits
// this app's college-aged users. Handshake gates ALL job search behind a school
// sign-in and renders results with JavaScript (no public/guest endpoint and no
// schema.org JSON-LD on the search page), so it needs two things the keyless
// boards don't: a connected account (Settings → Connected accounts) and the
// rendered-page fetch path. In practice the vision scrape mode (which reads the
// listings off screenshots) is the most reliable way to read it — the same as
// LinkedIn and ZipRecruiter — so Handshake is also registered as a vision board.
type handshake struct{}

func (handshake) ID() string             { return "handshake" }
func (handshake) Name() string           { return "Handshake" }
func (handshake) NeedsCredentials() bool { return false }

func (h handshake) Search(ctx context.Context, q Query) ([]store.Job, error) {
	// Every Handshake search is behind sign-in. Without a connected session we'd
	// just fetch the sign-in wall, so skip with a clear, actionable message instead
	// of wasting a render.
	if !hasAccount(q, h.ID()) {
		return nil, fmt.Errorf("connect your Handshake account first (Settings → Connected accounts) — its job search is behind your school sign-in")
	}
	queries := searchQueries(q.Focus)
	if len(queries) == 0 {
		queries = []string{""} // browse all roles in the user's Handshake
	}
	perRole := limit(q.Focus)
	out := map[string]store.Job{}
	var firstErr error
	for _, kw := range queries {
		// q.doc renders through the stealth/browser path when one is attached
		// (stealth mode), replaying the connected session's cookies + UA.
		doc, err := q.doc(ctx, handshakeSearchURL(kw), accountHeaders(q, h.ID()))
		if err != nil {
			if firstErr == nil {
				firstErr = err // a BlockedError here means the session is stale — surfaced as-is
			}
			continue
		}
		// Handshake's search page renders with JS and ships no JSON-LD, but try it
		// anyway (individual-posting markup occasionally leaks in); the precise
		// embedded-state parser is added once a live session is available to verify.
		for _, j := range ldJobsToStore(h.ID(), extractJSONLDJobs(doc), perRole) {
			if _, dup := out[j.ID]; !dup {
				out[j.ID] = j
			}
		}
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("Handshake returned no parseable listings over HTML — it renders results with JavaScript, so use the Vision scrape mode (it reads them off the screen, like LinkedIn/ZipRecruiter)")
	}
	return mapToSlice(out), nil
}

// handshakeSearchURL builds the keyword job-search URL. Handshake scopes location
// by saved school/facet rather than free text, so we search by keyword and let the
// aggregator's radius filter narrow results to the user's area. Shared by the HTML
// scrape source and the vision board.
func handshakeSearchURL(keyword string) string {
	u := "https://app.joinhandshake.com/job-search"
	if kw := strings.TrimSpace(keyword); kw != "" {
		u += "?query=" + url.QueryEscape(kw)
	}
	return u
}

// isHandshakePosting reports whether a link looks like an individual Handshake
// posting (used by the vision board to attach real URLs to listings).
func isHandshakePosting(u string) bool {
	return strings.Contains(u, "/jobs/") || strings.Contains(u, "/job-search/")
}
