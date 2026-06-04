package jobs

import (
	"context"
	"strings"
	"testing"

	"autoapply/internal/config"
)

func TestHandshakeWiring(t *testing.T) {
	// Registered, connectable, and present as a vision board (it's a gated SPA, so
	// it follows the LinkedIn/ZipRecruiter dual-presence pattern).
	if _, ok := Registry["handshake"]; !ok {
		t.Fatal("handshake not in Registry")
	}
	if !IsConnectable("handshake") {
		t.Error("handshake should be connectable (it's behind a school sign-in)")
	}
	b, ok := visionBoards["handshake"]
	if !ok {
		t.Fatal("handshake missing from vision boards")
	}
	if !b.requiresAccount || b.account != "handshake" {
		t.Errorf("handshake vision board should require its own account, got requiresAccount=%v account=%q", b.requiresAccount, b.account)
	}

	// Keyword search URL.
	if got := handshakeSearchURL("mechanical engineer"); got != "https://app.joinhandshake.com/job-search?query=mechanical+engineer" {
		t.Errorf("handshakeSearchURL = %q", got)
	}
	if got := handshakeSearchURL(""); got != "https://app.joinhandshake.com/job-search" {
		t.Errorf("empty-keyword url = %q", got)
	}

	// Without a connected account it must refuse clearly rather than fetch a wall.
	_, err := handshake{}.Search(context.Background(), Query{Focus: config.JobFocus{Interest: "engineer"}})
	if err == nil || !strings.Contains(err.Error(), "connect your Handshake account") {
		t.Errorf("expected a connect-account error, got %v", err)
	}
}

func TestAvailableSourcesAPIVsScrapeClassification(t *testing.T) {
	infos := AvailableSources()
	if len(infos) == 0 {
		t.Fatal("AvailableSources returned no sources")
	}
	for _, info := range infos {
		wantAPI := info.ID == "adzuna" || info.ID == "usajobs"
		if info.OfficialAPI != wantAPI {
			t.Errorf("%s: OfficialAPI = %v, want %v", info.ID, info.OfficialAPI, wantAPI)
		}
		if info.Scrape == info.OfficialAPI {
			t.Errorf("%s: expected Scrape to be inverse of OfficialAPI (scrape=%v api=%v)", info.ID, info.Scrape, info.OfficialAPI)
		}
	}
}

func TestSourceUsesScrapeMode(t *testing.T) {
	creds := config.SourcesConfig{}
	if SourceUsesScrapeMode("adzuna", creds) {
		t.Error("adzuna should not use scrape mode")
	}
	if SourceUsesScrapeMode("usajobs", creds) {
		t.Error("usajobs should not use scrape mode")
	}
	if !SourceUsesScrapeMode("simplyhired", creds) {
		t.Error("simplyhired should use scrape mode")
	}
	if !SourceUsesScrapeMode("monster", creds) {
		t.Error("monster should use scrape mode")
	}
	if !SourceUsesScrapeMode("handshake", creds) {
		t.Error("handshake should use scrape mode")
	}
	if SourceUsesScrapeMode("ziprecruiter", creds) {
		t.Error("ziprecruiter should not use scrape mode without a connected account")
	}

	creds.Accounts = map[string]config.Account{
		"ziprecruiter": {Cookie: "cf_clearance=ok"},
	}
	if !SourceUsesScrapeMode("ziprecruiter", creds) {
		t.Error("ziprecruiter should use scrape mode with a connected account")
	}
}
