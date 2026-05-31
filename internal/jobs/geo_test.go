package jobs

import (
	"math"
	"testing"

	"autoapply/internal/config"
	"autoapply/internal/store"
)

// TestHaversineKnownDistances guards against transposed/wrong-sign coordinates
// in the gazetteer by checking a few well-known city-pair distances.
func TestHaversineKnownDistances(t *testing.T) {
	cases := []struct {
		a, b string
		want float64 // straight-line miles, approximate
	}{
		{"chicago, il", "new york, ny", 711},
		{"san francisco, ca", "los angeles, ca", 347},
		{"chicago, il", "milwaukee, wi", 83},
		{"new york, ny", "newark, nj", 9},
		{"dallas, tx", "houston, tx", 225},
		{"seattle, wa", "miami, fl", 2734},
	}
	for _, c := range cases {
		a, ok := cityCoords[c.a]
		if !ok {
			t.Fatalf("missing city %q", c.a)
		}
		b, ok := cityCoords[c.b]
		if !ok {
			t.Fatalf("missing city %q", c.b)
		}
		got := haversineMiles(a[0], a[1], b[0], b[1])
		if math.Abs(got-c.want) > c.want*0.08+10 {
			t.Errorf("distance %s↔%s = %.0f mi, want ~%.0f", c.a, c.b, got, c.want)
		}
	}
}

func TestZipState(t *testing.T) {
	cases := map[string]string{
		"60601": "il", "10001": "ny", "90001": "ca", "77001": "tx",
		"02134": "ma", "98101": "wa", "1": "", "abcde": "",
	}
	for zip, want := range cases {
		if got := zipState(zip); got != want {
			t.Errorf("zipState(%q) = %q, want %q", zip, got, want)
		}
	}
}

func TestWithinSearchArea(t *testing.T) {
	chicago := config.JobFocus{Location: config.Location{City: "Chicago", State: "IL", RadiusMiles: 25}}
	tx := config.JobFocus{Location: config.Location{State: "TX", RadiusMiles: 25}}
	zipOnly := config.JobFocus{Location: config.Location{Zip: "60601", RadiusMiles: 25}}
	noRadius := config.JobFocus{Location: config.Location{City: "Chicago", State: "IL"}} // radius 0 → US-only

	cases := []struct {
		name  string
		focus config.JobFocus
		loc   string
		want  bool
	}{
		{"same city in radius", chicago, "Chicago, IL", true},
		{"far city same state kept (unlistable city)", chicago, "Springfield, IL", true},
		{"different region dropped", chicago, "New York, NY", false},
		{"far west coast dropped", chicago, "Los Angeles, CA", false},
		{"multi-location with a match kept", chicago, "Austin, TX, Chicago, IL", true},
		{"multi-location no match dropped", chicago, "Austin, TX, Boston, MA", false},
		{"state-only home keeps in-state", tx, "Houston, TX", true},
		{"state-only home drops out-of-state", tx, "Denver, CO", false},
		{"zip-only home resolves state", zipOnly, "New York, NY", false},
		{"no radius falls back to US check (far US ok)", noRadius, "Miami, FL", true},
		{"no radius rejects non-US", noRadius, "London, UK", false},
		{"unlocalizable job kept if US", chicago, "United States", true},
	}
	for _, c := range cases {
		if got := withinSearchArea(c.loc, c.focus); got != c.want {
			t.Errorf("%s: withinSearchArea(%q) = %v, want %v", c.name, c.loc, got, c.want)
		}
	}
}

// TestAcceptRemoteRespectsIncludeRemote confirms remote jobs ride on the
// include-remote flag and bypass the distance filter.
func TestAcceptRemoteRespectsIncludeRemote(t *testing.T) {
	job := store.Job{Title: "Engineer", Location: "Remote", Remote: true}
	on := config.JobFocus{IncludeRemote: true, Location: config.Location{City: "Chicago", State: "IL", RadiusMiles: 25}}
	off := config.JobFocus{IncludeRemote: false, Location: config.Location{City: "Chicago", State: "IL", RadiusMiles: 25}}
	if !accept(job, on) {
		t.Error("remote job should be accepted when IncludeRemote is on")
	}
	if accept(job, off) {
		t.Error("remote job should be rejected when IncludeRemote is off")
	}
}
