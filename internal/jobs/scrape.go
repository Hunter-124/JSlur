package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"autoapply/internal/config"
	"autoapply/internal/store"
)

// This file holds the shared toolkit for the scraping sources (LinkedIn, Indeed,
// ZipRecruiter, Monster, SimplyHired, Craigslist) and the company-site apply
// resolver. Unlike the keyed JSON APIs (Adzuna, USAJOBS), these talk to public
// web/mobile endpoints, so requests must look like they come from a real client.

// browserUA is a current desktop-Chrome User-Agent. Many boards reject the
// default Go user agent outright, so every scrape request carries this.
const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// fetch performs an HTTP request with browser-like headers (overridable) and
// returns the body. Non-2xx responses become errors carrying a short snippet,
// so a Cloudflare/anti-bot block surfaces as a clear, logged note rather than a
// crash. Bodies are capped to keep memory bounded.
func fetch(ctx context.Context, method, url string, headers map[string]string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(data))
		if len(snippet) > 160 {
			snippet = snippet[:160]
		}
		return nil, fmt.Errorf("http %d from %s: %s", resp.StatusCode, hostOf(url), snippet)
	}
	return data, nil
}

// getDoc GETs a URL with browser headers and returns the body as a string.
func getDoc(ctx context.Context, url string, headers map[string]string) (string, error) {
	b, err := fetch(ctx, http.MethodGet, url, headers, nil)
	return string(b), err
}

// hostOf returns the host of a URL for error messages, tolerating bad input.
func hostOf(raw string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	return s
}

// ---- JSON-LD JobPosting extraction --------------------------------------
//
// Most mainstream boards (and company career pages) embed schema.org JobPosting
// objects in <script type="application/ld+json"> for SEO. Reading those is far
// more robust than per-site CSS scraping, so several sources and the apply
// resolver lean on it.

var ldScriptRe = regexp.MustCompile(`(?is)<script[^>]*type=["']application/ld\+json["'][^>]*>(.*?)</script>`)

// ldJob is a normalized JobPosting parsed from JSON-LD.
type ldJob struct {
	Title       string
	Company     string
	CompanyURL  string // hiringOrganization.sameAs / url — the firm's own site
	Location    string
	Remote      bool
	URL         string // canonical posting/apply URL
	Description string
	Posted      string
	Salary      string
}

// extractJSONLDJobs pulls every JobPosting embedded in an HTML document.
func extractJSONLDJobs(doc string) []ldJob {
	var out []ldJob
	for _, m := range ldScriptRe.FindAllStringSubmatch(doc, -1) {
		raw := strings.TrimSpace(m[1])
		if raw == "" {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			continue // skip malformed blocks rather than failing the whole page
		}
		collectJobPostings(v, &out)
	}
	return out
}

// collectJobPostings walks arbitrary JSON-LD (object, array or @graph) and
// appends any JobPosting nodes it finds.
func collectJobPostings(v any, out *[]ldJob) {
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			collectJobPostings(e, out)
		}
	case map[string]any:
		if g, ok := t["@graph"]; ok {
			collectJobPostings(g, out)
		}
		if isLDType(t["@type"], "JobPosting") {
			if j := parseLDJob(t); j.Title != "" {
				*out = append(*out, j)
			}
		}
	}
}

// isLDType reports whether a JSON-LD @type value (string or []string) equals want.
func isLDType(v any, want string) bool {
	switch t := v.(type) {
	case string:
		return strings.EqualFold(t, want)
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && strings.EqualFold(s, want) {
				return true
			}
		}
	}
	return false
}

func parseLDJob(m map[string]any) ldJob {
	j := ldJob{
		Title:       ldStr(m["title"]),
		URL:         ldStr(m["url"]),
		Posted:      ldStr(m["datePosted"]),
		Description: stripHTML(ldStr(m["description"])),
	}

	// hiringOrganization: object {name, sameAs/url} or a bare string.
	switch org := m["hiringOrganization"].(type) {
	case map[string]any:
		j.Company = ldStr(org["name"])
		j.CompanyURL = firstNonEmpty(ldStr(org["sameAs"]), ldStr(org["url"]))
	case string:
		j.Company = org
	}

	// jobLocation: object or array of objects, each with an address.
	j.Location = ldLocation(m["jobLocation"])

	// Remote signals.
	if strings.EqualFold(ldStr(m["jobLocationType"]), "TELECOMMUTE") || m["applicantLocationRequirements"] != nil {
		j.Remote = true
	}
	if looksRemote(j.Title, j.Location, j.Description) {
		j.Remote = true
	}

	j.Salary = ldSalary(m["baseSalary"])
	return j
}

// ldLocation renders the first usable address from a jobLocation value as
// "City, ST" (or a country/locality fragment when that's all there is).
func ldLocation(v any) string {
	var addr map[string]any
	switch t := v.(type) {
	case map[string]any:
		addr = mapField(t, "address")
		if addr == nil {
			addr = t
		}
	case []any:
		for _, e := range t {
			if em, ok := e.(map[string]any); ok {
				if a := mapField(em, "address"); a != nil {
					addr = a
					break
				}
			}
		}
	}
	if addr == nil {
		return ""
	}
	city := ldStr(addr["addressLocality"])
	region := ldStr(addr["addressRegion"])
	country := ldStr(addr["addressCountry"])
	switch {
	case city != "" && region != "":
		return city + ", " + region
	case city != "":
		return city
	case region != "":
		return region
	default:
		return country
	}
}

// ldSalary renders a baseSalary node when present, best effort.
func ldSalary(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	val := mapField(m, "value")
	if val == nil {
		return ""
	}
	min := ldNum(val["minValue"])
	max := ldNum(val["maxValue"])
	if min == 0 && max == 0 {
		min = ldNum(val["value"])
	}
	return formatUSD(min, max)
}

// ---- small JSON-LD value helpers ----

func ldStr(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return fmt.Sprintf("%g", t)
	case map[string]any:
		// Some publishers nest text as {"@value": "..."}.
		if s, ok := t["@value"].(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func ldNum(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		var f float64
		_, _ = fmt.Sscanf(t, "%g", &f)
		return int(f)
	}
	return 0
}

func mapField(m map[string]any, key string) map[string]any {
	if sub, ok := m[key].(map[string]any); ok {
		return sub
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// looksRemote reports whether any of the given strings mention remote work.
func looksRemote(parts ...string) bool {
	s := strings.ToLower(strings.Join(parts, " "))
	for _, kw := range []string{"remote", "work from home", "work-from-home", "telecommute", "anywhere", "wfh"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// searchQuery is the keyword string the keyword-based boards search on. These
// boards are search engines, not category taxonomies, so the user's free-text
// interest is the query.
func searchQuery(focus config.JobFocus) string {
	return strings.TrimSpace(focus.Interest)
}

// roleSepRe splits an interest into separate role queries on explicit separators
// (slash, newline, semicolon, pipe). Commas are intentionally NOT separators —
// they occur inside single role phrases ("Accounting, Finance").
var roleSepRe = regexp.MustCompile(`\s*[/\n;|]\s*`)

// searchQueries returns the role queries to search. A single interest like
// "manufacturing / mechanical engineering" becomes two queries, so the keyword
// boards can search several roles at once. Returns nil when no interest is set.
func searchQueries(focus config.JobFocus) []string {
	interest := strings.TrimSpace(focus.Interest)
	if interest == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range roleSepRe.Split(interest, -1) {
		part = strings.TrimSpace(part)
		key := strings.ToLower(part)
		if part != "" && !seen[key] {
			seen[key] = true
			out = append(out, part)
		}
	}
	if len(out) > 5 {
		out = out[:5] // cap to keep request volume sane
	}
	return out
}

// ---- Craigslist region resolution ----
//
// Craigslist is partitioned into per-metro subdomains, so a search needs one.
// We map common metros (and a state fallback) to subdomains; unknown locations
// skip the source rather than guessing wrong.

var clMetros = map[string]string{
	"new york": "newyork", "nyc": "newyork", "brooklyn": "newyork", "manhattan": "newyork",
	"los angeles": "losangeles", "la": "losangeles", "long beach": "losangeles",
	"chicago": "chicago", "houston": "houston", "phoenix": "phoenix",
	"philadelphia": "philadelphia", "philly": "philadelphia", "san antonio": "sanantonio",
	"san diego": "sandiego", "dallas": "dallas", "fort worth": "dallas", "austin": "austin",
	"san jose": "sfbay", "san francisco": "sfbay", "sf": "sfbay", "oakland": "sfbay",
	"seattle": "seattle", "boston": "boston", "denver": "denver", "atlanta": "atlanta",
	"miami": "miami", "washington": "washingtondc", "washington dc": "washingtondc", "dc": "washingtondc",
	"portland": "portland", "las vegas": "lasvegas", "detroit": "detroit",
	"minneapolis": "minneapolis", "st paul": "minneapolis", "nashville": "nashville",
	"charlotte": "charlotte", "orlando": "orlando", "tampa": "tampa", "sacramento": "sacramento",
	"pittsburgh": "pittsburgh", "cleveland": "cleveland", "columbus": "columbus",
	"kansas city": "kansascity", "st louis": "stlouis", "salt lake city": "saltlakecity",
	"indianapolis": "indianapolis", "raleigh": "raleigh", "baltimore": "baltimore",
}

var clStates = map[string]string{
	"ny": "newyork", "ca": "losangeles", "il": "chicago", "tx": "dallas",
	"wa": "seattle", "ma": "boston", "co": "denver", "ga": "atlanta",
	"fl": "miami", "or": "portland", "nv": "lasvegas", "mi": "detroit",
	"mn": "minneapolis", "tn": "nashville", "nc": "charlotte", "pa": "philadelphia",
	"oh": "columbus", "az": "phoenix", "ut": "saltlakecity", "mo": "stlouis",
	"in": "indianapolis", "md": "baltimore", "dc": "washingtondc",
}

// craigslistRegion picks a Craigslist subdomain for a location, or "" if none
// is recognised.
func craigslistRegion(loc config.Location) string {
	if r, ok := clMetros[strings.ToLower(strings.TrimSpace(loc.City))]; ok {
		return r
	}
	if r, ok := clStates[strings.ToLower(strings.TrimSpace(loc.State))]; ok {
		return r
	}
	return ""
}

// ldJobsToStore converts JSON-LD jobs into store.Job records for a source,
// de-duping by URL and capping at max. Used by the boards that rely on embedded
// schema.org JobPosting markup (Monster, SimplyHired).
func ldJobsToStore(srcID string, lds []ldJob, max int) []store.Job {
	out := map[string]store.Job{}
	for _, l := range lds {
		link := strings.TrimSpace(l.URL)
		if link == "" || l.Title == "" {
			continue
		}
		id := store.MakeJobID(srcID, link)
		if _, dup := out[id]; dup {
			continue
		}
		out[id] = store.Job{
			ID:          id,
			Source:      srcID,
			Title:       l.Title,
			Company:     l.Company,
			CompanyURL:  l.CompanyURL,
			Location:    l.Location,
			Remote:      l.Remote,
			URL:         link,
			Description: l.Description,
			Salary:      l.Salary,
			PostedAt:    parseTime(l.Posted),
		}
		if len(out) >= max {
			break
		}
	}
	return mapToSlice(out)
}
