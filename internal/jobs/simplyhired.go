package jobs

import (
	"context"
	"fmt"
	"net/url"

	"autoapply/internal/store"
)

// simplyhired scrapes SimplyHired's search results via embedded schema.org
// JobPosting data. SimplyHired aggregates from many boards and is broad (not
// tech-specific). Best-effort, like Monster.
type simplyhired struct{}

func (simplyhired) ID() string             { return "simplyhired" }
func (simplyhired) Name() string           { return "SimplyHired" }
func (simplyhired) NeedsCredentials() bool { return false }

func (s simplyhired) Search(ctx context.Context, q Query) ([]store.Job, error) {
	queries := searchQueries(q.Focus)
	loc := q.Focus.Location.Query()
	if len(queries) == 0 {
		if loc == "" {
			return nil, fmt.Errorf("describe your target roles in Job Focus to search SimplyHired")
		}
		queries = []string{""}
	}
	perRole := limit(q.Focus)
	out := map[string]store.Job{}
	var firstErr error
	for _, kw := range queries {
		u := fmt.Sprintf("https://www.simplyhired.com/search?q=%s&l=%s",
			url.QueryEscape(kw), url.QueryEscape(loc))
		doc, err := q.doc(ctx, u, accountHeaders(q, s.ID()))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, j := range ldJobsToStore(s.ID(), extractJSONLDJobs(doc), perRole) {
			if _, dup := out[j.ID]; !dup {
				out[j.ID] = j
			}
		}
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("no parseable results (SimplyHired page may require JavaScript)")
	}
	return mapToSlice(out), nil
}
