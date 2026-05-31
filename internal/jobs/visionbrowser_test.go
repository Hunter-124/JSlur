package jobs

import (
	"context"
	"strings"
	"testing"

	"autoapply/internal/browser"
	"autoapply/internal/config"
)

func TestParseVisionJobs(t *testing.T) {
	// Plain array.
	got, err := parseVisionJobs(`[{"title":"RN - ICU","company":"Mercy","location":"Chicago, IL","remote":false,"salary":"$80k","url":"https://x/1","description":"great"}]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Title != "RN - ICU" || got[0].Company != "Mercy" || got[0].Salary != "$80k" {
		t.Fatalf("unexpected parse: %#v", got)
	}

	// Wrapped in a code fence + prose (models often do this despite instructions).
	fenced := "Here you go:\n```json\n[{\"title\":\"Welder\",\"remote\":true}]\n```\nhope that helps"
	got, err = parseVisionJobs(fenced)
	if err != nil {
		t.Fatalf("fenced parse: %v", err)
	}
	if len(got) != 1 || got[0].Title != "Welder" || !got[0].Remote {
		t.Fatalf("fenced unexpected: %#v", got)
	}

	// Empty array is valid (no jobs visible).
	if got, err := parseVisionJobs("[]"); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %#v", err, got)
	}

	// No array at all is an error.
	if _, err := parseVisionJobs("sorry, I cannot see any jobs"); err == nil {
		t.Fatalf("expected error for non-array response")
	}
}

func TestVisionBoardSearchURL(t *testing.T) {
	focus := config.JobFocus{Location: config.Location{City: "Chicago", State: "IL"}}

	in := visionBoards["indeed"].searchURL("mechanical engineer", focus)
	if !strings.HasPrefix(in, "https://www.indeed.com/jobs?") ||
		!strings.Contains(in, "q=mechanical+engineer") || !strings.Contains(in, "Chicago%2C+IL") {
		t.Errorf("indeed url = %q", in)
	}

	li := visionBoards["linkedin"].searchURL("welder", config.JobFocus{})
	if !strings.Contains(li, "keywords=welder") || !strings.Contains(li, "location=United+States") {
		t.Errorf("linkedin url (no location should default to US) = %q", li)
	}

	if !visionBoards["indeed"].isPosting("https://www.indeed.com/viewjob?jk=abc") {
		t.Errorf("indeed isPosting should accept a viewjob link")
	}
	if visionBoards["linkedin"].isPosting("https://www.linkedin.com/company/acme") {
		t.Errorf("linkedin isPosting should reject a non-posting link")
	}
}

func TestVisionBoardAccountGating(t *testing.T) {
	// LinkedIn and ZipRecruiter gate search behind sign-in -> require a connected
	// account; Indeed and Google are public.
	want := map[string]bool{"linkedin": true, "ziprecruiter": true, "indeed": false, "google": false}
	for id, req := range want {
		b, ok := visionBoards[id]
		if !ok {
			t.Fatalf("board %q missing", id)
		}
		if b.requiresAccount != req {
			t.Errorf("board %q requiresAccount = %v, want %v", id, b.requiresAccount, req)
		}
		if req && b.account == "" {
			t.Errorf("board %q requires an account but names no account id", id)
		}
	}
}

func TestLooksLikeWall(t *testing.T) {
	// Cloudflare / captcha interstitials — matched anywhere in title or text.
	if r := looksLikeWall("Just a moment...", "Enable JavaScript and cookies to continue"); r == "" {
		t.Error("expected a Cloudflare bot check to be detected from the title")
	}
	if r := looksLikeWall("Indeed", "Please verify you are a human to continue"); r == "" {
		t.Error("expected a human-verification check to be detected from the body text")
	}
	// Sign-in walls are trusted only from the title (body links would false-match).
	if r := looksLikeWall("Sign Up | LinkedIn", "join linkedin to see jobs"); r == "" {
		t.Error("expected a LinkedIn sign-in wall from the title")
	}
	// A normal results page with a "Sign in" link in the body is NOT a wall.
	if r := looksLikeWall("mechanical engineer jobs in Wilmington, NC | Indeed.com",
		"Sign in to save jobs. 25 jobs found. Mechanical Engineer at Acme..."); r != "" {
		t.Errorf("normal results page flagged as %q", r)
	}
	if r := looksLikeWall("software jobs - Google Search", "About 1,000,000 results"); r != "" {
		t.Errorf("normal search page flagged as %q", r)
	}
}

func TestReadListingsAttachesURLsFromLinks(t *testing.T) {
	// The model returns titles/companies; it picks the matching url from the link
	// list we provide. Verify the conversion to store.Job, including url fallback.
	q := Query{
		Focus: config.JobFocus{},
		Vision: func(ctx context.Context, prompt string, images [][]byte) (string, error) {
			if !strings.Contains(prompt, "Acme Robotics") {
				t.Errorf("prompt missing candidate link text:\n%s", prompt)
			}
			return `[
				{"title":"Mechanical Engineer","company":"Acme Robotics","location":"Chicago, IL","url":"https://www.indeed.com/viewjob?jk=111"},
				{"title":"Design Engineer","company":"NoLink Co","url":""}
			]`, nil
		},
	}
	shots := browser.Shots{
		Images: [][]byte{[]byte("fake-png")},
		Links: []browser.Link{
			{Text: "Mechanical Engineer - Acme Robotics", URL: "https://www.indeed.com/viewjob?jk=111&from=serp"},
			{Text: "About us", URL: "https://www.indeed.com/about"},
		},
	}
	var firstErr error
	jobs := visionBrowser{}.readListings(context.Background(), q, visionBoards["indeed"], shots, 25, &firstErr)
	if firstErr != nil {
		t.Fatalf("unexpected err: %v", firstErr)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2: %#v", len(jobs), jobs)
	}

	var withLink, withoutLink bool
	ids := map[string]bool{}
	for _, j := range jobs {
		if j.Source != "visionbrowser" {
			t.Errorf("source = %q", j.Source)
		}
		if ids[j.ID] {
			t.Errorf("duplicate id %s", j.ID)
		}
		ids[j.ID] = true
		switch j.Title {
		case "Mechanical Engineer":
			withLink = true
			if j.URL != "https://www.indeed.com/viewjob?jk=111" { // jk preserved; only #fragment dropped
				t.Errorf("expected posting url with jk preserved, got %q", j.URL)
			}
		case "Design Engineer":
			withoutLink = true
			if j.URL != "" {
				t.Errorf("expected empty url for unmatched listing, got %q", j.URL)
			}
		}
	}
	if !withLink || !withoutLink {
		t.Errorf("missing expected listings: withLink=%v withoutLink=%v", withLink, withoutLink)
	}
}
