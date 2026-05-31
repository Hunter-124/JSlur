package jobs

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"

	"autoapply/internal/store"
)

// craigslist scrapes a metro's Craigslist jobs section via its public RSS feed.
// Craigslist is partitioned by metro, so it only runs when the focus location
// maps to a known subdomain. It is a good source of local, non-software roles
// (trades, hospitality, admin, healthcare support, …). Listings are sparse
// (often no company name), which the AI + apply resolver help fill in.
type craigslist struct{}

func (craigslist) ID() string             { return "craigslist" }
func (craigslist) Name() string           { return "Craigslist" }
func (craigslist) NeedsCredentials() bool { return false }

func (c craigslist) Search(ctx context.Context, q Query) ([]store.Job, error) {
	region := craigslistRegion(q.Focus.Location)
	if region == "" {
		return nil, fmt.Errorf("set a recognised US city/state in Job Focus to search Craigslist")
	}
	perRole := limit(q.Focus)
	queries := searchQueries(q.Focus)
	if len(queries) == 0 {
		queries = []string{""} // all jobs in the metro
	}

	// A US-recognisable location so the aggregator's US filter keeps these.
	loc := strings.TrimSpace(q.Focus.Location.Query())
	if !isUSLocation(loc) {
		if loc == "" {
			loc = region
		}
		loc += ", US"
	}

	out := map[string]store.Job{}
	var firstErr error
	for _, kw := range queries {
		u := fmt.Sprintf("https://%s.craigslist.org/search/jjj?format=rss", region)
		if kw != "" {
			u += "&query=" + url.QueryEscape(kw)
		}
		doc, err := getDoc(ctx, u, accountHeaders(q, c.ID()))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		var feed struct {
			Items []struct {
				Title string `xml:"title"`
				Link  string `xml:"link"`
				About string `xml:"about,attr"`
				Desc  string `xml:"description"`
				Date  string `xml:"date"`
			} `xml:"item"`
		}
		if err := xml.Unmarshal([]byte(doc), &feed); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("parse craigslist rss: %w", err)
			}
			continue
		}
		count := 0
		for _, it := range feed.Items {
			link := strings.TrimSpace(it.Link)
			if link == "" {
				link = strings.TrimSpace(it.About)
			}
			title := htmlText(it.Title)
			if link == "" || title == "" {
				continue
			}
			id := store.MakeJobID(c.ID(), link)
			if _, dup := out[id]; dup {
				continue
			}
			out[id] = store.Job{
				ID:          id,
				Source:      c.ID(),
				Title:       title,
				Location:    loc,
				Remote:      looksRemote(title),
				URL:         link,
				Description: htmlText(it.Desc),
				PostedAt:    parseTime(it.Date),
			}
			if count++; count >= perRole {
				break
			}
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return mapToSlice(out), nil
}
