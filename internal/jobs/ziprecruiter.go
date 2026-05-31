package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"autoapply/internal/store"
)

// ziprecruiter queries ZipRecruiter's mobile API — the endpoint the iOS app
// uses. The headers below (including the public Basic token) are the app's own
// and need no per-user key. The site front-end is Cloudflare-protected, so this
// mobile API is the reliable keyless path; like the app, we first hit an event
// endpoint to obtain a session cookie.
type ziprecruiter struct{}

func (ziprecruiter) ID() string             { return "ziprecruiter" }
func (ziprecruiter) Name() string           { return "ZipRecruiter" }
func (ziprecruiter) NeedsCredentials() bool { return false }

var zrHeaders = map[string]string{
	"Host":                 "api.ziprecruiter.com",
	"accept":               "*/*",
	"x-zr-zva-override":    "100000000;vid:ZT1huzm_EQlDTVEc",
	"x-pushnotificationid": "0ff4983d38d7fc5b3370297f2bcffcf4b3321c418f5c22dd152a0264707602a0",
	"x-deviceid":           "D77B3A92-E589-46A4-8A39-6EF6F1D86006",
	"user-agent":           "Job Search/87.0 (iPhone; CPU iOS 16_6_1 like Mac OS X)",
	"authorization":        "Basic YTBlZjMyZDYtN2I0Yy00MWVkLWEyODMtYTI1NDAzMzI0YTcyOg==",
	"accept-language":      "en-US,en;q=0.9",
}

func (s ziprecruiter) Search(ctx context.Context, q Query) ([]store.Job, error) {
	// When the user has connected a ZipRecruiter session, scrape the website
	// (its JSON-LD) using the captured cf_clearance cookie + UA — the mobile API
	// below ignores web cookies and is the one that tends to get 403'd.
	if hasAccount(q, s.ID()) {
		return s.searchWebsite(ctx, q)
	}

	queries := searchQueries(q.Focus)
	loc := q.Focus.Location.Query()
	if len(queries) == 0 {
		if loc == "" && !q.Focus.IncludeRemote {
			return nil, fmt.Errorf("describe your target roles in Job Focus to search ZipRecruiter")
		}
		queries = []string{"remote"}
	}
	perRole := limit(q.Focus)
	radius := q.Focus.Location.RadiusMiles
	if radius <= 0 {
		radius = 50
	}

	// A cookie jar lets the session cookie from the event endpoint carry over.
	jar, _ := cookiejar.New(nil)
	cli := &http.Client{Timeout: 30 * time.Second, Jar: jar}
	// Best-effort handshake to obtain a session cookie (ignore failures).
	_ = zrEvent(ctx, cli)

	out := map[string]store.Job{}
	var firstErr error
	for _, kw := range queries {
		target := len(out) + perRole
		continueFrom := ""
		for page := 0; page < 5 && len(out) < target; page++ {
			params := url.Values{}
			params.Set("search", kw)
			if loc != "" {
				params.Set("location", loc)
			}
			params.Set("radius", fmt.Sprintf("%d", radius))
			if continueFrom != "" {
				params.Set("continue_from", continueFrom)
			}
			endpoint := "https://api.ziprecruiter.com/jobs-app/jobs?" + params.Encode()

			raw, err := zrGet(ctx, cli, endpoint)
			if err != nil {
				firstErr = err
				break
			}
			var resp struct {
				Jobs     []map[string]any `json:"jobs"`
				Continue string           `json:"continue"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				firstErr = err
				break
			}
			if len(resp.Jobs) == 0 {
				break
			}
			for _, j := range resp.Jobs {
				job := s.parseJob(j)
				if job.URL == "" || job.Title == "" {
					continue
				}
				if _, dup := out[job.ID]; dup {
					continue
				}
				out[job.ID] = job
				if len(out) >= target {
					break
				}
			}
			if resp.Continue == "" {
				break
			}
			continueFrom = resp.Continue
		}
	}

	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return mapToSlice(out), nil
}

// parseJob reads one ZipRecruiter job object defensively — field names have
// drifted over time, so each value tries a few likely keys.
func (s ziprecruiter) parseJob(j map[string]any) store.Job {
	title := mStr(j, "name", "job_title", "title")
	company := nestedStr(j, "hiring_company", "name")
	if company == "" {
		company = mStr(j, "company", "org_name")
	}
	loc := strings.TrimSpace(strings.Trim(mStr(j, "job_city")+", "+mStr(j, "job_state"), ", "))
	if loc == "" {
		loc = mStr(j, "location")
	}
	link := mStr(j, "job_url", "url")
	if link == "" {
		if key := mStr(j, "listing_key", "id"); key != "" {
			link = "https://www.ziprecruiter.com/jobs//j?lvk=" + key
		}
	}
	desc := mStr(j, "job_description", "description")
	id := store.MakeJobID(s.ID(), link)
	return store.Job{
		ID:          id,
		Source:      s.ID(),
		Title:       title,
		Company:     company,
		Location:    loc,
		Remote:      looksRemote(title, loc, desc),
		URL:         link,
		Description: stripHTML(desc),
		Salary:      formatUSD(mInt(j, "salary_min_annual", "salary_min"), mInt(j, "salary_max_annual", "salary_max")),
		SalaryMin:   mInt(j, "salary_min_annual", "salary_min"),
		PostedAt:    parseTime(mStr(j, "posted_time", "posted_time_friendly")),
	}
}

// searchWebsite scrapes the ZipRecruiter website's embedded JSON-LD using the
// connected browser session, whose cf_clearance cookie + matching User-Agent get
// past Cloudflare.
func (s ziprecruiter) searchWebsite(ctx context.Context, q Query) ([]store.Job, error) {
	queries := searchQueries(q.Focus)
	if len(queries) == 0 {
		queries = []string{""}
	}
	loc := q.Focus.Location.Query()
	perRole := limit(q.Focus)
	out := map[string]store.Job{}
	var firstErr error
	for _, kw := range queries {
		u := fmt.Sprintf("https://www.ziprecruiter.com/jobs-search?search=%s&location=%s",
			url.QueryEscape(kw), url.QueryEscape(loc))
		doc, err := q.doc(ctx, u, accountHeaders(q, s.ID()))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, j := range ldJobsToStore(s.ID(), extractJSONLDJobs(doc), perRole) {
			if _, dup := out[j.ID]; !dup {
				out[j.ID] = j
			}
		}
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("no parseable results from ZipRecruiter website (session may be stale — reconnect the account)")
	}
	return mapToSlice(out), nil
}

func zrEvent(ctx context.Context, cli *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.ziprecruiter.com/jobs-app/event", strings.NewReader(""))
	if err != nil {
		return err
	}
	for k, v := range zrHeaders {
		req.Header.Set(k, v)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	return nil
}

func zrGet(ctx context.Context, cli *http.Client, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range zrHeaders {
		req.Header.Set(k, v)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http %d from api.ziprecruiter.com", resp.StatusCode)
	}
	return data, nil
}

// ---- defensive map readers reused by the JSON scraping sources ----

func mStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := ldStr(m[k]); s != "" {
			return s
		}
	}
	return ""
}

func nestedStr(m map[string]any, a, b string) string {
	if sub, ok := m[a].(map[string]any); ok {
		return ldStr(sub[b])
	}
	return ""
}

func mInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if n := ldNum(m[k]); n != 0 {
			return n
		}
	}
	return 0
}
