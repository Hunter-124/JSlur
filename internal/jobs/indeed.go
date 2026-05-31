package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"autoapply/internal/store"
)

// indeed queries Indeed's mobile GraphQL endpoint — the same one the Indeed app
// uses. It needs no per-user key or login; the public app key below is the one
// baked into the (MIT-licensed) JobSpy library. Indeed rotates it occasionally;
// if Indeed starts returning auth errors, update indeedAPIKey.
type indeed struct{}

func (indeed) ID() string             { return "indeed" }
func (indeed) Name() string           { return "Indeed" }
func (indeed) NeedsCredentials() bool { return false }

const (
	indeedEndpoint = "https://apis.indeed.com/graphql"
	// indeedAPIKey is Indeed's public mobile-app key (not a user secret).
	indeedAPIKey = "161092c2017b5bbab13edb12461a62d5a833871e7cad6d9d475304573de67ac8"
)

func (s indeed) Search(ctx context.Context, q Query) ([]store.Job, error) {
	queries := searchQueries(q.Focus)
	loc := q.Focus.Location.Query()
	if len(queries) == 0 {
		if loc == "" && !q.Focus.IncludeRemote {
			return nil, fmt.Errorf("describe your target roles in Job Focus to search Indeed")
		}
		queries = []string{"remote"}
	}
	perRole := limit(q.Focus)
	radius := q.Focus.Location.RadiusMiles
	if radius <= 0 {
		radius = 50
	}

	headers := map[string]string{
		"content-type":    "application/json",
		"indeed-api-key":  indeedAPIKey,
		"accept":          "application/json",
		"indeed-locale":   "en-US",
		"User-Agent":      "Mozilla/5.0 (iPhone; CPU iPhone OS 16_6_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 Indeed App 193.1",
		"indeed-app-info": "appv=193.1; appid=com.indeed.jobsearch; osv=16.6.1; os=ios; dtype=phone",
	}

	out := map[string]store.Job{}
	var firstErr error
	for _, kw := range queries {
		target := len(out) + perRole // per-role budget
		cursor := ""
		for page := 0; page < 5 && len(out) < target; page++ {
			query := indeedQuery(kw, loc, radius, cursor)
			body, _ := json.Marshal(map[string]string{"query": query})
			raw, err := fetch(ctx, http.MethodPost, indeedEndpoint, headers, bytes.NewReader(body))
			if err != nil {
				firstErr = err
				break
			}
			var resp indeedResp
			if err := json.Unmarshal(raw, &resp); err != nil {
				firstErr = err
				break
			}
			if len(resp.Errors) > 0 && len(resp.Data.JobSearch.Results) == 0 {
				firstErr = fmt.Errorf("indeed graphql: %s", resp.Errors[0].Message)
				break
			}
			for _, r := range resp.Data.JobSearch.Results {
				j := r.Job
				link := j.Recruit.ViewJobURL
				if link == "" && j.Key != "" {
					link = "https://www.indeed.com/viewjob?jk=" + j.Key
				}
				if link == "" || j.Title == "" {
					continue
				}
				id := store.MakeJobID(s.ID(), link)
				if _, dup := out[id]; dup {
					continue
				}
				jloc := firstNonEmpty(j.Location.Formatted.Long, j.Location.City)
				var posted time.Time
				if j.DatePublished > 0 {
					posted = time.UnixMilli(j.DatePublished)
				}
				out[id] = store.Job{
					ID:          id,
					Source:      s.ID(),
					Title:       j.Title,
					Company:     j.Employer.Name,
					CompanyURL:  j.Employer.Dossier.Links.CorporateWebsite,
					Location:    jloc,
					Remote:      looksRemote(j.Title, jloc),
					URL:         link,
					Description: stripHTML(j.Description.HTML),
					PostedAt:    posted,
				}
				if len(out) >= target {
					break
				}
			}
			cursor = resp.Data.JobSearch.PageInfo.NextCursor
			if cursor == "" {
				break
			}
		}
	}

	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return mapToSlice(out), nil
}

// indeedQuery builds the GraphQL query. Args are injected as GraphQL literals;
// %q yields a correctly quoted/escaped string for the free-text fields.
func indeedQuery(what, loc string, radius int, cursor string) string {
	var args strings.Builder
	if what != "" {
		fmt.Fprintf(&args, "what: %q ", what)
	}
	if loc != "" {
		fmt.Fprintf(&args, "location: {where: %q, radius: %d, radiusUnit: MILES} ", loc, radius)
	}
	args.WriteString("limit: 100 sort: RELEVANCE ")
	if cursor != "" {
		fmt.Fprintf(&args, "cursor: %q ", cursor)
	}
	return fmt.Sprintf(`query GetJobData {
  jobSearch(%s) {
    pageInfo { nextCursor }
    results {
      job {
        key
        title
        datePublished
        description { html }
        location { formatted { long } city admin1Code countryCode }
        employer { name dossier { links { corporateWebsite } } }
        recruit { viewJobUrl }
      }
    }
  }
}`, args.String())
}

type indeedResp struct {
	Data struct {
		JobSearch struct {
			PageInfo struct {
				NextCursor string `json:"nextCursor"`
			} `json:"pageInfo"`
			Results []struct {
				Job struct {
					Key           string `json:"key"`
					Title         string `json:"title"`
					DatePublished int64  `json:"datePublished"`
					Description   struct {
						HTML string `json:"html"`
					} `json:"description"`
					Location struct {
						Formatted struct {
							Long string `json:"long"`
						} `json:"formatted"`
						City        string `json:"city"`
						Admin1Code  string `json:"admin1Code"`
						CountryCode string `json:"countryCode"`
					} `json:"location"`
					Employer struct {
						Name    string `json:"name"`
						Dossier struct {
							Links struct {
								CorporateWebsite string `json:"corporateWebsite"`
							} `json:"links"`
						} `json:"dossier"`
					} `json:"employer"`
					Recruit struct {
						ViewJobURL string `json:"viewJobUrl"`
					} `json:"recruit"`
				} `json:"job"`
			} `json:"results"`
		} `json:"jobSearch"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}
