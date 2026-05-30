package jobs

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"autoapply/internal/store"
)

// usajobs searches the federal USAJOBS API. It is US-only with native ZIP/city +
// radius search and a large occupational-series taxonomy (which we pre-filter
// before asking the AI to choose). Free credentials: https://developer.usajobs.gov
type usajobs struct{}

func (usajobs) ID() string             { return "usajobs" }
func (usajobs) Name() string           { return "USAJOBS (US federal)" }
func (usajobs) NeedsCredentials() bool { return true }

func (s usajobs) Search(ctx context.Context, q Query) ([]store.Job, error) {
	c := q.Creds.USAJobs
	if strings.TrimSpace(c.APIKey) == "" || strings.TrimSpace(c.Email) == "" {
		return nil, ErrNotConfigured
	}
	headers := map[string]string{
		"Host":              "data.usajobs.gov",
		"User-Agent":        c.Email,
		"Authorization-Key": c.APIKey,
	}
	max := limit(q.Focus)

	// Occupational-series taxonomy is large; pre-filter to a shortlist before
	// asking the selector so the AI prompt stays small.
	var codelist struct {
		CodeList []struct {
			ValidValue []struct {
				Code  string `json:"Code"`
				Value string `json:"Value"`
			} `json:"ValidValue"`
		} `json:"CodeList"`
	}
	var all []Category
	if err := getJSON(ctx, "https://data.usajobs.gov/api/codelist/occupationalseries", &codelist); err == nil {
		for _, cl := range codelist.CodeList {
			for _, v := range cl.ValidValue {
				if v.Code != "" && v.Value != "" {
					all = append(all, Category{ID: v.Code, Label: v.Value})
				}
			}
		}
	}
	shortlist := HeuristicSelect(q.Focus.Interest, all, 40)
	if len(shortlist) == 0 && len(all) > 40 {
		shortlist = all[:40]
	} else if len(shortlist) == 0 {
		shortlist = all
	}
	chosen := chooseCategories(ctx, q, s.ID(), shortlist)

	var codes []string
	for _, c := range chosen {
		codes = append(codes, c.ID)
	}

	u := fmt.Sprintf("https://data.usajobs.gov/api/search?ResultsPerPage=%d", max)
	if loc := q.Focus.Location.Query(); loc != "" {
		u += "&LocationName=" + url.QueryEscape(loc)
		if q.Focus.Location.RadiusMiles > 0 {
			u += fmt.Sprintf("&Radius=%d", q.Focus.Location.RadiusMiles)
		}
	}
	if len(codes) > 0 {
		u += "&JobCategoryCode=" + url.QueryEscape(strings.Join(codes, ";"))
	}

	var resp struct {
		SearchResult struct {
			Items []struct {
				Descriptor struct {
					PositionTitle    string `json:"PositionTitle"`
					PositionURI      string `json:"PositionURI"`
					OrganizationName string `json:"OrganizationName"`
					LocationDisplay  string `json:"PositionLocationDisplay"`
					Qualification    string `json:"QualificationSummary"`
					PublicationDate  string `json:"PublicationStartDate"`
					Remuneration     []struct {
						Min      string `json:"MinimumRange"`
						Max      string `json:"MaximumRange"`
						Interval string `json:"RateIntervalCode"`
					} `json:"PositionRemuneration"`
					JobCategory []struct {
						Name string `json:"Name"`
					} `json:"JobCategory"`
				} `json:"MatchedObjectDescriptor"`
			} `json:"SearchResultItems"`
		} `json:"SearchResult"`
	}
	if err := getJSONWithHeaders(ctx, u, headers, &resp); err != nil {
		return nil, err
	}

	out := map[string]store.Job{}
	for _, item := range resp.SearchResult.Items {
		d := item.Descriptor
		if d.PositionURI == "" {
			continue
		}
		id := store.MakeJobID(s.ID(), d.PositionURI)
		if _, dup := out[id]; dup {
			continue
		}
		var salaryMin int
		var salary string
		if len(d.Remuneration) > 0 {
			r := d.Remuneration[0]
			minF, _ := strconv.ParseFloat(r.Min, 64)
			maxF, _ := strconv.ParseFloat(r.Max, 64)
			perYear := strings.Contains(strings.ToLower(r.Interval), "year") ||
				strings.EqualFold(r.Interval, "PA")
			if perYear {
				salaryMin = int(minF)
				salary = formatUSD(int(minF), int(maxF))
			} else if minF > 0 {
				salary = fmt.Sprintf("$%.0f–$%.0f / %s", minF, maxF, r.Interval)
			}
		}
		var tags []string
		for _, jc := range d.JobCategory {
			tags = append(tags, jc.Name)
		}
		out[id] = store.Job{
			ID:          id,
			Source:      s.ID(),
			Title:       d.PositionTitle,
			Company:     d.OrganizationName,
			Location:    d.LocationDisplay,
			Remote:      strings.Contains(strings.ToLower(d.LocationDisplay), "remote"),
			URL:         d.PositionURI,
			Description: stripHTML(d.Qualification),
			Salary:      salary,
			SalaryMin:   salaryMin,
			Tags:        tags,
			PostedAt:    parseTime(d.PublicationDate),
		}
	}
	return mapToSlice(out), nil
}
