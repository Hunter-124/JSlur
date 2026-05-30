package jobs

// Connectable job boards and how a captured browser session is replayed on their
// scrape requests. The "connect account" flow (see internal/browser) opens the
// LoginURL, the user signs in / clears any bot check, and the cookies named in
// AuthCookies signal completion. Captured cookies + User-Agent are then sent on
// HTTP scrapes — which both authenticates (LinkedIn) and passes Cloudflare
// (cf_clearance is bound to the UA, so we replay both).

// AccountSpec describes a connectable source.
type AccountSpec struct {
	LoginURL    string   `json:"loginUrl"`
	AuthCookies []string `json:"-"`
	Hint        string   `json:"hint"`
}

// AccountSpecs lists the sources that benefit from a captured session. Indeed is
// intentionally absent — its keyless mobile API already works and uses no web
// cookies, so "connecting" it would add nothing.
var AccountSpecs = map[string]AccountSpec{
	"linkedin":     {"https://www.linkedin.com/login", []string{"li_at"}, "Log in to LinkedIn, then come back."},
	"ziprecruiter": {"https://www.ziprecruiter.com/jobs-search?search=jobs", []string{"cf_clearance", "__cf_bm", "zva"}, "Clear any 'are you human?' check (sign-in optional)."},
	"simplyhired":  {"https://www.simplyhired.com/search?q=jobs", []string{"cf_clearance", "__cf_bm"}, "Clear any 'are you human?' check."},
	"monster":      {"https://www.monster.com/jobs/search?q=jobs", []string{"cf_clearance", "__cf_bm"}, "Clear any 'are you human?' check."},
	"craigslist":   {"https://www.craigslist.org/", []string{"cl_b", "cl_def_lang"}, "Open Craigslist once to establish a session."},
}

// IsConnectable reports whether a source supports a captured account.
func IsConnectable(sourceID string) bool {
	_, ok := AccountSpecs[sourceID]
	return ok
}

// accountHeaders returns the Cookie + User-Agent headers for a source's captured
// session, or nil when none is connected.
func accountHeaders(q Query, sourceID string) map[string]string {
	a, ok := q.Creds.Accounts[sourceID]
	if !ok || a.Cookie == "" {
		return nil
	}
	h := map[string]string{"Cookie": a.Cookie}
	if a.UserAgent != "" {
		h["User-Agent"] = a.UserAgent
	}
	return h
}

// hasAccount reports whether a captured session exists for a source.
func hasAccount(q Query, sourceID string) bool {
	a, ok := q.Creds.Accounts[sourceID]
	return ok && a.Cookie != ""
}

// mergeHeaders combines header maps; later maps win. Returns nil if empty.
func mergeHeaders(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
