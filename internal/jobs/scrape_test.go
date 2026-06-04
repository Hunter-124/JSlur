package jobs

import (
	"strings"
	"testing"

	"autoapply/internal/config"
	"autoapply/internal/store"
)

func TestExtractJSONLDJobs(t *testing.T) {
	html := `<html><head>
	<script type="application/ld+json">
	{"@context":"https://schema.org","@type":"JobPosting","title":"RN - ICU",
	 "description":"<p>Great <b>nursing</b> role</p>",
	 "datePosted":"2026-05-01",
	 "hiringOrganization":{"@type":"Organization","name":"Mercy Hospital","sameAs":"https://mercy.example"},
	 "jobLocation":{"@type":"Place","address":{"@type":"PostalAddress","addressLocality":"Chicago","addressRegion":"IL","addressCountry":"US"}},
	 "url":"https://mercy.example/careers/rn-icu"}
	</script></head><body></body></html>`

	jobs := extractJSONLDJobs(html)
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	j := jobs[0]
	if j.Title != "RN - ICU" {
		t.Errorf("title = %q", j.Title)
	}
	if j.Company != "Mercy Hospital" {
		t.Errorf("company = %q", j.Company)
	}
	if j.CompanyURL != "https://mercy.example" {
		t.Errorf("companyURL = %q", j.CompanyURL)
	}
	if j.Location != "Chicago, IL" {
		t.Errorf("location = %q", j.Location)
	}
	if j.URL != "https://mercy.example/careers/rn-icu" {
		t.Errorf("url = %q", j.URL)
	}
	if !strings.Contains(j.Description, "nursing") {
		t.Errorf("description missing text: %q", j.Description)
	}
}

func TestExtractJSONLDJobs_GraphAndRemote(t *testing.T) {
	html := `<script type="application/ld+json">
	{"@graph":[{"@type":"WebSite"},
	{"@type":"JobPosting","title":"Welder","hiringOrganization":"Acme Fab",
	 "url":"https://acme.example/jobs/welder","jobLocationType":"TELECOMMUTE"}]}</script>`
	jobs := extractJSONLDJobs(html)
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].Company != "Acme Fab" {
		t.Errorf("company = %q", jobs[0].Company)
	}
	if !jobs[0].Remote {
		t.Errorf("expected remote=true for TELECOMMUTE")
	}
}

func TestLinkedInParsePage(t *testing.T) {
	doc := `<ul>
	<li><div class="base-card">
	  <a class="base-card__full-link" href="https://www.linkedin.com/jobs/view/123?trk=abc">x</a>
	  <h3 class="base-search-card__title">Registered Nurse</h3>
	  <h4 class="base-search-card__subtitle"><a class="hidden-nested-link">Mercy Hospital</a></h4>
	  <span class="job-search-card__location">Chicago, IL</span>
	  <time class="job-search-card__listdate" datetime="2026-05-02">2 days</time>
	</div></li>
	<li><div class="base-card">
	  <a class="base-card__full-link" href="https://www.linkedin.com/jobs/view/456?x=y">y</a>
	  <h3 class="base-search-card__title">Remote Welder</h3>
	  <h4 class="base-search-card__subtitle">Acme</h4>
	  <span class="job-search-card__location">Remote</span>
	</div></li>
	</ul>`

	out := map[string]store.Job{}
	n := linkedin{}.parsePage(doc, out, 25)
	if n != 2 {
		t.Fatalf("parsed %d cards, want 2", n)
	}
	if len(out) != 2 {
		t.Fatalf("built %d jobs, want 2", len(out))
	}

	var nurse, welder store.Job
	for _, j := range out {
		switch j.Title {
		case "Registered Nurse":
			nurse = j
		case "Remote Welder":
			welder = j
		}
	}
	if nurse.Company != "Mercy Hospital" {
		t.Errorf("nurse company = %q", nurse.Company)
	}
	if nurse.URL != "https://www.linkedin.com/jobs/view/123" {
		t.Errorf("nurse url = %q (query not stripped?)", nurse.URL)
	}
	if nurse.Location != "Chicago, IL" {
		t.Errorf("nurse location = %q", nurse.Location)
	}
	if welder.Company != "Acme" {
		t.Errorf("welder company = %q", welder.Company)
	}
	if !welder.Remote {
		t.Errorf("welder should be remote (location 'Remote')")
	}
}

func TestLinkedinLocation(t *testing.T) {
	cases := []struct {
		name string
		loc  config.Location
		want string
	}{
		// The Spain bug: a bare ZIP must NOT be sent — LinkedIn geocodes it globally
		// and 28405 (Wilmington, NC) resolves to the Madrid, Spain region. We send
		// the city + state derived from the ZIP instead.
		{"city+state+zip", config.Location{City: "Wilmington", State: "NC", Zip: "28405"}, "Wilmington, NC"},
		{"zip only derives state", config.Location{Zip: "28405"}, "NC"},
		{"city+zip no state", config.Location{City: "Wilmington", Zip: "28405"}, "Wilmington, NC"},
		{"city+state", config.Location{City: "Chicago", State: "IL"}, "Chicago, IL"},
		{"state only", config.Location{State: "TX"}, "TX"},
		{"city only", config.Location{City: "Austin"}, "Austin"},
		{"nothing usable", config.Location{}, ""},
		{"unknown zip", config.Location{Zip: "00000"}, ""},
	}
	for _, c := range cases {
		got := linkedinLocation(c.loc)
		if got != c.want {
			t.Errorf("%s: linkedinLocation(%+v) = %q, want %q", c.name, c.loc, got, c.want)
		}
		// Hard invariant: never a bare 5-digit ZIP (that's what searched Spain).
		if got != "" && got == strings.TrimSpace(c.loc.Zip) {
			t.Errorf("%s: returned a bare ZIP %q — LinkedIn reads it as a foreign postal code", c.name, got)
		}
	}
}

func TestCraigslistRegion(t *testing.T) {
	cases := []struct {
		loc  config.Location
		want string
	}{
		{config.Location{City: "Chicago"}, "chicago"},
		{config.Location{City: "san francisco"}, "sfbay"},
		{config.Location{State: "WA"}, "seattle"},
		// Wilmington has its own subdomain; without it the NC fallback was Charlotte.
		{config.Location{City: "Wilmington", State: "NC"}, "wilmington"},
		{config.Location{City: "Nowheresville"}, ""},
		{config.Location{}, ""},
	}
	for _, c := range cases {
		if got := craigslistRegion(c.loc); got != c.want {
			t.Errorf("craigslistRegion(%+v) = %q, want %q", c.loc, got, c.want)
		}
	}
}

func TestExtractSimplyHiredJobs(t *testing.T) {
	// Mirrors SimplyHired's rendered page: a Next.js state blob with a "jobs" array
	// (no schema.org JSON-LD). Brackets inside string values must not break parsing.
	doc := `<html><body><script id="__NEXT_DATA__" type="application/json">
	{"props":{"pageProps":{"searchKey":"abc","jobs":[
	  {"jobKey":"K1","title":"Lead Mechanical Engineer","company":"GE Vernova","location":"Wilmington, NC","snippet":"Design [systems] and reports.","salaryInfo":"$89,300 - $150,000 a year","botUrl":"/job/K1"},
	  {"jobKey":"K2","title":"Remote Design Engineer","company":"Acme","location":"United States","snippet":"Work from home.","salaryInfo":"","botUrl":"/job/K2"},
	  {"jobKey":"","title":"","company":"","location":"","snippet":"","salaryInfo":"","botUrl":""}
	],"relatedSearches":{"jobs":null}}}}</script></body></html>`

	shs := extractSimplyHiredJobs(doc)
	if len(shs) != 2 {
		t.Fatalf("extracted %d listings, want 2 (the empty one dropped): %#v", len(shs), shs)
	}
	jobs := shJobsToStore("simplyhired", shs, 25)
	if len(jobs) != 2 {
		t.Fatalf("converted %d jobs, want 2", len(jobs))
	}
	var lead, remote store.Job
	for _, j := range jobs {
		switch j.Title {
		case "Lead Mechanical Engineer":
			lead = j
		case "Remote Design Engineer":
			remote = j
		}
	}
	if lead.Company != "GE Vernova" || lead.Location != "Wilmington, NC" {
		t.Errorf("lead job = %+v", lead)
	}
	if lead.Salary != "$89,300 - $150,000 a year" {
		t.Errorf("lead salary = %q", lead.Salary)
	}
	if lead.URL != "https://www.simplyhired.com/job/K1" {
		t.Errorf("lead url = %q (botUrl not absolutized?)", lead.URL)
	}
	if !strings.Contains(lead.Description, "[systems]") {
		t.Errorf("lead description lost bracketed text: %q", lead.Description)
	}
	if !remote.Remote {
		t.Errorf("remote job not flagged remote: %+v", remote)
	}
}

func TestJSONArrayAt(t *testing.T) {
	// Balanced scan stops at the matching ']' and ignores brackets inside strings.
	if got := jsonArrayAt(`[1,[2,3],"a]b"]tail`); got != `[1,[2,3],"a]b"]` {
		t.Errorf("jsonArrayAt = %q", got)
	}
	if got := jsonArrayAt(`{"not":"array"}`); got != "" {
		t.Errorf("non-array should yield empty, got %q", got)
	}
	if got := jsonArrayAt(`[unterminated`); got != "" {
		t.Errorf("unbalanced should yield empty, got %q", got)
	}
}

func TestSearchQueries(t *testing.T) {
	q := func(s string) []string { return searchQueries(config.JobFocus{Interest: s}) }

	got := q("manufacturing / mechanical engineering")
	if len(got) != 2 || got[0] != "manufacturing" || got[1] != "mechanical engineering" {
		t.Fatalf("slash split = %#v", got)
	}
	if g := q("Accounting, Finance"); len(g) != 1 {
		t.Errorf("commas should NOT split: %#v", g)
	}
	if g := q("nurse\nRN; ICU"); len(g) != 3 {
		t.Errorf("newline/semicolon split = %#v", g)
	}
	if g := q("dev / dev"); len(g) != 1 {
		t.Errorf("duplicates should be removed: %#v", g)
	}
	if g := q("   "); g != nil {
		t.Errorf("blank interest should be nil: %#v", g)
	}
}

func TestLooksRemote(t *testing.T) {
	if !looksRemote("Senior Engineer", "Remote - US") {
		t.Error("expected remote")
	}
	if looksRemote("Welder", "Chicago, IL") {
		t.Error("expected not remote")
	}
}
