package engine

// This file implements the browser + vision fallback for resolving a job's
// official application URL. When the normal (link-mining + site-crawl) resolver
// in jobs.ResolveOfficial can't find a real apply page, the engine drives a
// browser to a web search for the company's careers page, screenshots the
// results, and asks the vision model to pick the company's own careers/ATS URL
// from the links it scraped. It then re-resolves from that URL to drill down to
// the actual application page. This is slow (browser + a vision call), so it
// runs only on the manual/on-demand ResolveApply path.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"autoapply/internal/ai"
	"autoapply/internal/browser"
	"autoapply/internal/jobs"
	"autoapply/internal/store"
)

// needsVisionFallback reports whether normal resolution came up short enough to
// justify the slow browser+vision search: no apply URL at all, or only the bare
// company website (no specific careers/apply page and no detected ATS).
func needsVisionFallback(o jobs.Official) bool {
	if strings.TrimSpace(o.ApplyURL) == "" {
		return true
	}
	return o.ATS == "" && strings.HasPrefix(o.Note, "company website")
}

// visionFindApplyURL searches the web for the company's careers page with a real
// browser, has the vision model pick the official URL off the results, then
// re-resolves from it to find the concrete application/ATS link. It returns the
// resolved Official and true on success, or false when it can't improve on what
// the caller already had.
func (e *Engine) visionFindApplyURL(ctx context.Context, job store.Job, provider ai.Provider) (jobs.Official, bool) {
	company := strings.TrimSpace(job.Company)
	if company == "" || provider == nil {
		return jobs.Official{}, false
	}

	// Bound the whole detour: browser launch + a search page + one vision call.
	bctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	bcfg := e.cfg.Get().Sources.Browser
	sess, _, err := browser.NewShooter(bctx, browser.SessionOptions{
		Headful:    bcfg.Headful,
		ProfileDir: e.visionProfileDir(),
		Engine:     bcfg.Engine(),
		PythonPath: bcfg.PythonPath,
	})
	if err != nil {
		e.logf("warn", "vision URL search: couldn't start a browser (%v)", err)
		return jobs.Official{}, false
	}
	defer sess.Close()

	query := company + " careers"
	e.logf("info", "vision URL search: looking up %q", query)
	searchURL := "https://www.bing.com/search?q=" + url.QueryEscape(query)
	shots, err := sess.Shots(bctx, searchURL, "", "", 2)
	if err != nil {
		e.logf("warn", "vision URL search: results page didn't load (%v)", err)
		return jobs.Official{}, false
	}
	if len(shots.Images) == 0 {
		e.logf("warn", "vision URL search: captured no screenshots of the results")
		return jobs.Official{}, false
	}

	// Candidate links = real result targets that aren't job boards or the search
	// engine itself. Search engines wrap result links in click-tracking
	// redirects, so unwrap them first, then drop aggregators.
	var cands []browser.Link
	seen := map[string]bool{}
	for _, l := range shots.Links {
		u := unwrapResultURL(l.URL)
		if u == "" || seen[u] || jobs.IsAggregatorURL(u) {
			continue
		}
		seen[u] = true
		cands = append(cands, browser.Link{Text: l.Text, URL: u})
		if len(cands) >= 30 {
			break
		}
	}
	if len(cands) == 0 {
		e.logf("warn", "vision URL search: no usable company links on the results page")
		return jobs.Official{}, false
	}

	imgs := make([]ai.Image, 0, len(shots.Images))
	for _, b := range shots.Images {
		imgs = append(imgs, ai.Image{Mime: "image/png", Data: b})
	}
	raw, err := provider.Vision(bctx, ai.Request{Prompt: buildCompanyURLPrompt(job, cands), MaxTokens: 1024, Temperature: 0}, imgs)
	if err != nil {
		e.logf("warn", "vision URL search: model error (%v)", err)
		return jobs.Official{}, false
	}
	picked := parseURLReply(raw)
	if picked == "" || !strings.HasPrefix(picked, "http") || jobs.IsAggregatorURL(picked) {
		e.logf("warn", "vision URL search: the model didn't identify a usable company URL")
		return jobs.Official{}, false
	}
	e.logf("info", "vision URL search: candidate careers page → %s", picked)

	// Re-resolve from the discovered URL so the crawler can drill from a careers
	// landing page down to the actual ATS/application link.
	probe := job
	probe.URL = picked
	probe.CompanyURL = picked
	res := jobs.ResolveOfficial(bctx, probe, e.officialURLPicker(provider), true)
	if strings.TrimSpace(res.ApplyURL) == "" {
		// Crawl found nothing more specific; the careers page itself is the result.
		return jobs.Official{ApplyURL: picked, CompanyURL: rootURL(picked), Note: "company careers page (via browser + vision search)"}, true
	}
	res.Note = strings.TrimSpace(res.Note + " (via browser + vision search)")
	return res, true
}

// buildCompanyURLPrompt asks the vision model to pick the company's own
// careers/application URL from the scraped candidate links (and what it can read
// in the screenshots), never an aggregator.
func buildCompanyURLPrompt(job store.Job, cands []browser.Link) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The attached images are screenshots of a web search for %q.\n", job.Company+" careers")
	fmt.Fprintf(&b, "Goal: find the official place to apply for jobs at %s", job.Company)
	if t := strings.TrimSpace(job.Title); t != "" {
		fmt.Fprintf(&b, " (the user is interested in a %q role)", t)
	}
	b.WriteString(" — the company's OWN careers/jobs page, or its applicant-tracking system (Greenhouse, Lever, Ashby, Workday, SmartRecruiters, Workable, iCIMS, …).\n")
	b.WriteString("\nCandidate result links (text -> url):\n")
	for _, l := range cands {
		t := strings.Join(strings.Fields(l.Text), " ")
		if len(t) > 80 {
			t = t[:80]
		}
		fmt.Fprintf(&b, "- %s -> %s\n", t, l.URL)
	}
	b.WriteString("\nChoose the SINGLE best official careers/application URL for this company. Strongly prefer a link from the list above; you may also use a URL clearly visible in the screenshots. NEVER choose a job board or aggregator (LinkedIn, Indeed, ZipRecruiter, Monster, SimplyHired, Glassdoor, Google, Bing, etc.).\n")
	b.WriteString(`Return ONLY a JSON object (no prose, no code fences): {"url":"<the company's own careers/application URL, or an empty string if none is present>"}.`)
	return b.String()
}

var urlInReplyRe = regexp.MustCompile(`https?://[^\s)<>"'\]}]+`)

// parseURLReply pulls the chosen URL out of the model's reply, tolerating code
// fences and surrounding prose; falls back to the first bare URL it finds.
func parseURLReply(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(strings.TrimPrefix(s, "json"), "JSON")
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	if start, end := strings.Index(s, "{"), strings.LastIndex(s, "}"); start >= 0 && end > start {
		var obj struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(s[start:end+1]), &obj) == nil && strings.TrimSpace(obj.URL) != "" {
			return strings.TrimSpace(obj.URL)
		}
	}
	if m := urlInReplyRe.FindString(s); m != "" {
		return strings.TrimRight(m, ".,;:!?)]}'\"")
	}
	return ""
}

// unwrapResultURL resolves a search-engine click-tracking redirect to the real
// destination it wraps (Bing's /ck/a?u=, DuckDuckGo's /l/?uddg=, Google's
// /url?q=). Non-redirect URLs are returned unchanged.
func unwrapResultURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	host := strings.ToLower(u.Host)
	q := u.Query()
	switch {
	case strings.Contains(host, "bing.com") && strings.Contains(u.Path, "/ck/"):
		if v := q.Get("u"); v != "" {
			// Bing prefixes the base64url payload with "a1".
			v = strings.TrimPrefix(v, "a1")
			for _, dec := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
				if b, err := dec.DecodeString(v); err == nil && strings.HasPrefix(string(b), "http") {
					return string(b)
				}
			}
		}
	case strings.Contains(host, "duckduckgo.com"):
		if v := q.Get("uddg"); v != "" {
			return v
		}
	case u.Path == "/url":
		if v := q.Get("q"); v != "" {
			return v
		}
		if v := q.Get("url"); v != "" {
			return v
		}
	}
	return raw
}

// visionProfileDir is the persistent Chrome profile the vision browser reuses so
// a solved Cloudflare challenge / warmed-up fingerprint survives between runs.
func (e *Engine) visionProfileDir() string {
	if e.dataDir == "" {
		return ""
	}
	return filepath.Join(e.dataDir, "vision-profile")
}

// rootURL returns scheme://host for a URL, or the input unchanged on parse error.
func rootURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return raw
}
