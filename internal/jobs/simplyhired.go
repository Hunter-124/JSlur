package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"autoapply/internal/store"
)

// simplyhired scrapes SimplyHired's search results. SimplyHired is a Next.js app
// that no longer emits schema.org JSON-LD; its rendered search page instead
// carries the listings as a JSON "jobs" array embedded in the page state. We read
// that array (and fall back to JSON-LD for any page that still has it). It
// aggregates from many boards and is broad (not tech-specific). Needs the page
// fully rendered, so it runs through the stealth/browser fetch path.
type simplyhired struct{}

func (simplyhired) ID() string             { return "simplyhired" }
func (simplyhired) Name() string           { return "SimplyHired" }
func (simplyhired) NeedsCredentials() bool { return false }

func (s simplyhired) Search(ctx context.Context, q Query) ([]store.Job, error) {
	queries := searchQueries(q.Focus)
	loc := q.Focus.Location.Query()
	if len(queries) == 0 {
		if loc == "" {
			return nil, fmt.Errorf("describe your target roles in Job Focus to search SimplyHired")
		}
		queries = []string{""}
	}
	perRole := limit(q.Focus)
	out := map[string]store.Job{}
	var firstErr error
	for _, kw := range queries {
		u := fmt.Sprintf("https://www.simplyhired.com/search?q=%s&l=%s",
			url.QueryEscape(kw), url.QueryEscape(loc))
		doc, err := q.doc(ctx, u, accountHeaders(q, s.ID()))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// Primary: the embedded "jobs" array. Fallback: schema.org JSON-LD, for any
		// page that still carries it.
		found := shJobsToStore(s.ID(), extractSimplyHiredJobs(doc), perRole)
		if len(found) == 0 {
			found = ldJobsToStore(s.ID(), extractJSONLDJobs(doc), perRole)
		}
		for _, j := range found {
			if _, dup := out[j.ID]; !dup {
				out[j.ID] = j
			}
		}
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("no parseable results (SimplyHired page may require JavaScript — enable stealth or vision scraping)")
	}
	return mapToSlice(out), nil
}

// shJob is one listing from SimplyHired's embedded page-state "jobs" array.
type shJob struct {
	JobKey     string `json:"jobKey"`
	Title      string `json:"title"`
	Company    string `json:"company"`
	Location   string `json:"location"`
	Snippet    string `json:"snippet"`
	SalaryInfo string `json:"salaryInfo"`
	BotURL     string `json:"botUrl"` // clean relative posting path, e.g. "/job/<key>"
}

// extractSimplyHiredJobs pulls the listings out of the embedded page state. The
// rendered search page contains a JSON object with a "jobs":[…] array (inside the
// Next.js __NEXT_DATA__ script); we locate that array and decode it directly,
// which is far more robust than scraping the React-generated DOM.
func extractSimplyHiredJobs(doc string) []shJob {
	const marker = `"jobs":`
	for off := 0; ; {
		i := strings.Index(doc[off:], marker)
		if i < 0 {
			return nil
		}
		i += off
		off = i + len(marker)
		arr := jsonArrayAt(strings.TrimSpace(doc[i+len(marker):]))
		if arr == "" {
			continue // "jobs" whose value isn't an array (e.g. null) — keep looking
		}
		var jobs []shJob
		if err := json.Unmarshal([]byte(arr), &jobs); err != nil {
			continue
		}
		var real []shJob
		for _, j := range jobs {
			if strings.TrimSpace(j.Title) != "" && (j.JobKey != "" || j.BotURL != "") {
				real = append(real, j)
			}
		}
		if len(real) > 0 {
			return real
		}
	}
}

// shJobsToStore converts SimplyHired listings into store.Job records, de-duping
// by URL/key and capping at max.
func shJobsToStore(srcID string, shs []shJob, max int) []store.Job {
	out := map[string]store.Job{}
	for _, j := range shs {
		title := strings.TrimSpace(j.Title)
		if title == "" {
			continue
		}
		link := strings.TrimSpace(j.BotURL)
		if strings.HasPrefix(link, "/") {
			link = "https://www.simplyhired.com" + link
		}
		seed := strings.TrimSpace(j.JobKey)
		if seed == "" {
			seed = link
		}
		id := store.MakeJobID(srcID, seed)
		if _, dup := out[id]; dup {
			continue
		}
		loc := strings.TrimSpace(j.Location)
		out[id] = store.Job{
			ID:          id,
			Source:      srcID,
			Title:       title,
			Company:     strings.TrimSpace(j.Company),
			Location:    loc,
			Remote:      looksRemote(title, loc, j.Snippet),
			URL:         link,
			Description: strings.TrimSpace(j.Snippet),
			Salary:      strings.TrimSpace(j.SalaryInfo),
		}
		if len(out) >= max {
			break
		}
	}
	return mapToSlice(out)
}

// jsonArrayAt returns the JSON array literal at the start of s (s[0] must be '['),
// i.e. the substring from '[' through its matching ']', honoring quoted strings
// and escapes so brackets inside string values don't throw off the depth count.
// Returns "" when s doesn't start with '[' or has no balanced match.
func jsonArrayAt(s string) string {
	if len(s) == 0 || s[0] != '[' {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return ""
}
