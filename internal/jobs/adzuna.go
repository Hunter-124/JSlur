package jobs

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"autoapply/internal/store"
)

// adzuna searches the Adzuna US job API. It supports true ZIP/city + radius
// search, a category taxonomy, and salary data, which makes it the primary
// location-aware source. Free credentials: https://developer.adzuna.com
type adzuna struct{}

func (adzuna) ID() string             { return "adzuna" }
func (adzuna) Name() string           { return "Adzuna (US, ZIP + radius)" }
func (adzuna) NeedsCredentials() bool { return true }

func (a adzuna) Search(ctx context.Context, q Query) ([]store.Job, error) {
	c := q.Creds.Adzuna
	if strings.TrimSpace(c.AppID) == "" || strings.TrimSpace(c.AppKey) == "" {
		return nil, ErrNotConfigured
	}
	auth := fmt.Sprintf("app_id=%s&app_key=%s", url.QueryEscape(c.AppID), url.QueryEscape(c.AppKey))
	max := limit(q.Focus)

	// Category taxonomy.
	var taxonomy struct {
		Results []struct {
			Tag   string `json:"tag"`
			Label string `json:"label"`
		} `json:"results"`
	}
	var available []Category
	if err := getJSON(ctx, "https://api.adzuna.com/v1/api/jobs/us/categories?"+auth, &taxonomy); err == nil {
		for _, t := range taxonomy.Results {
			available = append(available, Category{ID: t.Tag, Label: t.Label})
		}
	}
	chosen := chooseCategories(ctx, q, a.ID(), available)

	// Common location/salary parameters.
	common := ""
	if where := q.Focus.Location.Query(); where != "" {
		common += "&where=" + url.QueryEscape(where)
	}
	if q.Focus.Location.RadiusMiles > 0 {
		km := int(float64(q.Focus.Location.RadiusMiles)*1.60934 + 0.5)
		common += fmt.Sprintf("&distance=%d", km)
	}
	if q.Focus.MinSalary > 0 {
		common += fmt.Sprintf("&salary_min=%d", q.Focus.MinSalary)
	}

	// One query per chosen category (capped), else a single location-only query.
	cats := []string{""}
	if len(chosen) > 0 {
		cats = cats[:0]
		for i, c := range chosen {
			if i >= 3 {
				break
			}
			cats = append(cats, c.ID)
		}
	}

	out := map[string]store.Job{}
	for _, cat := range cats {
		if len(out) >= max {
			break
		}
		u := fmt.Sprintf("https://api.adzuna.com/v1/api/jobs/us/search/1?%s&results_per_page=%d&content-type=application/json%s",
			auth, max, common)
		if cat != "" {
			u += "&category=" + url.QueryEscape(cat)
		}
		var resp struct {
			Results []struct {
				ID          string  `json:"id"`
				Title       string  `json:"title"`
				Description string  `json:"description"`
				RedirectURL string  `json:"redirect_url"`
				Created     string  `json:"created"`
				SalaryMin   float64 `json:"salary_min"`
				SalaryMax   float64 `json:"salary_max"`
				Company     struct {
					DisplayName string `json:"display_name"`
				} `json:"company"`
				Location struct {
					DisplayName string `json:"display_name"`
				} `json:"location"`
				Category struct {
					Label string `json:"label"`
				} `json:"category"`
			} `json:"results"`
		}
		if err := getJSON(ctx, u, &resp); err != nil {
			return mapToSlice(out), err
		}
		for _, j := range resp.Results {
			if j.RedirectURL == "" {
				continue
			}
			id := store.MakeJobID(a.ID(), j.RedirectURL)
			if _, dup := out[id]; dup {
				continue
			}
			out[id] = store.Job{
				ID:          id,
				Source:      a.ID(),
				Title:       j.Title,
				Company:     j.Company.DisplayName,
				Location:    j.Location.DisplayName,
				Remote:      strings.Contains(strings.ToLower(j.Title+" "+j.Location.DisplayName), "remote"),
				URL:         j.RedirectURL,
				Description: stripHTML(j.Description),
				Salary:      formatUSD(int(j.SalaryMin), int(j.SalaryMax)),
				SalaryMin:   int(j.SalaryMin),
				Tags:        []string{j.Category.Label},
				PostedAt:    parseTime(j.Created),
			}
			if len(out) >= max {
				break
			}
		}
	}
	return mapToSlice(out), nil
}

// formatUSD renders a salary range for display.
func formatUSD(min, max int) string {
	switch {
	case min > 0 && max > 0:
		return fmt.Sprintf("$%s–$%s", commas(min), commas(max))
	case min > 0:
		return fmt.Sprintf("$%s+", commas(min))
	case max > 0:
		return fmt.Sprintf("up to $%s", commas(max))
	default:
		return ""
	}
}

// commas formats an integer with thousands separators.
func commas(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		if len(s) > pre {
			b.WriteString(",")
		}
	}
	for i := pre; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteString(",")
		}
	}
	return b.String()
}
