package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"autoapply/internal/browser"
	"autoapply/internal/config"
	"autoapply/internal/store"
)

// visionBrowser searches job boards the legitimate way: it drives a real browser
// to each board's normal search-results page (replaying any connected session so
// it looks like a signed-in human), screenshots the rendered page, and asks a
// vision-capable AI model to read the listings off the images. Because it
// navigates like a person and never touches a raw scrape/HTTP endpoint, it isn't
// rate-limited or Cloudflare-blocked the way the plain-HTTP scrapers can be.
//
// It needs a vision-capable AI provider, injected by the engine as q.Vision.
// Without one it returns a friendly note and the aggregator skips it.
type visionBrowser struct{}

func (visionBrowser) ID() string             { return "visionbrowser" }
func (visionBrowser) Name() string           { return "AI Browser Search (vision)" }
func (visionBrowser) NeedsCredentials() bool { return false }

// visionBoard describes one board the vision searcher knows how to drive.
type visionBoard struct {
	id   string
	name string
	// account is the connected-account id whose session (cookies + UA) to replay,
	// or "" when the board needs none for a logged-out search.
	account string
	// requiresAccount marks boards that gate search/results behind a sign-in
	// (LinkedIn, ZipRecruiter): without a connected session they show a sign-in
	// wall and return nothing, so we skip them with a clear note rather than
	// wasting a page load + vision call. Boards like Indeed/Google are public
	// (bot-walled at worst), so this is false for them.
	requiresAccount bool
	// searchURL builds the human results-page URL for a keyword + focus.
	searchURL func(keyword string, focus config.JobFocus) string
	// isPosting reports whether a link looks like an individual posting on this
	// board, so we can hand the model real URLs to attach to each listing.
	isPosting func(string) bool
}

var visionBoardList = []visionBoard{
	{
		id: "indeed", name: "Indeed",
		searchURL: func(kw string, f config.JobFocus) string {
			return "https://www.indeed.com/jobs?q=" + url.QueryEscape(kw) + "&l=" + url.QueryEscape(f.Location.Query())
		},
		isPosting: func(u string) bool {
			return strings.Contains(u, "/rc/clk") || strings.Contains(u, "/viewjob") || strings.Contains(u, "jk=") || strings.Contains(u, "/pagead/clk")
		},
	},
	{
		id: "linkedin", name: "LinkedIn", account: "linkedin", requiresAccount: true,
		searchURL: func(kw string, f config.JobFocus) string {
			// Place name, never a bare ZIP: LinkedIn geocodes this globally and
			// reads a US ZIP as a foreign postal code (see linkedinLocation).
			loc := linkedinLocation(f.Location)
			if loc == "" {
				loc = "United States"
			}
			return "https://www.linkedin.com/jobs/search?keywords=" + url.QueryEscape(kw) + "&location=" + url.QueryEscape(loc)
		},
		isPosting: func(u string) bool { return strings.Contains(u, "/jobs/view/") },
	},
	{
		id: "ziprecruiter", name: "ZipRecruiter", account: "ziprecruiter", requiresAccount: true,
		searchURL: func(kw string, f config.JobFocus) string {
			return "https://www.ziprecruiter.com/jobs-search?search=" + url.QueryEscape(kw) + "&location=" + url.QueryEscape(f.Location.Query())
		},
		isPosting: func(u string) bool {
			return strings.Contains(u, "/jobs/") || strings.Contains(u, "/job/") || strings.Contains(u, "/c/")
		},
	},
	{
		id: "handshake", name: "Handshake", account: "handshake", requiresAccount: true,
		searchURL: func(kw string, f config.JobFocus) string { return handshakeSearchURL(kw) },
		isPosting: isHandshakePosting,
	},
	{
		id: "google", name: "Google Jobs",
		searchURL: func(kw string, f config.JobFocus) string {
			q := strings.TrimSpace(kw + " jobs " + f.Location.Query())
			return "https://www.google.com/search?ibp=htl;jobs&q=" + url.QueryEscape(q)
		},
		// Google Jobs renders results as JS panels with no per-listing href, so we
		// rely on the visible title/company alone (url comes back empty).
		isPosting: func(string) bool { return false },
	},
}

var visionBoards = func() map[string]visionBoard {
	m := make(map[string]visionBoard, len(visionBoardList))
	for _, b := range visionBoardList {
		m[b.id] = b
	}
	return m
}()

func (vb visionBrowser) Search(ctx context.Context, q Query) ([]store.Job, error) {
	if q.Vision == nil {
		return nil, fmt.Errorf("needs a vision-capable AI provider — set one in Settings, then enable this source")
	}
	queries := searchQueries(q.Focus)
	if len(queries) == 0 {
		if q.Focus.Location.Query() == "" {
			return nil, fmt.Errorf("describe your target roles (or a location) in Job Focus to use AI Browser Search")
		}
		queries = []string{""}
	}
	if len(queries) > 3 {
		queries = queries[:3] // browser rendering is slow; cap roles
	}

	boards := q.Creds.Browser.Boards
	if len(boards) == 0 {
		boards = []string{"indeed", "linkedin"}
	}
	perBoard := limit(q.Focus)
	maxScreens := q.Creds.Browser.MaxScreens
	if maxScreens <= 0 {
		maxScreens = 3
	}

	sess, engineNote, err := browser.NewShooter(ctx, browser.SessionOptions{
		Headful:        q.Creds.Browser.Headful,
		ProfileDir:     q.BrowserProfileDir,
		Engine:         q.Creds.Browser.Engine(),
		PythonPath:     q.Creds.Browser.PythonPath,
		MaxConcurrency: q.Creds.Browser.ScrapeConcurrency(),
		Notify:         func(level, msg string) { q.logf(level, "%s", msg) },
		OnBlock:        q.OnBlock,
	})
	if err != nil {
		return nil, fmt.Errorf("could not start a browser for vision search: %w", err)
	}
	defer sess.Close()

	mode := "headless"
	if q.Creds.Browser.Headful {
		mode = "a visible"
	}
	q.logf("info", "AI Browser Search: using the %s; driving %s browser across %d board(s) — slower than the other sources, hang tight", engineNote, mode, len(boards))

	// Build the (board × role) work list, resolving each board's connected session
	// up front (and logging the per-board account decisions sequentially so the log
	// stays readable). The actual page loads + vision reads then run in parallel.
	type visionTask struct {
		b          visionBoard
		kw         string
		label      string
		pageURL    string
		cookie, ua string
	}
	var tasks []visionTask
	for _, boardID := range boards {
		b, ok := visionBoards[boardID]
		if !ok {
			continue
		}
		cookie, ua := "", ""
		if b.account != "" {
			if acc, ok := q.Creds.Accounts[b.account]; ok && acc.Cookie != "" {
				cookie, ua = acc.Cookie, acc.UserAgent
			} else if b.requiresAccount {
				// No session, and this board gates search behind sign-in — searching
				// logged-out would just screenshot a sign-in wall, so skip it.
				q.logf("warn", "%s requires a connected account (it hides search results behind sign-in) — connect it under Settings → Connected accounts, then re-run. Skipping.", b.name)
				continue
			} else {
				q.logf("info", "%s: no connected account — searching logged-out (connect it in Settings for better results)", b.name)
			}
		}
		for _, kw := range queries {
			label := b.name
			if kw != "" {
				label = fmt.Sprintf("%s — %q", b.name, kw)
			}
			tasks = append(tasks, visionTask{b: b, kw: kw, label: label, pageURL: b.searchURL(kw, q.Focus), cookie: cookie, ua: ua})
		}
	}

	var mu sync.Mutex
	out := map[string]store.Job{}
	var firstErr error
	recordErr := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}

	// Run several boards/roles at once — the stealth sidecar loads them in parallel
	// tabs and the vision calls are independent. Bounded by the configured
	// concurrency so we don't overwhelm the browser or the AI endpoint.
	workers := q.Creds.Browser.ScrapeConcurrency()
	if workers > len(tasks) {
		workers = len(tasks)
	}
	if workers < 1 {
		workers = 1
	}
	jobCh := make(chan visionTask)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobCh {
				if ctx.Err() != nil {
					continue
				}
				q.logf("info", "%s: opening search results…", t.label)
				shots, err := sess.Shots(ctx, t.pageURL, t.cookie, t.ua, maxScreens)
				if err != nil {
					recordErr(err)
					q.logf("warn", "%s: could not load the page (%v)", t.label, err)
					continue
				}
				if len(shots.Images) == 0 {
					q.logf("warn", "%s: captured no screenshots", t.label)
					continue
				}
				// Prefer the engine's structured block detection; fall back to the
				// title/text heuristic for the chromedp path / older responses.
				if reason := shots.BlockReason; reason != "" {
					q.logf("warn", "%s: the page hit %s (%q) — connect this account in Settings or switch to visible/headful mode to solve it once", t.label, reason, strings.TrimSpace(shots.Title))
				} else if reason := looksLikeWall(shots.Title, shots.Text); reason != "" {
					q.logf("warn", "%s: the page looks like %s (%q) — connect this account in Settings or try another board", t.label, reason, strings.TrimSpace(shots.Title))
				}
				found := vb.readListings(ctx, q, t.b, shots, perBoard, recordErr)
				if len(found) == 0 {
					q.logf("info", "%s: the model read no listings from %d screenshot(s) (page title: %q)", t.label, len(shots.Images), strings.TrimSpace(shots.Title))
				} else {
					q.logf("info", "%s: read %d listing(s) from %d screenshot(s)", t.label, len(found), len(shots.Images))
				}
				mu.Lock()
				for _, j := range found {
					if _, dup := out[j.ID]; !dup {
						out[j.ID] = j
					}
				}
				mu.Unlock()
			}
		}()
	}
	for _, t := range tasks {
		jobCh <- t
	}
	close(jobCh)
	wg.Wait()

	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return mapToSlice(out), nil
}

// wallSignals: looksLikeWall returns a short human reason when a page title/text
// matches a known bot-check/captcha interstitial or a sign-in wall — the usual
// reason a board returns "no jobs". It's only used to warn the user; the
// screenshots are still sent to the vision model regardless (the heuristic can
// false-positive, and the model sometimes reads a partially-rendered page).
func looksLikeWall(title, text string) string {
	t := strings.ToLower(title)
	hay := strings.ToLower(title + "\n" + text)
	// Hard bot/captcha interstitials — distinctive phrases, safe to match anywhere.
	for _, s := range []struct{ needle, reason string }{
		{"just a moment", "a Cloudflare bot check"},
		{"attention required", "a Cloudflare block"},
		{"verify you are human", "a human-verification check"},
		{"please verify you are a human", "a human-verification check"},
		{"are you a robot", "a bot check"},
		{"px-captcha", "a captcha"},
		{"unusual traffic", "a rate-limit / bot check"},
		{"security check", "a security check"},
		{"press & hold", "a press-and-hold bot check"},
	} {
		if strings.Contains(hay, s.needle) {
			return s.reason
		}
	}
	// Sign-in / sign-up walls — only trust the page title here, since a real
	// results page also carries "sign in" links in its body and would mis-match.
	for _, s := range []struct{ needle, reason string }{
		{"sign in", "a sign-in wall"},
		{"sign up", "a sign-up wall"},
		{"log in", "a sign-in wall"},
		{"login", "a sign-in wall"},
		{"join linkedin", "a LinkedIn sign-in wall"},
	} {
		if strings.Contains(t, s.needle) {
			return s.reason
		}
	}
	return ""
}

// readListings sends the screenshots (plus candidate posting links) to the
// vision model and converts its JSON reply into store.Job records.
func (vb visionBrowser) readListings(ctx context.Context, q Query, b visionBoard, shots browser.Shots, max int, recordErr func(error)) []store.Job {
	// Candidate posting links for the model to attach to listings. Keep the query
	// string — some boards (e.g. Indeed) carry the job id there (?jk=…) — and drop
	// only the fragment.
	var links []browser.Link
	for _, l := range shots.Links {
		if b.isPosting != nil && b.isPosting(l.URL) {
			links = append(links, browser.Link{Text: l.Text, URL: stripFragment(l.URL)})
		}
		if len(links) >= 60 {
			break
		}
	}

	raw, err := q.Vision(ctx, buildVisionPrompt(b.name, q.Focus, links, max), shots.Images)
	if err != nil {
		recordErr(err)
		return nil
	}
	parsed, err := parseVisionJobs(raw)
	if err != nil {
		recordErr(err)
		return nil
	}

	out := make([]store.Job, 0, len(parsed))
	for _, p := range parsed {
		title := strings.TrimSpace(p.Title)
		if title == "" {
			continue
		}
		link := stripFragment(strings.TrimSpace(p.URL))
		if link != "" && !strings.HasPrefix(link, "http") {
			link = "" // ignore relative/garbage the model may have invented
		}
		// Stable id: the posting URL when present, else a board+title+company key so
		// the same listing isn't re-added on every search.
		seed := link
		if seed == "" {
			seed = "v|" + b.id + "|" + strings.ToLower(title+"|"+p.Company)
		}
		id := store.MakeJobID(vb.ID(), seed)
		out = append(out, store.Job{
			ID:          id,
			Source:      vb.ID(),
			Title:       title,
			Company:     strings.TrimSpace(p.Company),
			Location:    strings.TrimSpace(p.Location),
			Remote:      p.Remote || looksRemote(title, p.Location),
			URL:         link,
			Salary:      strings.TrimSpace(p.Salary),
			Description: strings.TrimSpace(p.Description),
		})
		if len(out) >= max {
			break
		}
	}
	return out
}

// visionJob is one listing as returned by the vision model.
type visionJob struct {
	Title       string `json:"title"`
	Company     string `json:"company"`
	Location    string `json:"location"`
	Remote      bool   `json:"remote"`
	Salary      string `json:"salary"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

func buildVisionPrompt(boardName string, focus config.JobFocus, links []browser.Link, max int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The attached images are screenshots of the %s job-search results page.\n", boardName)
	if interest := strings.TrimSpace(focus.Interest); interest != "" {
		fmt.Fprintf(&b, "The user is looking for: %s\n", interest)
	}
	b.WriteString("\nRead EVERY distinct job listing visible across the images and return them as JSON.\n")
	if len(links) > 0 {
		b.WriteString("\nHere are hyperlinks found on the same page (text → url). For each listing, set \"url\" to the matching link from this list (match by the job title / company text). If you can't confidently match one, use an empty string — do NOT invent or guess a URL.\n")
		for _, l := range links {
			t := strings.Join(strings.Fields(l.Text), " ")
			if len(t) > 80 {
				t = t[:80]
			}
			fmt.Fprintf(&b, "- %s → %s\n", t, l.URL)
		}
	}
	fmt.Fprintf(&b, "\nReturn ONLY a JSON array (no prose, no code fences), at most %d items, of exactly this shape:\n", max)
	b.WriteString(`[{"title":"","company":"","location":"City, ST","remote":false,"salary":"","url":"","description":""}]`)
	b.WriteString("\nRules: include only real listings you can actually see in the images; copy the title, company and location exactly as shown; set \"salary\" only if a pay figure is shown; set \"remote\" true only if the listing says remote/work-from-home; set \"description\" to the short summary snippet shown on the card (one or two lines, or empty if none). If no jobs are visible, return [].")
	return b.String()
}

// stripFragment removes a URL's #fragment (and trims space) while preserving the
// query string, which on some boards carries the posting id.
func stripFragment(u string) string {
	u = strings.TrimSpace(u)
	if i := strings.IndexByte(u, '#'); i >= 0 {
		u = u[:i]
	}
	return u
}

// parseVisionJobs extracts the JSON array from the model output, tolerating code
// fences and surrounding prose.
func parseVisionJobs(raw string) ([]visionJob, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(strings.TrimPrefix(s, "json"), "JSON")
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	start, end := strings.Index(s, "["), strings.LastIndex(s, "]")
	if start < 0 || end < start {
		return nil, fmt.Errorf("no JSON array in vision response")
	}
	var jobs []visionJob
	if err := json.Unmarshal([]byte(s[start:end+1]), &jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}
