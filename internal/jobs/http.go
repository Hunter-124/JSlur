package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 30 * time.Second}

// getJSON performs a GET and decodes the JSON body into out.
func getJSON(ctx context.Context, url string, out any) error {
	return getJSONWithHeaders(ctx, url, nil, out)
}

// getJSONWithHeaders performs a GET with extra request headers (used by APIs
// such as USAJOBS that require an Authorization-Key and email User-Agent).
func getJSONWithHeaders(ctx context.Context, url string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "autoapply/1.0 (+https://localhost)")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %d from %s", resp.StatusCode, url)
	}
	return json.Unmarshal(data, out)
}

var (
	tagRe   = regexp.MustCompile(`(?s)<[^>]*>`)
	wsRe    = regexp.MustCompile(`[ \t]+`)
	nlRe    = regexp.MustCompile(`\n{3,}`)
	entRepl = strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`,
		"&#39;", "'", "&apos;", "'", "&nbsp;", " ",
	)
)

// parseTime tries several common board date formats, returning the zero time
// if none match.
func parseTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, value); err == nil {
			return t
		}
	}
	return time.Time{}
}

// stripHTML converts an HTML job description to readable plain text. It is
// intentionally lightweight — good enough to feed an LLM, not a full parser.
func stripHTML(s string) string {
	s = strings.ReplaceAll(s, "</p>", "\n\n")
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "<br />", "\n")
	s = strings.ReplaceAll(s, "</li>", "\n")
	s = strings.ReplaceAll(s, "<li>", "• ")
	s = tagRe.ReplaceAllString(s, "")
	s = entRepl.Replace(s)
	s = wsRe.ReplaceAllString(s, " ")
	s = nlRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
