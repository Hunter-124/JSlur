package jobs

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"autoapply/internal/store"
)

// remotive searches remotive.com (free, keyless) for remote roles. It only runs
// when IncludeRemote is set, and keeps only roles open to US-based applicants.
// It is category-aware via Remotive's categories endpoint.
type remotive struct{}

func (remotive) ID() string             { return "remotive" }
func (remotive) Name() string           { return "Remotive (US-eligible remote)" }
func (remotive) NeedsCredentials() bool { return false }

func (r remotive) Search(ctx context.Context, q Query) ([]store.Job, error) {
	if !q.Focus.IncludeRemote {
		return nil, nil // remote-only board; nothing to contribute
	}
	max := limit(q.Focus)

	// Fetch the category taxonomy and let the selector pick relevant ones.
	var cats struct {
		Jobs []struct {
			Name string `json:"name"`
			Slug string `json:"slug"`
		} `json:"jobs"`
	}
	var available []Category
	if err := getJSON(ctx, "https://remotive.com/api/remote-jobs/categories", &cats); err == nil {
		for _, c := range cats.Jobs {
			available = append(available, Category{ID: c.Slug, Label: c.Name})
		}
	}
	chosen := chooseCategories(ctx, q, r.ID(), available)

	// Query each chosen category (or a single broad query if none chosen).
	queries := []string{""}
	if len(chosen) > 0 {
		queries = queries[:0]
		for _, c := range chosen {
			queries = append(queries, c.ID)
		}
	}

	out := map[string]store.Job{}
	for _, cat := range queries {
		if len(out) >= max {
			break
		}
		u := fmt.Sprintf("https://remotive.com/api/remote-jobs?limit=%d", max)
		if cat != "" {
			u += "&category=" + url.QueryEscape(cat)
		}
		var resp struct {
			Jobs []struct {
				URL         string   `json:"url"`
				Title       string   `json:"title"`
				CompanyName string   `json:"company_name"`
				Tags        []string `json:"tags"`
				Location    string   `json:"candidate_required_location"`
				Salary      string   `json:"salary"`
				PubDate     string   `json:"publication_date"`
				Description string   `json:"description"`
			} `json:"jobs"`
		}
		if err := getJSON(ctx, u, &resp); err != nil {
			return mapToSlice(out), err
		}
		for _, j := range resp.Jobs {
			if j.URL == "" || !usEligibleRemote(j.Location) {
				continue
			}
			id := store.MakeJobID(r.ID(), j.URL)
			if _, dup := out[id]; dup {
				continue
			}
			loc := j.Location
			if loc == "" {
				loc = "Remote"
			}
			out[id] = store.Job{
				ID:          id,
				Source:      r.ID(),
				Title:       j.Title,
				Company:     j.CompanyName,
				Location:    loc,
				Remote:      true,
				URL:         j.URL,
				Description: stripHTML(j.Description),
				Salary:      j.Salary,
				Tags:        j.Tags,
				PostedAt:    parseTime(j.PubDate),
			}
			if len(out) >= max {
				break
			}
		}
	}
	return mapToSlice(out), nil
}

// usEligibleRemote keeps remote roles a US applicant can take: unrestricted, or
// explicitly including the US / Americas / Worldwide.
func usEligibleRemote(required string) bool {
	l := strings.ToLower(strings.TrimSpace(required))
	if l == "" {
		return true
	}
	for _, ok := range []string{"worldwide", "anywhere", "usa", "u.s.", "united states", "north america", "americas"} {
		if strings.Contains(l, ok) {
			return true
		}
	}
	// "US" as a standalone token.
	for _, tok := range strings.FieldsFunc(l, func(rn rune) bool { return rn == ',' || rn == ' ' || rn == '/' }) {
		if tok == "us" {
			return true
		}
	}
	return false
}
