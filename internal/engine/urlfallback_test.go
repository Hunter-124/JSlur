package engine

import (
	"encoding/base64"
	"testing"

	"autoapply/internal/jobs"
)

func TestNeedsVisionFallback(t *testing.T) {
	cases := []struct {
		name string
		in   jobs.Official
		want bool
	}{
		{"no url at all", jobs.Official{}, true},
		{"bare company site", jobs.Official{ApplyURL: "https://acme.com", CompanyURL: "https://acme.com", Note: "company website — couldn't find a specific apply page; check the Careers section"}, true},
		{"real ATS link", jobs.Official{ApplyURL: "https://boards.greenhouse.io/acme/jobs/1", ATS: "Greenhouse", Note: "found in listing"}, false},
		{"careers page found on site", jobs.Official{ApplyURL: "https://acme.com/careers", CompanyURL: "https://acme.com", Note: "found on company site"}, false},
	}
	for _, c := range cases {
		if got := needsVisionFallback(c.in); got != c.want {
			t.Errorf("%s: needsVisionFallback = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseURLReply(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"url":"https://boards.greenhouse.io/acme"}`, "https://boards.greenhouse.io/acme"},
		{"```json\n{\"url\": \"https://acme.com/careers\"}\n```", "https://acme.com/careers"},
		{`Sure! {"url":"https://acme.com/jobs"} hope that helps`, "https://acme.com/jobs"},
		{`{"url":""}`, ""},
		// No JSON object: fall back to the first bare URL, trailing punctuation trimmed.
		{"The careers page is https://acme.com/careers.", "https://acme.com/careers"},
		{"I couldn't find one.", ""},
	}
	for _, c := range cases {
		if got := parseURLReply(c.in); got != c.want {
			t.Errorf("parseURLReply(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUnwrapResultURL(t *testing.T) {
	real := "https://jobs.acme.com/careers?id=5"

	// Bing /ck/a wraps the destination as base64url in the u= param, prefixed "a1".
	bing := "https://www.bing.com/ck/a?!&&p=abc&u=a1" + base64.RawURLEncoding.EncodeToString([]byte(real))
	if got := unwrapResultURL(bing); got != real {
		t.Errorf("bing unwrap = %q, want %q", got, real)
	}

	// DuckDuckGo redirect carries the URL-encoded destination in uddg=.
	ddg := "https://duckduckgo.com/l/?uddg=https%3A%2F%2Fjobs.acme.com%2Fcareers%3Fid%3D5&rut=x"
	if got := unwrapResultURL(ddg); got != real {
		t.Errorf("ddg unwrap = %q, want %q", got, real)
	}

	// Google /url?q= redirect.
	goog := "https://www.google.com/url?q=https%3A%2F%2Fjobs.acme.com%2Fcareers%3Fid%3D5&sa=U"
	if got := unwrapResultURL(goog); got != real {
		t.Errorf("google unwrap = %q, want %q", got, real)
	}

	// A direct (non-redirect) URL is returned unchanged.
	if got := unwrapResultURL(real); got != real {
		t.Errorf("direct url changed: %q", got)
	}
}
