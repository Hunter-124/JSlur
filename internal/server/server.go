// Package server exposes the engine over a small HTTP API and serves the
// embedded web GUI. Live updates are pushed to the browser via Server-Sent
// Events; everything else is plain JSON over REST.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"autoapply/internal/ai"
	"autoapply/internal/config"
	"autoapply/internal/engine"
	"autoapply/internal/jobs"
	"autoapply/internal/store"
)

// Server wires the HTTP handlers to the application services.
type Server struct {
	cfg    *config.Store
	db     *store.Store
	engine *engine.Engine
	static fs.FS
}

// New builds an http.Handler serving both the API and the GUI.
func New(cfg *config.Store, db *store.Store, eng *engine.Engine, static fs.FS) http.Handler {
	s := &Server{cfg: cfg, db: db, engine: eng, static: static}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("PUT /api/config", s.putConfig)
	mux.HandleFunc("POST /api/ai/test", s.testAI)
	mux.HandleFunc("GET /api/ai/models", s.getModels)
	mux.HandleFunc("GET /api/sources", s.getSources)

	mux.HandleFunc("GET /api/accounts", s.getAccounts)
	mux.HandleFunc("POST /api/accounts/{source}/connect", s.postConnectAccount)
	mux.HandleFunc("DELETE /api/accounts/{source}", s.deleteAccount)

	mux.HandleFunc("POST /api/resume/parse", s.postParseResume)

	mux.HandleFunc("GET /api/jobs", s.getJobs)
	mux.HandleFunc("POST /api/search", s.postSearch)
	mux.HandleFunc("POST /api/filter", s.postFilter)
	mux.HandleFunc("POST /api/tailor", s.postTailor)
	mux.HandleFunc("POST /api/jobs/{id}/generate", s.postGenerate)
	mux.HandleFunc("POST /api/jobs/{id}/apply", s.postApply)
	mux.HandleFunc("POST /api/jobs/{id}/resolve-apply", s.postResolveApply)
	mux.HandleFunc("POST /api/jobs/{id}/skip", s.postSkip)
	mux.HandleFunc("PUT /api/jobs/{id}/application", s.putApplication)
	mux.HandleFunc("POST /api/open-folder", s.postOpenFolder)

	mux.HandleFunc("GET /api/engine/status", s.getStatus)
	mux.HandleFunc("POST /api/engine/start", s.postStart)
	mux.HandleFunc("POST /api/engine/stop", s.postStop)
	mux.HandleFunc("POST /api/engine/run", s.postRun)

	mux.HandleFunc("GET /api/logs", s.getLogs)
	mux.HandleFunc("GET /api/events", s.events)

	mux.Handle("/", http.FileServer(http.FS(s.static)))
	return mux
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ---- config ----

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cfg.Get())
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	var cfg config.Config
	if err := readJSON(r, &cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Captured browser sessions are managed via the accounts endpoints; never let
	// a config save from the GUI silently wipe them.
	if cfg.Sources.Accounts == nil {
		cfg.Sources.Accounts = s.cfg.Get().Sources.Accounts
	}
	if err := s.cfg.Set(cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) getSources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, jobs.AvailableSources())
}

// postParseResume accepts a résumé as either an uploaded file (multipart field
// "resume") or pasted text (JSON {"text": "..."}), extracts/parses it, and saves
// the derived profile + target roles to the configuration.
func (s *Server) postParseResume(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	var (
		filename, text string
		data           []byte
	)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(12 << 20); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		f, hdr, err := r.FormFile("resume")
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("no résumé file uploaded: %w", err))
			return
		}
		defer f.Close()
		data, _ = io.ReadAll(io.LimitReader(f, 12<<20))
		filename = hdr.Filename
	} else {
		var body struct {
			Text     string `json:"text"`
			Filename string `json:"filename"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		text, filename = body.Text, body.Filename
	}

	res, err := s.engine.ImportResume(ctx, filename, text, data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// getModels lists the models the (saved) credentials for a provider can use.
func (s *Server) getModels(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	models, err := ai.ListModels(ctx, s.cfg.Get().AI, provider)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": models})
}

// AccountStatus describes a connectable source for the GUI.
type AccountStatus struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Hint       string     `json:"hint"`
	Connected  bool       `json:"connected"`
	CapturedAt *time.Time `json:"capturedAt,omitempty"`
}

func (s *Server) getAccounts(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	var out []AccountStatus
	for _, info := range jobs.AvailableSources() {
		if !jobs.IsConnectable(info.ID) {
			continue
		}
		st := AccountStatus{ID: info.ID, Name: info.Name, Hint: jobs.AccountSpecs[info.ID].Hint}
		if acc, ok := cfg.Sources.Accounts[info.ID]; ok && acc.Cookie != "" {
			st.Connected = true
			t := acc.CapturedAt
			st.CapturedAt = &t
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) postConnectAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.ConnectAccount(r.PathValue("source")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.DisconnectAccount(r.PathValue("source")); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// testAI runs a tiny round-trip against a provider to verify configuration.
func (s *Server) testAI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
	}
	_ = readJSON(r, &body)

	aiCfg := s.cfg.Get().AI
	if body.Provider != "" {
		aiCfg.Active = body.Provider
	}
	provider, err := ai.New(aiCfg)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	out, err := provider.Generate(ctx, ai.Request{
		Prompt:    "Reply with exactly the word: ok",
		MaxTokens: 16,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "name": provider.Name(), "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": provider.Name(), "sample": out})
}

// ---- jobs ----

func (s *Server) getJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.db.Records())
}

func (s *Server) postSearch(w http.ResponseWriter, r *http.Request) {
	go func() {
		// Generous budget: the AI Browser Search source drives a real browser and
		// calls a vision model per board, which is much slower than HTTP scraping.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		_, _ = s.engine.Search(ctx)
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

// postFilter runs the AI relevance filter (pipeline stage 2) in the background.
func (s *Server) postFilter(w http.ResponseWriter, r *http.Request) {
	s.engine.RunFilter()
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

// postTailor tailors all matched/discovered jobs (pipeline stage 3) in the background.
func (s *Server) postTailor(w http.ResponseWriter, r *http.Request) {
	s.engine.RunTailor()
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (s *Server) postGenerate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Instructions string `json:"instructions"`
	}
	_ = readJSON(r, &body) // optional body; absent is fine
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_ = s.engine.Process(ctx, id, body.Instructions)
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (s *Server) putApplication(w http.ResponseWriter, r *http.Request) {
	var edit engine.ApplicationEdit
	if err := readJSON(r, &edit); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.engine.EditApplication(r.PathValue("id"), edit); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) postOpenFolder(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.OpenExportFolder(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) postApply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = s.engine.Apply(ctx, id)
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

// postResolveApply resolves the company's own application URL for a job
// (synchronously, including a site crawl) and returns it. The resolved URL is
// also persisted on the job, so the client can simply re-read it.
func (s *Server) postResolveApply(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := s.engine.ResolveApply(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) postSkip(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Skip(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- engine ----

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.Status())
}

func (s *Server) postStart(w http.ResponseWriter, r *http.Request) {
	s.engine.Start()
	writeJSON(w, http.StatusOK, s.engine.Status())
}

func (s *Server) postStop(w http.ResponseWriter, r *http.Request) {
	s.engine.Stop()
	writeJSON(w, http.StatusOK, s.engine.Status())
}

func (s *Server) postRun(w http.ResponseWriter, r *http.Request) {
	s.engine.RunOnce()
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

// ---- live events ----

func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.Hub().History())
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, cancel := s.engine.Hub().Subscribe()
	defer cancel()

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}
