package jobs

import (
	"testing"

	"autoapply/internal/config"
)

func TestAccountHeaders(t *testing.T) {
	q := Query{Creds: config.SourcesConfig{Accounts: map[string]config.Account{
		"ziprecruiter": {Cookie: "cf_clearance=abc; sess=1", UserAgent: "UA/1.0"},
	}}}
	h := accountHeaders(q, "ziprecruiter")
	if h["Cookie"] != "cf_clearance=abc; sess=1" {
		t.Errorf("cookie = %q", h["Cookie"])
	}
	if h["User-Agent"] != "UA/1.0" {
		t.Errorf("user-agent = %q", h["User-Agent"])
	}
	if !hasAccount(q, "ziprecruiter") {
		t.Error("expected hasAccount(ziprecruiter) = true")
	}
	if accountHeaders(q, "linkedin") != nil {
		t.Error("expected nil headers for an unconnected source")
	}
	if hasAccount(q, "linkedin") {
		t.Error("expected hasAccount(linkedin) = false")
	}
}

func TestMergeHeaders(t *testing.T) {
	m := mergeHeaders(map[string]string{"A": "1", "B": "2"}, map[string]string{"B": "3"})
	if m["A"] != "1" || m["B"] != "3" {
		t.Errorf("merge = %v (later map should win)", m)
	}
	if mergeHeaders(nil, nil) != nil {
		t.Error("expected nil for empty merge")
	}
}

func TestIsConnectable(t *testing.T) {
	if !IsConnectable("linkedin") {
		t.Error("linkedin should be connectable")
	}
	if IsConnectable("indeed") {
		t.Error("indeed should NOT be connectable (keyless mobile API)")
	}
	if IsConnectable("themuse") {
		t.Error("themuse should NOT be connectable")
	}
}
