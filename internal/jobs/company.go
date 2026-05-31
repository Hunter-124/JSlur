package jobs

import (
	"context"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"autoapply/internal/store"
)

// This file resolves the "official" application URL for a job: the link on the
// company's OWN site or applicant-tracking system (ATS), as opposed to the
// aggregator/board the posting was discovered on. The engine records this so a
// listing found on LinkedIn, Indeed or a forum can be applied to at the source.

// atsHosts maps known ATS domain suffixes to a friendly name. A posting URL on
// one of these is, by definition, the company's own application flow.
var atsHosts = map[string]string{
	"greenhouse.io":       "Greenhouse",
	"lever.co":            "Lever",
	"ashbyhq.com":         "Ashby",
	"myworkdayjobs.com":   "Workday",
	"myworkdaysite.com":   "Workday",
	"smartrecruiters.com": "SmartRecruiters",
	"workable.com":        "Workable",
	"bamboohr.com":        "BambooHR",
	"jobvite.com":         "Jobvite",
	"icims.com":           "iCIMS",
	"recruitee.com":       "Recruitee",
	"teamtailor.com":      "Teamtailor",
	"breezy.hr":           "Breezy",
	"applytojob.com":      "JazzHR",
	"taleo.net":           "Taleo",
	"successfactors.com":  "SuccessFactors",
	"rippling.com":        "Rippling",
	"paylocity.com":       "Paylocity",
	"paycomonline.net":    "Paycom",
	"eightfold.ai":        "Eightfold",
	"oraclecloud.com":     "Oracle",
	"jobs.sap.com":        "SuccessFactors",
}

// aggregatorHosts are job boards / link shorteners / social sites that are never
// the company's own application destination.
var aggregatorHosts = []string{
	"linkedin.com", "lnkd.in", "indeed.com", "ziprecruiter.com", "monster.com",
	"simplyhired.com", "craigslist.org", "glassdoor.com", "themuse.com",
	"remotive.com", "weworkremotely.com", "remoteok.com", "remoteok.io",
	"dice.com", "adzuna.com", "usajobs.gov", "ycombinator.com",
	"snagajob.com", "careerbuilder.com", "flexjobs.com", "wellfound.com",
	"angel.co", "google.com", "bing.com", "facebook.com", "twitter.com",
	"x.com", "t.co", "bit.ly", "youtube.com", "instagram.com", "reddit.com",
}

var (
	urlInTextRe  = regexp.MustCompile(`https?://[^\s)<>"'\]\}]+`)
	careerHintRe = regexp.MustCompile(`(?i)(careers?|jobs?|join-?us|join/|apply|openings?|positions?|work-?with-us|employment|hiring|recruit|gh_jid|workday)`)
)

// detectATS returns the ATS name for a URL, or "" if it's not a recognised ATS.
func detectATS(rawurl string) string {
	h := strings.ToLower(hostOf(rawurl))
	for suffix, name := range atsHosts {
		if h == suffix || strings.HasSuffix(h, "."+suffix) {
			return name
		}
	}
	return ""
}

// IsAggregatorURL reports whether a URL points at a job board, social site or
// link shortener — i.e. never a company's own application destination. Exposed
// so callers like the engine's browser+vision URL fallback can vet the links it
// scrapes off a search-results page before handing them to the model.
func IsAggregatorURL(rawurl string) bool { return isAggregator(rawurl) }

// isAggregator reports whether a URL points at a job board / social / shortener.
func isAggregator(rawurl string) bool {
	h := strings.ToLower(hostOf(rawurl))
	for _, suffix := range aggregatorHosts {
		if h == suffix || strings.HasSuffix(h, "."+suffix) {
			return true
		}
	}
	return false
}

// OfficialURLPicker optionally lets the caller use AI to choose the best
// official application URL given the listing and the candidate links we found
// (and to infer one from the company name when no link is present). It mirrors
// the CategorySelector pattern so the jobs package stays free of an AI import.
type OfficialURLPicker func(ctx context.Context, job store.Job, candidates []string) (string, error)

// Official is the resolved application destination.
type Official struct {
	ApplyURL   string // best application URL on the company's own site/ATS
	CompanyURL string // the company's careers/site root, when known
	ATS        string // detected ATS name, "" if unknown
	Note       string // short human explanation of how it was found
}

// ResolveOfficial finds the company's own application URL for a job.
//
// It first mines the listing (its URL + any links in the description), preferring
// ATS and careers links over aggregators. If that's inconclusive it asks the
// optional AI picker. When deep is true it will additionally fetch the company's
// site and crawl for a careers/apply link (used by the manual button, not the
// hot pipeline path).
func ResolveOfficial(ctx context.Context, job store.Job, pick OfficialURLPicker, deep bool) Official {
	cands := collectCandidates(job)
	best, score := bestCandidate(cands)

	companyURL := strings.TrimSpace(job.CompanyURL)
	if companyURL == "" {
		for _, c := range cands {
			if !isAggregator(c) && careerHintRe.MatchString(c) {
				companyURL = rootOf(c)
				break
			}
		}
	}

	// 1. A strong link from the listing itself (ATS or clear careers/apply URL).
	if best != "" && score >= 30 {
		return Official{ApplyURL: best, CompanyURL: firstNonEmpty(companyURL, rootOf(best)), ATS: detectATS(best), Note: "found in listing"}
	}

	// 2. Ask the AI to choose / infer (cheap: no network).
	if pick != nil {
		if u, err := pick(ctx, job, cands); err == nil {
			if u = strings.TrimSpace(u); u != "" && strings.HasPrefix(u, "http") && !isAggregator(u) {
				return Official{ApplyURL: u, CompanyURL: firstNonEmpty(companyURL, rootOf(u)), ATS: detectATS(u), Note: "chosen by AI"}
			}
		}
	}

	// 3. Deep: find the company's own site (deriving it from the name when the
	//    listing didn't provide one), then look for a careers/apply link on it —
	//    first by crawling the homepage, then by trying conventional careers paths.
	if deep {
		if companyURL == "" {
			companyURL = guessCompanySite(ctx, job.Company)
		}
		if companyURL != "" {
			if u := crawlCareers(ctx, companyURL, job); u != "" {
				return Official{ApplyURL: u, CompanyURL: companyURL, ATS: detectATS(u), Note: careersNote(u)}
			}
			if u := probeCareers(ctx, companyURL); u != "" {
				return Official{ApplyURL: u, CompanyURL: companyURL, ATS: detectATS(u), Note: careersNote(u)}
			}
		}
	}

	// 4. Fall back to the best non-aggregator candidate, even if weak.
	if best != "" {
		return Official{ApplyURL: best, CompanyURL: firstNonEmpty(companyURL, rootOf(best)), ATS: detectATS(best), Note: "best available link"}
	}

	// 5. Last resort: if we at least know the company's website, point there so
	//    the user can reach its Careers section themselves — better than nothing.
	if companyURL != "" {
		return Official{ApplyURL: companyURL, CompanyURL: companyURL, Note: "company website — couldn't find a specific apply page; check the Careers section"}
	}
	return Official{Note: "no official URL found"}
}

// careersNote describes how a resolved company URL should be labelled.
func careersNote(u string) string {
	if ats := detectATS(u); ats != "" {
		return "company application (" + ats + ")"
	}
	return "found on company site"
}

// companySuffixes are legal/marketing tokens dropped from a company name before
// guessing its domain.
var companySuffixes = map[string]bool{
	"inc": true, "llc": true, "ltd": true, "corp": true, "co": true, "company": true,
	"corporation": true, "incorporated": true, "group": true, "holdings": true,
	"plc": true, "gmbh": true, "technologies": true, "technology": true, "labs": true,
	"systems": true, "solutions": true, "services": true, "and": true,
}

// companySlug reduces a company name to a lowercase, domain-friendly token:
// drops legal suffixes and anything that isn't a letter or digit.
func companySlug(company string) string {
	var kept []string
	for _, f := range strings.Fields(strings.ToLower(company)) {
		f = strings.Trim(f, ".,&")
		if f == "" || companySuffixes[f] {
			continue
		}
		kept = append(kept, f)
	}
	var b strings.Builder
	for _, r := range strings.Join(kept, "") {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// guessCompanySite finds the company's own website by trying likely domains
// built from its name and returning the first that resolves to a real site
// (not a parked/registrar page). Best-effort and network-bound, so it runs only
// on the deep (manual) resolution path and respects the context deadline.
func guessCompanySite(ctx context.Context, company string) string {
	slug := companySlug(company)
	if len(slug) < 2 {
		return ""
	}
	for _, host := range []string{slug + ".com", slug + ".io", slug + ".co", slug + ".ai", "get" + slug + ".com"} {
		if ctx.Err() != nil {
			return ""
		}
		u := "https://" + host
		if isAggregator(u) {
			continue
		}
		doc, err := getDoc(ctx, u, nil)
		if err != nil {
			continue
		}
		// Guard against parked domains: a genuine company homepage almost always
		// names itself or links its careers/about/contact pages.
		low := strings.ToLower(doc)
		if strings.Contains(low, slug) || strings.Contains(low, "careers") ||
			strings.Contains(low, "about") || strings.Contains(low, "contact") {
			return u
		}
	}
	return ""
}

// careerPaths are conventional locations of a company's careers/jobs page,
// tried when crawling the homepage didn't surface one.
var careerPaths = []string{
	"/careers", "/careers/jobs", "/jobs", "/join-us", "/join",
	"/company/careers", "/about/careers", "/work-with-us", "/en/careers",
}

// probeCareers tries conventional careers/jobs paths on the company root and
// returns the first that loads — preferring an ATS/apply link found on that
// page, otherwise the careers page itself. Bounded by ctx; deep path only.
func probeCareers(ctx context.Context, companyURL string) string {
	root := rootOf(companyURL)
	if root == "" {
		return ""
	}
	for _, p := range careerPaths {
		if ctx.Err() != nil {
			return ""
		}
		u := root + p
		doc, err := getDoc(ctx, u, nil)
		if err != nil {
			continue
		}
		if apply := bestLink(u, doc, false); apply != "" && detectATS(apply) != "" {
			return apply
		}
		return u
	}
	return ""
}

// collectCandidates gathers every plausible official URL for a job: its own URL
// (unless that's an aggregator), any links in the description, and a stored
// company URL. Aggregator/social links are dropped.
func collectCandidates(job store.Job) []string {
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		u = trimURL(u)
		if u == "" || seen[u] || isAggregator(u) {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	if !isAggregator(job.URL) {
		add(job.URL)
	}
	for _, m := range urlInTextRe.FindAllString(job.Description, -1) {
		add(m)
	}
	add(job.CompanyURL)
	return out
}

// bestCandidate scores candidates and returns the top one with its score.
func bestCandidate(cands []string) (string, int) {
	type scored struct {
		u string
		n int
	}
	var ranked []scored
	for _, u := range cands {
		ranked = append(ranked, scored{u, scoreURL(u)})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].n > ranked[j].n })
	if len(ranked) == 0 || ranked[0].n < 0 {
		return "", 0
	}
	return ranked[0].u, ranked[0].n
}

// scoreURL ranks how likely a URL is the company's own application page.
func scoreURL(u string) int {
	if isAggregator(u) {
		return -100
	}
	s := 0
	if detectATS(u) != "" {
		s += 100
	}
	host, path := hostOf(u), pathOf(u)
	if careerHintRe.MatchString(path) {
		s += 30
	}
	if strings.HasPrefix(strings.ToLower(host), "careers.") || strings.HasPrefix(strings.ToLower(host), "jobs.") {
		s += 25
	}
	return s
}

// crawlCareers fetches the company root, finds the most careers-like link,
// follows it once, and returns the best ATS/apply link found there (or the
// careers page itself). Strictly best-effort and bounded to two fetches.
func crawlCareers(ctx context.Context, companyURL string, job store.Job) string {
	doc, err := getDoc(ctx, companyURL, nil)
	if err != nil {
		return ""
	}
	careers := bestLink(companyURL, doc, true)
	if careers == "" {
		// Maybe the page we fetched is already a board with JSON-LD jobs.
		if jobsLD := extractJSONLDJobs(doc); len(jobsLD) > 0 {
			return firstNonEmpty(jobsLD[0].URL, companyURL)
		}
		return ""
	}
	if detectATS(careers) != "" {
		return careers
	}
	// Follow the careers page once and look for an apply/ATS link there.
	if cdoc, err := getDoc(ctx, careers, nil); err == nil {
		if apply := bestLink(careers, cdoc, false); apply != "" {
			return apply
		}
		if jobsLD := extractJSONLDJobs(cdoc); len(jobsLD) > 0 && jobsLD[0].URL != "" {
			return jobsLD[0].URL
		}
	}
	return careers
}

var hrefRe = regexp.MustCompile(`(?i)href=["']([^"']+)["']`)

// bestLink returns the highest-scoring absolute link in doc, resolved against
// base. When careersOnly is true it only considers career/apply-looking links.
func bestLink(base, doc string, careersOnly bool) string {
	baseU, err := url.Parse(base)
	if err != nil {
		return ""
	}
	best, bestScore := "", 0
	seen := map[string]bool{}
	for _, m := range hrefRe.FindAllStringSubmatch(doc, -1) {
		ref, err := url.Parse(strings.TrimSpace(m[1]))
		if err != nil {
			continue
		}
		abs := baseU.ResolveReference(ref).String()
		if seen[abs] || isAggregator(abs) || !strings.HasPrefix(abs, "http") {
			continue
		}
		seen[abs] = true
		score := scoreURL(abs)
		if careersOnly && score < 25 {
			continue
		}
		if score > bestScore {
			best, bestScore = abs, score
		}
	}
	return best
}

// trimURL strips trailing punctuation that regularly clings to URLs in prose.
func trimURL(u string) string {
	u = strings.TrimSpace(u)
	return strings.TrimRight(u, ".,;:!?)]}'\"")
}

func pathOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Path
	}
	return ""
}

func rootOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return raw
}
