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
//
// AuthCookies names cookies that appear ONLY after the user is done (a genuine
// post-login cookie like LinkedIn's li_at, or a Cloudflare clearance that's set
// after the challenge passes). When that list is empty the capture can't tell
// when sign-in finished, so it keeps the window open and finishes when the user
// closes it. Never list cookies a site sets on first page load (e.g. __cf_bm,
// zva) — they'd make the window close instantly, before the user can sign in.
var AccountSpecs = map[string]AccountSpec{
	"linkedin": {"https://www.linkedin.com/login", []string{"li_at"}, "Log in to LinkedIn, then come back."},
	// ZipRecruiter gates results behind sign-in but exposes no distinct logged-in
	// cookie, so we wait for the user to close the window rather than auto-detect.
	"ziprecruiter": {"https://www.ziprecruiter.com/login", nil, "Sign in to ZipRecruiter, then close the browser window."},
	"simplyhired":  {"https://www.simplyhired.com/search?q=jobs", []string{"cf_clearance"}, "Clear any 'are you human?' check, then close the window."},
	"monster":      {"https://www.monster.com/jobs/search?q=jobs", []string{"cf_clearance"}, "Clear any 'are you human?' check, then close the window."},
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
