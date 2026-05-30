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

func TestCraigslistRegion(t *testing.T) {
	cases := []struct {
		loc  config.Location
		want string
	}{
		{config.Location{City: "Chicago"}, "chicago"},
		{config.Location{City: "san francisco"}, "sfbay"},
		{config.Location{State: "WA"}, "seattle"},
		{config.Location{City: "Nowheresville"}, ""},
		{config.Location{}, ""},
	}
	for _, c := range cases {
		if got := craigslistRegion(c.loc); got != c.want {
			t.Errorf("craigslistRegion(%+v) = %q, want %q", c.loc, got, c.want)
		}
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
