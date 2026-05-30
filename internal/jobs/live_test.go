package jobs

import (
	"context"
	"os"
	"testing"
	"time"

	"autoapply/internal/config"
)

// TestLiveSources exercises the scraping sources against the real endpoints.
// It is skipped unless AUTOAPPLY_LIVE=1, and never fails on a per-source error
// (anti-bot blocks are expected for some boards); it only reports what each
// source returns so the scrapers can be sanity-checked.
func TestLiveSources(t *testing.T) {
	if os.Getenv("AUTOAPPLY_LIVE") == "" {
		t.Skip("set AUTOAPPLY_LIVE=1 to run live network scraping tests")
	}
	focus := config.JobFocus{
		Interest:            "registered nurse",
		Location:            config.Location{City: "Chicago", State: "IL", RadiusMiles: 25},
		IncludeRemote:       true,
		MaxResultsPerSource: 10,
	}
	q := Query{Focus: focus}
	for _, id := range []string{"linkedin", "indeed", "ziprecruiter", "simplyhired", "monster", "craigslist"} {
		src := Registry[id]
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		results, err := src.Search(ctx, q)
		cancel()
		t.Logf("%-12s -> %d jobs, err=%v", id, len(results), err)
		for i, j := range results {
			if i >= 2 {
				break
			}
			t.Logf("    • %q @ %q [%s] %s", j.Title, j.Company, j.Location, j.URL)
		}
	}
}
