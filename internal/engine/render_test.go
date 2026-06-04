package engine

import (
	"context"
	"testing"

	"autoapply/internal/config"
	"autoapply/internal/jobs"
)

func TestAttachRendererSkipsAPISources(t *testing.T) {
	e := &Engine{}
	cfg := config.Config{
		Focus: config.JobFocus{Sources: []string{"adzuna", "usajobs"}},
		Sources: config.SourcesConfig{
			Browser: config.BrowserSearchConfig{ScrapeMode: config.ScrapeStealth},
		},
	}
	q := &jobs.Query{}
	if closeFn := e.attachRenderer(context.Background(), cfg, q); closeFn != nil {
		t.Fatal("attachRenderer should skip renderer when only API sources are enabled")
	}
	if q.Render != nil {
		t.Fatal("query renderer should remain nil for API-only selection")
	}
}
