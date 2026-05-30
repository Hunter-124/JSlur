package jobs

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"autoapply/internal/store"
)

// linkedin scrapes LinkedIn's public "jobs-guest" endpoint — the same one that
// powers the logged-out job widget. It needs no key or login and returns HTML
// job cards. It is the most reliable of the scraped boards.
type linkedin struct{}

func (linkedin) ID() string             { return "linkedin" }
func (linkedin) Name() string           { return "LinkedIn (scraped)" }
func (linkedin) NeedsCredentials() bool { return false }

const liEndpoint = "https://www.linkedin.com/jobs-guest/jobs/api/seeMoreJobPostings/search"

var (
	liCardRe  = regexp.MustCompile(`(?is)<li[^>]*>(.*?)</li>`)
	liHrefRe  = regexp.MustCompile(`(?i)href=["']([^"']*/jobs/view/[^"']+)["']`)
	liTitleRe = regexp.MustCompile(`(?is)<h3[^>]*base-search-card__title[^>]*>(.*?)</h3>`)
	liCompRe  = regexp.MustCompile(`(?is)<h4[^>]*base-search-card__subtitle[^>]*>(.*?)</h4>`)
	liLocRe   = regexp.MustCompile(`(?is)<span[^>]*job-search-card__location[^>]*>(.*?)</span>`)
	liDateRe  = regexp.MustCompile(`(?is)<time[^>]*datetime=["']([^"']+)["']`)
	liInnerA  = regexp.MustCompile(`(?is)<a[^>]*>(.*?)</a>`)
)

func (li linkedin) Search(ctx context.Context, q Query) ([]store.Job, error) {
	queries := searchQueries(q.Focus)
	loc := q.Focus.Location.Query()
	if len(queries) == 0 && loc == "" && !q.Focus.IncludeRemote {
		return nil, fmt.Errorf("describe your target roles in Job Focus to search LinkedIn")
	}
	if len(queries) == 0 {
		queries = []string{""} // location-only search
	}
	perRole := limit(q.Focus)
	headers := mergeHeaders(map[string]string{"X-Requested-With": "XMLHttpRequest"}, accountHeaders(q, li.ID()))

	// Variant A: jobs near the user's location (on-site + remote).
	// Variant B (if remote allowed): US-wide remote-only (f_WT=2).
	type variant struct{ location, extra string }
	variants := []variant{{location: orDefault(loc, "United States")}}
	if q.Focus.IncludeRemote {
		variants = append(variants, variant{location: "United States", extra: "&f_WT=2"})
	}

	out := map[string]store.Job{}
	var firstErr error
	for _, kw := range queries {
		target := len(out) + perRole // give each role its own budget
		for _, v := range variants {
			for start := 0; start < 75 && len(out) < target; start += 25 {
				u := fmt.Sprintf("%s?keywords=%s&location=%s&start=%d%s",
					liEndpoint, url.QueryEscape(kw), url.QueryEscape(v.location), start, v.extra)
				doc, err := getDoc(ctx, u, headers)
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					break
				}
				if li.parsePage(doc, out, target) == 0 {
					break // no more results for this variant
				}
			}
		}
	}

	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return mapToSlice(out), nil
}

// parsePage extracts job cards from one guest-API response into out, returning
// how many cards it saw (0 means the page was empty / end of results).
func (li linkedin) parsePage(doc string, out map[string]store.Job, max int) int {
	cards := liCardRe.FindAllStringSubmatch(doc, -1)
	for _, c := range cards {
		card := c[1]
		href := firstGroup(liHrefRe, card)
		if href == "" {
			continue
		}
		link := cleanURL(href)
		id := store.MakeJobID(li.ID(), link)
		if _, dup := out[id]; dup {
			continue
		}
		title := htmlText(firstGroup(liTitleRe, card))
		company := htmlText(firstGroup(liInnerA, firstGroup(liCompRe, card)))
		if company == "" {
			company = htmlText(firstGroup(liCompRe, card))
		}
		loc := htmlText(firstGroup(liLocRe, card))
		if title == "" {
			continue
		}
		out[id] = store.Job{
			ID:       id,
			Source:   li.ID(),
			Title:    title,
			Company:  company,
			Location: loc,
			Remote:   looksRemote(title, loc),
			URL:      link,
			PostedAt: parseTime(firstGroup(liDateRe, card)),
		}
		if len(out) >= max {
			break
		}
	}
	return len(cards)
}

// ---- small parsing helpers shared by the scraped HTML sources ----

func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return m[1]
	}
	return ""
}

// htmlText strips tags/entities and collapses whitespace in a small HTML fragment.
func htmlText(s string) string {
	return strings.TrimSpace(wsRe.ReplaceAllString(stripHTML(s), " "))
}

// cleanURL drops the query string (usually tracking params) from a posting URL.
func cleanURL(u string) string {
	u = strings.TrimSpace(u)
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	return u
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
