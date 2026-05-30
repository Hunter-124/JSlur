package jobs

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"autoapply/internal/config"
	"autoapply/internal/store"
)

// themuse searches themuse.com's free, keyless public API. It spans every field
// (healthcare, accounting, engineering, education, legal, ...) and filters by
// category server-side, so it's the keyless default that works out of the box.
// It has no radius support, so location filtering is by metro name (best effort).
type themuse struct{}

func (themuse) ID() string             { return "themuse" }
func (themuse) Name() string           { return "The Muse (all US fields)" }
func (themuse) NeedsCredentials() bool { return false }

func (m themuse) Search(ctx context.Context, q Query) ([]store.Job, error) {
	max := limit(q.Focus)
	chosen := chooseCategories(ctx, q, m.ID(), museCategories)

	var params strings.Builder
	for _, c := range chosen {
		params.WriteString("&category=" + url.QueryEscape(c.ID))
	}
	// Best-effort location: The Muse matches metro names, not ZIPs/radius.
	if loc := metroName(q.Focus.Location); loc != "" {
		params.WriteString("&location=" + url.QueryEscape(loc))
	}
	if q.Focus.IncludeRemote {
		params.WriteString("&location=" + url.QueryEscape("Flexible / Remote"))
	}

	out := map[string]store.Job{}
	for page := 0; page < 6 && len(out) < max; page++ {
		u := fmt.Sprintf("https://www.themuse.com/api/public/jobs?page=%d&descending=true%s", page, params.String())
		var resp struct {
			Results []struct {
				Name            string `json:"name"`
				Contents        string `json:"contents"`
				PublicationDate string `json:"publication_date"`
				Locations       []struct {
					Name string `json:"name"`
				} `json:"locations"`
				Categories []struct {
					Name string `json:"name"`
				} `json:"categories"`
				Company struct {
					Name string `json:"name"`
				} `json:"company"`
				Refs struct {
					LandingPage string `json:"landing_page"`
				} `json:"refs"`
			} `json:"results"`
		}
		if err := getJSON(ctx, u, &resp); err != nil {
			return mapToSlice(out), err
		}
		if len(resp.Results) == 0 {
			break
		}
		for _, j := range resp.Results {
			link := j.Refs.LandingPage
			if link == "" {
				continue
			}
			id := store.MakeJobID(m.ID(), link)
			if _, dup := out[id]; dup {
				continue
			}
			var locNames []string
			remote := false
			for _, l := range j.Locations {
				locNames = append(locNames, l.Name)
				ll := strings.ToLower(l.Name)
				if strings.Contains(ll, "remote") || strings.Contains(ll, "flexible") {
					remote = true
				}
			}
			var tags []string
			for _, c := range j.Categories {
				tags = append(tags, c.Name)
			}
			out[id] = store.Job{
				ID:          id,
				Source:      m.ID(),
				Title:       j.Name,
				Company:     j.Company.Name,
				Location:    strings.Join(locNames, ", "),
				Remote:      remote,
				URL:         link,
				Description: stripHTML(j.Contents),
				Tags:        tags,
				PostedAt:    parseTime(j.PublicationDate),
			}
			if len(out) >= max {
				break
			}
		}
	}
	return mapToSlice(out), nil
}

// metroName builds a "City, State" string for The Muse from the focus location.
// The Muse can't use a bare ZIP, so ZIP-only locations yield no location filter.
func metroName(l config.Location) string {
	city, state := strings.TrimSpace(l.City), strings.TrimSpace(l.State)
	switch {
	case city != "" && state != "":
		return city + ", " + state
	case city != "":
		return city
	default:
		return ""
	}
}
