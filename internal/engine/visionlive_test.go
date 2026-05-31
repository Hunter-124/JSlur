package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autoapply/internal/ai"
	"autoapply/internal/config"
	"autoapply/internal/store"
)

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// liveEngine builds an Engine seeded from the user's real saved config (so the
// search uses their actual job focus and any connected accounts) but with the AI
// pointed at the local vision model and writing to throwaway stores so nothing
// persistent is touched. It also streams the engine log to the test output.
func liveEngine(t *testing.T) (*Engine, func()) {
	t.Helper()
	base := envOr("AUTOAPPLY_AI_BASEURL", "http://localhost:1234/v1")
	model := envOr("AUTOAPPLY_AI_MODEL", "google/gemma-4-26b-a4b")

	// Seed from the real config if present (focus, connected accounts), else default.
	seed := config.Default()
	if appData := os.Getenv("APPDATA"); appData != "" {
		if real, err := config.NewStore(filepath.Join(appData, "autoapply", "config.json")); err == nil {
			seed = real.Get()
		}
	}
	seed.AI.Active = config.ProviderLocal
	seed.AI.Local = config.ProviderConfig{BaseURL: base, Model: model, ReasoningEffort: "none", MaxTokens: 2048, Temperature: 0}
	// Default: exercise the global "vision" scrape mode. The engine wires the
	// visionbrowser source in from this mode, so no source needs to be ticked.
	seed.Sources.Browser.ScrapeMode = config.ScrapeVision
	seed.Focus.Sources = []string{"themuse"} // placeholder so the list isn't empty
	// Headless by default for an unattended test; AUTOAPPLY_LIVE_HEADFUL=1 mirrors
	// the user's real (headful) setup, which is far harder for boards to bot-wall.
	seed.Sources.Browser.Headful = os.Getenv("AUTOAPPLY_LIVE_HEADFUL") != ""
	if seed.Sources.Browser.MaxScreens == 0 || seed.Sources.Browser.MaxScreens > 2 {
		seed.Sources.Browser.MaxScreens = 2
	}
	if boards := os.Getenv("AUTOAPPLY_LIVE_BOARDS"); boards != "" {
		seed.Sources.Browser.Boards = strings.Split(boards, ",")
	} else if len(seed.Sources.Browser.Boards) == 0 {
		seed.Sources.Browser.Boards = []string{"indeed", "google"}
	}
	if scr := os.Getenv("AUTOAPPLY_LIVE_SCRAPE"); scr != "" {
		// Exercise the stealth HTML-scraping path instead of the vision source.
		seed.Focus.Sources = strings.Split(scr, ",")
		seed.Sources.Browser.ScrapeMode = config.ScrapeStealth
	}

	// A stable dir (AUTOAPPLY_LIVE_PROFILE) persists the vision browser profile
	// across runs so cf_clearance reuse can be exercised; otherwise throwaway.
	dir := os.Getenv("AUTOAPPLY_LIVE_PROFILE")
	if dir == "" {
		dir = t.TempDir()
	}
	cs, err := config.NewStore(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	if err := cs.Set(seed); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	db, err := store.New(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatalf("data store: %v", err)
	}
	hub := NewHub()

	// Stream the engine log to the test output so we can watch what it does.
	ch, cancelSub := hub.Subscribe()
	go func() {
		for ev := range ch {
			if ev.Type == "log" {
				t.Logf("[%s] %s", ev.Level, ev.Message)
			}
		}
	}()

	return New(cs, db, hub, dir), cancelSub
}

// TestLiveVisionSearch runs the real vision browser search against live boards.
// Skipped unless AUTOAPPLY_LIVE_AI=1. Needs the local model server running and a
// browser installed.
func TestLiveVisionSearch(t *testing.T) {
	if os.Getenv("AUTOAPPLY_LIVE_AI") == "" {
		t.Skip("set AUTOAPPLY_LIVE_AI=1 to run the live vision search test")
	}
	eng, cancel := liveEngine(t)
	defer cancel()

	ctx, cancelCtx := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancelCtx()

	added, err := eng.Search(ctx)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	t.Logf("search added %d job(s)", added)
	for _, r := range eng.db.Records() {
		t.Logf("  - %s @ %s [%s] %s", r.Job.Title, r.Job.Company, r.Job.Location, r.Job.URL)
	}
	// We can't guarantee a board renders (bot walls vary), so don't fail on zero;
	// the streamed log shows whether listings were read or a wall was hit.
}

// TestLiveVisionURLFallback exercises the browser+vision official-URL fallback:
// it inserts a job with a known employer and no link, then resolves the apply
// URL on demand. Skipped unless AUTOAPPLY_LIVE_AI=1.
func TestLiveVisionURLFallback(t *testing.T) {
	if os.Getenv("AUTOAPPLY_LIVE_AI") == "" {
		t.Skip("set AUTOAPPLY_LIVE_AI=1 to run the live URL-fallback test")
	}
	eng, cancel := liveEngine(t)
	defer cancel()

	company := envOr("AUTOAPPLY_LIVE_COMPANY", "Victaulic")
	job := store.Job{
		ID:      store.MakeJobID("test", company),
		Source:  "test",
		Title:   "Mechanical Engineer",
		Company: company,
	}
	if !eng.db.UpsertJob(job) {
		t.Fatalf("seed job")
	}

	ctx, cancelCtx := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancelCtx()

	res, err := eng.ResolveApply(ctx, job.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	t.Logf("resolved apply=%q company=%q ats=%q note=%q", res.ApplyURL, res.CompanyURL, res.ATS, res.Note)
	if res.ApplyURL == "" && res.CompanyURL == "" {
		t.Log("note: fallback found nothing — check the streamed log (search may have been bot-blocked)")
	}
}

// TestLiveRealConfigProvider loads the user's actual saved config (no overrides)
// and confirms it produces a working AI provider — i.e. the persisted settings
// are valid end to end. Skipped unless AUTOAPPLY_LIVE_AI=1.
func TestLiveRealConfigProvider(t *testing.T) {
	if os.Getenv("AUTOAPPLY_LIVE_AI") == "" {
		t.Skip("set AUTOAPPLY_LIVE_AI=1 to run the live real-config test")
	}
	appData := os.Getenv("APPDATA")
	if appData == "" {
		t.Skip("no APPDATA")
	}
	cs, err := config.NewStore(filepath.Join(appData, "autoapply", "config.json"))
	if err != nil {
		t.Fatalf("load real config: %v", err)
	}
	c := cs.Get()
	t.Logf("active=%q local.model=%q local.baseUrl=%q local.reasoningEffort=%q",
		c.AI.Active, c.AI.Local.Model, c.AI.Local.BaseURL, c.AI.Local.ReasoningEffort)
	provider, err := ai.New(c.AI)
	if err != nil {
		t.Fatalf("provider from saved config: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := provider.Generate(ctx, ai.Request{Prompt: "Reply with exactly the word: ok"})
	if err != nil {
		t.Fatalf("generate from saved config (%s): %v", provider.Name(), err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("saved config produced an empty answer")
	}
	t.Logf("provider %s replied: %q", provider.Name(), out)
}

// TestLiveVisionFindURLDirect drives the browser+vision URL search directly
// (bypassing the needsVisionFallback gate) so the fallback machinery is
// exercised even when the AI text picker would have guessed the URL.
func TestLiveVisionFindURLDirect(t *testing.T) {
	if os.Getenv("AUTOAPPLY_LIVE_AI") == "" {
		t.Skip("set AUTOAPPLY_LIVE_AI=1 to run the live direct vision-URL test")
	}
	eng, cancel := liveEngine(t)
	defer cancel()

	provider, err := ai.New(eng.cfg.Get().AI)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	company := envOr("AUTOAPPLY_LIVE_COMPANY", "Victaulic")
	job := store.Job{Title: "Mechanical Engineer", Company: company}

	ctx, cancelCtx := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancelCtx()

	res, ok := eng.visionFindApplyURL(ctx, job, provider)
	t.Logf("ok=%v apply=%q company=%q ats=%q note=%q", ok, res.ApplyURL, res.CompanyURL, res.ATS, res.Note)
	if !ok || res.ApplyURL == "" {
		t.Errorf("expected the browser+vision search to find a careers/apply URL for %s", company)
	}
}
