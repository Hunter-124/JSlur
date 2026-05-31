package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"autoapply/internal/config"
	"autoapply/internal/engine"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the object Wails binds into the frontend. Rather than reimplement the
// API surface, it forwards calls to the existing HTTP handler in-process (no
// TCP, no localhost) and bridges engine events to the Wails event bus.
type App struct {
	ctx     context.Context
	cfg     *config.Store
	engine  *engine.Engine
	hub     *engine.Hub
	handler http.Handler
}

// NewApp wires the App to the shared services.
func NewApp(cfg *config.Store, eng *engine.Engine, hub *engine.Hub, handler http.Handler) *App {
	return &App{cfg: cfg, engine: eng, hub: hub, handler: handler}
}

// startup captures the Wails context, starts forwarding engine events to the
// frontend, and resumes automation if the user left it on.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go a.pumpEvents()
	if a.cfg.Get().Apply.AutoMode {
		a.engine.Start()
	}
}

// shutdown stops the engine cleanly when the window closes.
func (a *App) shutdown(ctx context.Context) {
	a.engine.Stop()
}

// pumpEvents relays the engine's live log/refresh events to the frontend over
// the Wails event bus (the replacement for the old SSE stream).
func (a *App) pumpEvents() {
	ch, cancel := a.hub.Subscribe()
	defer cancel()
	for ev := range ch {
		wruntime.EventsEmit(a.ctx, "backend", ev)
	}
}

// APIResponse mirrors an HTTP response for the in-process API bridge.
type APIResponse struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// Request runs an API call against the in-process handler — exactly the routes
// the old HTTP server exposed, but with no network involved. The frontend's
// api() helper calls this instead of fetch().
func (a *App) Request(method, path, body string) APIResponse {
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	return APIResponse{Status: rec.Code, Body: rec.Body.String()}
}

// RoundWindow makes the native window render its CSS rounded corners correctly:
// it clears any clip region and switches the window to a transparent backdrop
// so the corners are antialiased (Windows 10 has no native window rounding, and
// a GDI region clip aliases the curve). Called by the frontend on load/resize.
func (a *App) RoundWindow() {
	roundCorners(11)
}

// UploadResume parses a résumé file (passed as base64) and saves the derived
// profile + target roles, returning the same JSON the parse endpoint returned.
// File uploads can't ride the string-based Request bridge, so they get their
// own binding.
func (a *App) UploadResume(filename, contentB64 string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 90*time.Second)
	defer cancel()
	res, err := a.engine.ImportResume(ctx, filename, "", data)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(res)
	return string(b), nil
}
