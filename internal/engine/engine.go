// Package engine orchestrates the full pipeline: search job boards, tailor
// applications with AI, and act on the ready ones via the configured channel.
// It runs either on demand (single cycle) or continuously on a timer, emitting
// log/refresh events to the GUI through the Hub.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"autoapply/internal/ai"
	"autoapply/internal/browser"
	"autoapply/internal/config"
	"autoapply/internal/jobs"
	"autoapply/internal/resume"
	"autoapply/internal/store"
)

// Engine coordinates the pipeline and holds automation state.
type Engine struct {
	cfg     *config.Store
	db      *store.Store
	dataDir string
	agg     jobs.Aggregator
	hub     *Hub

	mu      sync.Mutex
	running bool
	busy    bool
	lastRun time.Time
	cancel  context.CancelFunc

	catMu    sync.Mutex
	catCache map[string][]jobs.Category // AI category picks, keyed by source+interest
}

// New creates an Engine.
func New(cfg *config.Store, db *store.Store, hub *Hub, dataDir string) *Engine {
	return &Engine{cfg: cfg, db: db, hub: hub, dataDir: dataDir, catCache: map[string][]jobs.Category{}}
}

// Hub exposes the event hub for the HTTP layer.
func (e *Engine) Hub() *Hub { return e.hub }

func (e *Engine) logf(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("[%s] %s", level, msg)
	e.hub.Publish(Event{Type: "log", Level: level, Message: msg})
}

func (e *Engine) refresh() { e.hub.Publish(Event{Type: "refresh"}) }

// Search queries the enabled sources and stores any new jobs. Returns the count
// of newly added jobs.
func (e *Engine) Search(ctx context.Context) (int, error) {
	cfg := e.cfg.Get()
	focus := cfg.Focus
	if len(focus.Sources) == 0 {
		return 0, fmt.Errorf("no job sources enabled")
	}

	// The AI maps the free-text interest onto each board's categories. If no AI
	// is configured the selector falls back to keyword matching, so search still
	// works without a key.
	var provider ai.Provider
	if p, err := ai.New(cfg.AI); err == nil {
		provider = p
	}

	where := focus.Location.Query()
	if where == "" {
		where = "anywhere in the US"
	}
	e.logf("info", "searching %d source(s) near %s for: %s",
		len(focus.Sources), where, orPlaceholder(focus.Interest))

	q := jobs.Query{
		Focus:  focus,
		Creds:  cfg.Sources,
		Select: e.categorySelector(provider, focus.Interest),
		Vision: e.visionFunc(provider),
	}
	found, results := e.agg.Search(ctx, q)
	for _, r := range results {
		switch {
		case r.Err != nil && errors.Is(r.Err, jobs.ErrNotConfigured):
			e.logf("info", "source %q skipped (%v)", r.Source, r.Err)
		case r.Err != nil:
			e.logf("warn", "source %q failed: %v", r.Source, r.Err)
		default:
			e.logf("info", "source %q returned %d jobs", r.Source, len(r.Jobs))
		}
	}

	// Dedup against what's already stored (not just within this batch): the same
	// role often appears across boards and across repeated searches.
	seenKey := map[string]bool{}
	seenURL := map[string]bool{}
	for _, r := range e.db.Records() {
		seenKey[dedupKey(r.Job.Company, r.Job.Title)] = true
		seenURL[normURL(r.Job.URL)] = true
	}
	added, dupes := 0, 0
	for _, j := range found {
		k, u := dedupKey(j.Company, j.Title), normURL(j.URL)
		if seenKey[k] || (u != "" && seenURL[u]) {
			dupes++
			continue
		}
		if e.db.UpsertJob(j) {
			seenKey[k], seenURL[u] = true, true
			added++
			_ = e.db.SaveApplication(store.Application{JobID: j.ID, Status: store.StatusDiscovered})
		}
	}
	if dupes > 0 {
		e.logf("success", "search complete: %d new job(s) added, %d duplicate(s) skipped", added, dupes)
	} else {
		e.logf("success", "search complete: %d new job(s) added", added)
	}
	e.refresh()
	return added, nil
}

// dedupKey is a normalized company|title key for cross-source de-duplication.
func dedupKey(company, title string) string {
	return strings.ToLower(strings.TrimSpace(company)) + "|" + strings.ToLower(strings.TrimSpace(title))
}

// normURL normalizes a URL for de-duplication: lowercased host+path, no scheme,
// query or trailing slash.
func normURL(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, "/")
}

// categorySelector returns a jobs.CategorySelector that uses the AI provider to
// map the user's interest onto a board's taxonomy, with a keyword-matching
// fallback and per-(source,interest) caching.
func (e *Engine) categorySelector(provider ai.Provider, interest string) jobs.CategorySelector {
	const maxPick = 4
	return func(ctx context.Context, sourceID string, available []jobs.Category) ([]jobs.Category, error) {
		if len(available) == 0 {
			return nil, nil
		}
		key := fmt.Sprintf("%s|%d|%s", sourceID, len(available), strings.TrimSpace(strings.ToLower(interest)))
		e.catMu.Lock()
		cached, ok := e.catCache[key]
		e.catMu.Unlock()
		if ok {
			return cached, nil
		}

		var chosen []jobs.Category
		if provider != nil && strings.TrimSpace(interest) != "" {
			labels := make([]string, len(available))
			for i, c := range available {
				labels[i] = c.Label
			}
			idx, err := resume.SelectCategories(ctx, provider, interest, labels, maxPick)
			if err != nil {
				e.logf("warn", "%s: AI category match failed (%v); using keyword matching", sourceID, err)
				chosen = jobs.HeuristicSelect(interest, available, maxPick)
			} else {
				for _, i := range idx {
					chosen = append(chosen, available[i])
				}
			}
		} else {
			chosen = jobs.HeuristicSelect(interest, available, maxPick)
		}

		if len(chosen) > 0 {
			names := make([]string, len(chosen))
			for i, c := range chosen {
				names[i] = c.Label
			}
			e.logf("info", "%s: matched categories → %s", sourceID, strings.Join(names, ", "))
		} else {
			e.logf("info", "%s: no specific category match; searching broadly", sourceID)
		}

		e.catMu.Lock()
		e.catCache[key] = chosen
		e.catMu.Unlock()
		return chosen, nil
	}
}

// visionFunc returns a jobs.VisionFunc backed by the AI provider, so the AI
// Browser Search source can read job listings off page screenshots. Returns nil
// when no provider is configured (that source then reports a friendly note).
func (e *Engine) visionFunc(provider ai.Provider) jobs.VisionFunc {
	if provider == nil {
		return nil
	}
	return func(ctx context.Context, prompt string, images [][]byte) (string, error) {
		imgs := make([]ai.Image, 0, len(images))
		for _, b := range images {
			imgs = append(imgs, ai.Image{Mime: "image/png", Data: b})
		}
		return provider.Vision(ctx, ai.Request{Prompt: prompt, MaxTokens: 4096}, imgs)
	}
}

func orPlaceholder(interest string) string {
	if strings.TrimSpace(interest) == "" {
		return "(no interest set yet — describe your target roles in Job Focus)"
	}
	return interest
}

// Process generates (or, with instructions, refines) tailored materials for one
// job and records the result.
func (e *Engine) Process(ctx context.Context, jobID, instructions string) error {
	job, ok := e.db.GetJob(jobID)
	if !ok {
		return fmt.Errorf("unknown job %s", jobID)
	}
	cfg := e.cfg.Get()
	provider, err := ai.New(cfg.AI)
	if err != nil {
		return fmt.Errorf("AI not ready: %w", err)
	}

	app, _ := e.db.GetApplication(jobID)
	app.JobID = jobID
	app.Status = store.StatusGenerating
	app.Error = ""
	_ = e.db.SaveApplication(app)
	e.refresh()
	if instructions != "" {
		e.logf("info", "refining application: %s @ %s", job.Title, job.Company)
	} else {
		e.logf("info", "tailoring application: %s @ %s", job.Title, job.Company)
	}

	res, err := resume.Generate(ctx, provider, cfg.Candidate, job, resume.Options{
		Interest:       cfg.Focus.Interest,
		Instructions:   instructions,
		PreviousResume: app.Resume,
		PreviousCover:  app.CoverLetter,
	})
	if err != nil {
		app.Status = store.StatusError
		app.Error = err.Error()
		_ = e.db.SaveApplication(app)
		e.logf("error", "failed to tailor %s @ %s: %v", job.Title, job.Company, err)
		e.refresh()
		return err
	}

	app.MatchScore = res.MatchScore
	app.MatchReason = res.MatchReason
	app.Strengths = res.Strengths
	app.Gaps = res.Gaps
	app.Resume = res.Resume
	app.CoverLetter = res.CoverLetter
	app.Error = ""

	if res.MatchScore < cfg.Focus.MinMatchScore {
		app.Status = store.StatusSkipped
		app.Notes = fmt.Sprintf("Auto-skipped: match %d below threshold %d", res.MatchScore, cfg.Focus.MinMatchScore)
		e.logf("info", "skipped %s @ %s (match %d < %d)", job.Title, job.Company, res.MatchScore, cfg.Focus.MinMatchScore)
	} else {
		app.Status = store.StatusReady
		e.logf("success", "ready: %s @ %s (match %d)", job.Title, job.Company, res.MatchScore)
	}
	_ = e.db.SaveApplication(app)
	// For jobs that made the cut, resolve the company's own application URL so it
	// can be applied to at the source rather than via the board it came from.
	// Cheap path only (no site crawl) to keep the pipeline fast.
	if app.Status == store.StatusReady && job.ApplyURL == "" {
		e.resolveOfficial(ctx, job, provider, false)
	}
	e.refresh()
	return nil
}

// officialURLPicker returns a jobs.OfficialURLPicker backed by the AI provider,
// or nil when no provider is configured (resolution then uses links only).
func (e *Engine) officialURLPicker(provider ai.Provider) jobs.OfficialURLPicker {
	if provider == nil {
		return nil
	}
	return func(ctx context.Context, job store.Job, candidates []string) (string, error) {
		return resume.PickApplyURL(ctx, provider, job, candidates)
	}
}

// resolveOfficial finds the company's own application/careers URL for a job and
// records it on the stored job. deep=true also crawls the company site.
func (e *Engine) resolveOfficial(ctx context.Context, job store.Job, provider ai.Provider, deep bool) jobs.Official {
	res := jobs.ResolveOfficial(ctx, job, e.officialURLPicker(provider), deep)
	if res.ApplyURL != "" || res.CompanyURL != "" {
		_, _ = e.db.SetJobApplyInfo(job.ID, res.ApplyURL, res.CompanyURL)
	}
	return res
}

// ResolveApply resolves the official application URL for one job on demand
// (including a company-site crawl) and returns the result. Powers the GUI's
// "find official application" action.
func (e *Engine) ResolveApply(ctx context.Context, jobID string) (jobs.Official, error) {
	job, ok := e.db.GetJob(jobID)
	if !ok {
		return jobs.Official{}, fmt.Errorf("unknown job %s", jobID)
	}
	var provider ai.Provider
	if p, err := ai.New(e.cfg.Get().AI); err == nil {
		provider = p
	}
	e.logf("info", "resolving official application URL for %s @ %s", job.Title, job.Company)
	res := e.resolveOfficial(ctx, job, provider, true)
	if res.ApplyURL != "" {
		e.logf("success", "official application for %s @ %s → %s (%s)", job.Title, job.Company, res.ApplyURL, res.Note)
	} else {
		e.logf("warn", "no official application URL found for %s @ %s", job.Title, job.Company)
	}
	e.refresh()
	return res, nil
}

// ConnectAccount opens a browser so the user can sign in to a board, then
// captures the resulting session (cookies + User-Agent) and stores it for the
// scrapers to replay. It runs asynchronously and reports progress via the log.
func (e *Engine) ConnectAccount(source string) error {
	spec, ok := jobs.AccountSpecs[source]
	if !ok {
		return fmt.Errorf("source %q does not support connecting an account", source)
	}
	go func() {
		e.logf("info", "opening a browser to connect %s — sign in / clear any check, and the session is captured automatically", source)
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		res, err := browser.Capture(ctx, spec.LoginURL, spec.AuthCookies, 5*time.Minute)
		if err != nil {
			e.logf("error", "connect %s failed: %v", source, err)
			return
		}
		cfg := e.cfg.Get()
		if cfg.Sources.Accounts == nil {
			cfg.Sources.Accounts = map[string]config.Account{}
		}
		cfg.Sources.Accounts[source] = config.Account{
			Cookie:     res.CookieHeader(),
			UserAgent:  res.UserAgent,
			CapturedAt: time.Now(),
		}
		if err := e.cfg.Set(cfg); err != nil {
			e.logf("error", "saving %s session failed: %v", source, err)
			return
		}
		e.logf("success", "connected %s — captured %d cookies; scrapes will now use this session", source, len(res.Cookies))
		e.refresh()
	}()
	return nil
}

// DisconnectAccount forgets a captured session for a source.
func (e *Engine) DisconnectAccount(source string) error {
	cfg := e.cfg.Get()
	if cfg.Sources.Accounts != nil {
		delete(cfg.Sources.Accounts, source)
	}
	if err := e.cfg.Set(cfg); err != nil {
		return err
	}
	e.logf("info", "disconnected %s session", source)
	e.refresh()
	return nil
}

// Apply acts on a prepared application using the configured channel.
func (e *Engine) Apply(ctx context.Context, jobID string) error {
	job, ok := e.db.GetJob(jobID)
	if !ok {
		return fmt.Errorf("unknown job %s", jobID)
	}
	app, ok := e.db.GetApplication(jobID)
	if !ok || app.Resume == "" {
		return fmt.Errorf("no prepared application for this job — generate it first")
	}
	cfg := e.cfg.Get()

	switch cfg.Apply.Channel {
	case config.ApplyExport:
		dir := e.exportDir(cfg)
		folder, err := exportApplication(dir, job, app)
		if err != nil {
			return e.failApply(app, "export failed: %v", err)
		}
		app.Notes = "Exported to " + folder
		e.logf("success", "exported application for %s @ %s -> %s", job.Title, job.Company, folder)

	case config.ApplyEmail:
		if err := emailApplication(cfg.Apply.SMTP, job.ApplyEmail, job, app); err != nil {
			return e.failApply(app, "email failed: %v", err)
		}
		app.Notes = "Emailed to " + job.ApplyEmail
		e.logf("success", "emailed application for %s @ %s to %s", job.Title, job.Company, job.ApplyEmail)

	default: // review
		app.Notes = "Marked as applied (manual/review channel)"
		e.logf("success", "marked applied: %s @ %s", job.Title, job.Company)
	}

	now := time.Now()
	app.AppliedAt = &now
	app.Status = store.StatusApplied
	_ = e.db.SaveApplication(app)
	e.refresh()
	return nil
}

func (e *Engine) failApply(app store.Application, format string, args ...any) error {
	app.Status = store.StatusError
	app.Error = fmt.Sprintf(format, args...)
	_ = e.db.SaveApplication(app)
	e.logf("error", "%s", app.Error)
	e.refresh()
	return fmt.Errorf("%s", app.Error)
}

// Skip marks an application as skipped.
func (e *Engine) Skip(jobID string) error {
	app, _ := e.db.GetApplication(jobID)
	app.JobID = jobID
	app.Status = store.StatusSkipped
	app.Notes = "Skipped by user"
	if err := e.db.SaveApplication(app); err != nil {
		return err
	}
	e.refresh()
	return nil
}

// ApplicationEdit carries user edits to a prepared application.
type ApplicationEdit struct {
	Status      string `json:"status"`
	Notes       string `json:"notes"`
	Resume      string `json:"resume"`
	CoverLetter string `json:"coverLetter"`
}

// EditApplication applies manual edits (status tracking, notes, hand-edited
// materials) to an existing application.
func (e *Engine) EditApplication(jobID string, edit ApplicationEdit) error {
	app, ok := e.db.GetApplication(jobID)
	if !ok {
		return fmt.Errorf("no application for this job yet")
	}
	if edit.Status != "" {
		if !store.TrackingStatuses[edit.Status] {
			return fmt.Errorf("invalid status %q", edit.Status)
		}
		if edit.Status == store.StatusApplied && app.AppliedAt == nil {
			now := time.Now()
			app.AppliedAt = &now
		}
		app.Status = edit.Status
	}
	app.Notes = edit.Notes
	if edit.Resume != "" {
		app.Resume = edit.Resume
	}
	if edit.CoverLetter != "" {
		app.CoverLetter = edit.CoverLetter
	}
	if err := e.db.SaveApplication(app); err != nil {
		return err
	}
	e.logf("info", "updated application (%s)", app.Status)
	e.refresh()
	return nil
}

func (e *Engine) exportDir(cfg config.Config) string {
	if cfg.Apply.ExportDir != "" {
		return cfg.Apply.ExportDir
	}
	return filepath.Join(e.dataDir, "applications")
}

// OpenExportFolder opens the export directory in the OS file manager.
func (e *Engine) OpenExportFolder() error {
	dir := e.exportDir(e.cfg.Get())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	// explorer.exe returns a non-zero exit code even on success, so ignore it.
	_ = cmd.Start()
	return nil
}

// Filter is pipeline stage 2: a cheap AI relevance pass over discovered jobs.
// Each is advanced to "matched" (keep) or "skipped" (below MinPrescreenScore),
// so the expensive tailoring stage only runs on jobs worth tailoring.
func (e *Engine) Filter(ctx context.Context) (matched, skipped int, err error) {
	cfg := e.cfg.Get()
	provider, err := ai.New(cfg.AI)
	if err != nil {
		return 0, 0, fmt.Errorf("AI not ready: %w", err)
	}
	threshold := cfg.Focus.MinPrescreenScore

	// Collect the jobs that haven't been screened yet.
	var pending []store.Job
	for _, r := range e.db.Records() {
		st := store.StatusDiscovered
		if r.Application != nil {
			st = r.Application.Status
		}
		if st == store.StatusDiscovered {
			pending = append(pending, r.Job)
		}
	}
	if len(pending) == 0 {
		e.logf("info", "filter: nothing new to screen")
		return 0, 0, nil
	}

	// Score many jobs per AI call (one request screens a whole batch), so the
	// filter stays cheap even when a search returns lots of jobs.
	const batchSize = 15
	calls := (len(pending) + batchSize - 1) / batchSize
	e.logf("info", "filtering %d job(s) for relevance in %d AI call(s) (keep ≥ %d)…", len(pending), calls, threshold)

	for i := 0; i < len(pending); i += batchSize {
		if ctx.Err() != nil {
			break
		}
		end := i + batchSize
		if end > len(pending) {
			end = len(pending)
		}
		chunk := pending[i:end]
		results, perr := resume.PrescreenBatch(ctx, provider, cfg.Candidate, cfg.Focus.Interest, chunk)
		for _, j := range chunk {
			app, _ := e.db.GetApplication(j.ID)
			app.JobID = j.ID
			res, ok := results[j.ID]
			if perr != nil || !ok {
				// Screening unavailable for this job — let it through, don't drop it.
				app.Status = store.StatusMatched
				app.PrescreenReason = "prescreen unavailable; passed through"
				matched++
			} else {
				app.PrescreenScore = res.Score
				app.PrescreenReason = res.Reason
				if threshold > 0 && res.Score < threshold {
					app.Status = store.StatusSkipped
					app.Notes = fmt.Sprintf("Filtered out by AI: relevance %d below %d", res.Score, threshold)
					skipped++
				} else {
					app.Status = store.StatusMatched
					matched++
				}
			}
			_ = e.db.SaveApplication(app)
		}
	}
	e.logf("success", "filter complete: %d matched, %d filtered out", matched, skipped)
	e.refresh()
	return matched, skipped, nil
}

// RunFilter runs the filter stage in the background (for the GUI button).
func (e *Engine) RunFilter() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, _, err := e.Filter(ctx); err != nil {
			e.logf("error", "filter failed: %v", err)
		}
	}()
}

// RunTailor is pipeline stage 3 on demand: tailor materials for every job that
// passed the filter (matched) — or every discovered job when the filter is off.
// Applying stays per-job / automation-driven (review-first).
func (e *Engine) RunTailor() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if _, err := ai.New(e.cfg.Get().AI); err != nil {
			e.logf("warn", "AI not configured (%v) — set a provider in AI & Apply to tailor", err)
			return
		}
		n := 0
		for _, r := range e.db.Records() {
			if ctx.Err() != nil {
				break
			}
			st := store.StatusDiscovered
			if r.Application != nil {
				st = r.Application.Status
			}
			if st == store.StatusMatched || st == store.StatusDiscovered {
				if err := e.Process(ctx, r.Job.ID, ""); err == nil {
					n++
				}
			}
		}
		e.logf("success", "tailored %d job(s)", n)
	}()
}

// RunOnce executes a single pipeline cycle in the background.
func (e *Engine) RunOnce() {
	ctx := context.Background()
	go e.runCycleGuarded(ctx)
}

func (e *Engine) runCycleGuarded(ctx context.Context) {
	e.mu.Lock()
	if e.busy {
		e.mu.Unlock()
		e.logf("warn", "a cycle is already running")
		return
	}
	e.busy = true
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		e.busy = false
		e.lastRun = time.Now()
		e.mu.Unlock()
		e.refresh()
	}()

	e.runCycle(ctx)
}

func (e *Engine) runCycle(ctx context.Context) {
	e.logf("info", "=== pipeline cycle started ===")
	if _, err := e.Search(ctx); err != nil {
		e.logf("error", "search failed: %v", err)
		return
	}

	cfg := e.cfg.Get()

	// Verify the AI provider once so a misconfiguration logs a single clear
	// message instead of one error per discovered job.
	if _, err := ai.New(cfg.AI); err != nil {
		e.logf("warn", "AI not configured (%v) — jobs were saved; set a provider in AI & Apply to tailor them", err)
		return
	}

	// Stage 2: cheap AI relevance filter (skipped when the threshold is 0).
	if cfg.Focus.MinPrescreenScore > 0 {
		if _, _, err := e.Filter(ctx); err != nil {
			e.logf("warn", "filter stage skipped: %v", err)
		}
	}

	// Stage 3: tailor the jobs that passed the filter (matched), or every
	// discovered job when the filter is off.
	for _, r := range e.db.Records() {
		if ctx.Err() != nil {
			return
		}
		st := store.StatusDiscovered
		if r.Application != nil {
			st = r.Application.Status
		}
		if st == store.StatusDiscovered || st == store.StatusMatched {
			if err := e.Process(ctx, r.Job.ID, ""); err != nil {
				// already logged; keep going with the rest
				continue
			}
		}
	}

	// Optionally act on the ready applications.
	if cfg.Apply.AutoApply && cfg.Apply.Channel != config.ApplyReview {
		max := cfg.Apply.MaxAppliesPerRun
		if max <= 0 {
			max = 5
		}
		applied := 0
		for _, r := range e.db.Records() {
			if ctx.Err() != nil || applied >= max {
				break
			}
			if r.Application != nil && r.Application.Status == store.StatusReady {
				if err := e.Apply(ctx, r.Job.ID); err == nil {
					applied++
				}
			}
		}
		e.logf("success", "auto-applied to %d job(s) this cycle", applied)
	} else {
		e.logf("info", "applications prepared and waiting for your review")
	}

	e.logf("success", "=== pipeline cycle finished ===")
}

// Start begins continuous automation on the configured interval.
func (e *Engine) Start() {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.running = true
	e.cancel = cancel
	e.mu.Unlock()

	go e.loop(ctx)
}

// Stop halts continuous automation. An in-flight cycle finishes on its own.
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return
	}
	e.running = false
	if e.cancel != nil {
		e.cancel()
	}
	e.logf("warn", "automation stopped")
	e.refresh()
}

func (e *Engine) loop(ctx context.Context) {
	e.logf("success", "automation started")
	e.refresh()
	e.runCycleGuarded(ctx)
	for {
		d := time.Duration(maxInt(e.cfg.Get().Apply.IntervalMinutes, 1)) * time.Minute
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			e.runCycleGuarded(ctx)
		}
	}
}

// Status is a snapshot for the dashboard.
type Status struct {
	Running        bool        `json:"running"`
	Busy           bool        `json:"busy"`
	LastRun        *time.Time  `json:"lastRun,omitempty"`
	AutoMode       bool        `json:"autoMode"`
	Channel        string      `json:"channel"`
	ActiveProvider string      `json:"activeProvider"`
	ProviderError  string      `json:"providerError,omitempty"`
	Stats          store.Stats `json:"stats"`
}

// Status returns the current engine status.
func (e *Engine) Status() Status {
	e.mu.Lock()
	st := Status{Running: e.running, Busy: e.busy}
	if !e.lastRun.IsZero() {
		lr := e.lastRun
		st.LastRun = &lr
	}
	e.mu.Unlock()

	cfg := e.cfg.Get()
	st.AutoMode = cfg.Apply.AutoMode
	st.Channel = cfg.Apply.Channel
	st.Stats = e.db.Stats()
	if p, err := ai.New(cfg.AI); err != nil {
		st.ProviderError = err.Error()
	} else {
		st.ActiveProvider = p.Name()
	}
	return st
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
