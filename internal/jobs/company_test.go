package jobs

import (
	"context"
	"testing"

	"autoapply/internal/store"
)

func TestDetectATS(t *testing.T) {
	cases := map[string]string{
		"https://boards.greenhouse.io/acme/jobs/123":               "Greenhouse",
		"https://jobs.lever.co/acme/abc-def":                       "Lever",
		"https://acme.wd1.myworkdayjobs.com/en-US/careers/job/123": "Workday",
		"https://jobs.ashbyhq.com/acme/abc":                        "Ashby",
		"https://acme.com/careers":                                 "",
		"https://www.linkedin.com/jobs/view/123":                   "",
	}
	for u, want := range cases {
		if got := detectATS(u); got != want {
			t.Errorf("detectATS(%q) = %q, want %q", u, got, want)
		}
	}
}

func TestIsAggregator(t *testing.T) {
	agg := []string{
		"https://www.linkedin.com/jobs/view/1",
		"https://indeed.com/viewjob?jk=1",
		"https://www.ziprecruiter.com/jobs/x",
		"https://x.com/foo",
		"https://lnkd.in/abc",
	}
	for _, u := range agg {
		if !isAggregator(u) {
			t.Errorf("expected aggregator: %s", u)
		}
	}
	notAgg := []string{
		"https://boards.greenhouse.io/acme/jobs/1",
		"https://acme.com/careers",
		"https://careers.acme.org/job/2",
	}
	for _, u := range notAgg {
		if isAggregator(u) {
			t.Errorf("expected NOT aggregator: %s", u)
		}
	}
}

func TestResolveOfficial_FromDescriptionLink(t *testing.T) {
	job := store.Job{
		Source:      "linkedin",
		URL:         "https://www.linkedin.com/jobs/view/999",
		Company:     "Acme",
		Description: "Great role. Apply at https://boards.greenhouse.io/acme/jobs/123 today!",
	}
	res := ResolveOfficial(context.Background(), job, nil, false)
	if res.ApplyURL != "https://boards.greenhouse.io/acme/jobs/123" {
		t.Fatalf("ApplyURL = %q", res.ApplyURL)
	}
	if res.ATS != "Greenhouse" {
		t.Errorf("ATS = %q", res.ATS)
	}
}

func TestResolveOfficial_BoardURLIsOfficialWhenATS(t *testing.T) {
	job := store.Job{Source: "themuse", URL: "https://jobs.lever.co/acme/xyz", Company: "Acme"}
	res := ResolveOfficial(context.Background(), job, nil, false)
	if res.ApplyURL != job.URL {
		t.Fatalf("ApplyURL = %q, want the lever URL", res.ApplyURL)
	}
}

func TestResolveOfficial_AggregatorOnly_NoLink(t *testing.T) {
	job := store.Job{
		Source:      "indeed",
		URL:         "https://www.indeed.com/viewjob?jk=1",
		Company:     "Acme",
		Description: "no links here",
	}
	res := ResolveOfficial(context.Background(), job, nil, false)
	if res.ApplyURL != "" {
		t.Errorf("expected empty ApplyURL, got %q", res.ApplyURL)
	}
}

func TestResolveOfficial_AIPicker(t *testing.T) {
	job := store.Job{Source: "indeed", URL: "https://www.indeed.com/viewjob?jk=1", Company: "Acme", Description: "apply on our site"}
	pick := func(ctx context.Context, j store.Job, cands []string) (string, error) {
		return "https://acme.com/careers/123", nil
	}
	res := ResolveOfficial(context.Background(), job, pick, false)
	if res.ApplyURL != "https://acme.com/careers/123" {
		t.Fatalf("ApplyURL = %q", res.ApplyURL)
	}
	if res.Note != "chosen by AI" {
		t.Errorf("Note = %q", res.Note)
	}
}

func TestResolveOfficial_AIPickerRejectsAggregator(t *testing.T) {
	job := store.Job{Source: "indeed", URL: "https://www.indeed.com/viewjob?jk=1", Company: "Acme"}
	pick := func(ctx context.Context, j store.Job, cands []string) (string, error) {
		return "https://www.linkedin.com/jobs/view/1", nil // aggregator — must be ignored
	}
	res := ResolveOfficial(context.Background(), job, pick, false)
	if res.ApplyURL != "" {
		t.Errorf("expected empty ApplyURL (AI returned an aggregator), got %q", res.ApplyURL)
	}
}

// TestResolveOfficial_FallsBackToCompanySite: when no specific apply page is
// found but the company's own site is known, the user should still get a link
// to it rather than a dead end.
func TestResolveOfficial_FallsBackToCompanySite(t *testing.T) {
	job := store.Job{
		Source:     "indeed",
		URL:        "https://www.indeed.com/viewjob?jk=1",
		Company:    "Acme",
		CompanyURL: "https://acme.com",
	}
	res := ResolveOfficial(context.Background(), job, nil, false)
	if res.ApplyURL != "https://acme.com" {
		t.Fatalf("ApplyURL = %q, want the company site", res.ApplyURL)
	}
	if res.CompanyURL != "https://acme.com" {
		t.Errorf("CompanyURL = %q", res.CompanyURL)
	}
}

func TestCompanySlug(t *testing.T) {
	cases := map[string]string{
		"Acme Inc.":            "acme",
		"Foo Bar LLC":          "foobar",
		"AT&T":                 "att",
		"General Motors":       "generalmotors",
		"Stripe":               "stripe",
		"Initech Technologies": "initech",
	}
	for in, want := range cases {
		if got := companySlug(in); got != want {
			t.Errorf("companySlug(%q) = %q, want %q", in, got, want)
		}
	}
}
