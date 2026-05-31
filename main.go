// Command autoapply is an AI-powered job search & application assistant. It runs
// as a native desktop app: a frameless Wails window renders the GUI, from which
// you set your candidate profile and job focus, then it searches public job
// boards, tailors a resume + cover letter per posting using the AI provider of
// your choice, and prepares applications for review.
package main

import (
	"embed"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"autoapply/internal/config"
	"autoapply/internal/engine"
	"autoapply/internal/server"
	"autoapply/internal/store"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:web
var webFS embed.FS

func main() {
	dataDir := defaultDataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	cfgStore, err := config.NewStore(filepath.Join(dataDir, "config.json"))
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	db, err := store.New(filepath.Join(dataDir, "data.json"))
	if err != nil {
		log.Fatalf("load store: %v", err)
	}

	hub := engine.NewHub()
	eng := engine.New(cfgStore, db, hub, dataDir)

	assets, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("static fs: %v", err)
	}
	// The handler is invoked in-process by the App.Request bridge — it carries
	// the whole API surface, but nothing is served over the network.
	handler := server.New(cfgStore, db, eng, assets)
	app := NewApp(cfgStore, eng, hub, handler)

	err = wails.Run(&options.App{
		Title:            "AutoApply",
		Width:            1200,
		Height:           840,
		MinWidth:         960,
		MinHeight:        640,
		Frameless:        true, // we draw our own titlebar/controls
		BackgroundColour: options.NewRGBA(13, 15, 21, 255),
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		Windows: &windows.Options{
			// We draw our own border and clip rounded corners via a window region
			// (see RoundWindow / window_windows.go), so disable Wails' own frameless
			// decorations to avoid a competing border.
			DisableFramelessWindowDecorations: true,
			Theme:                             windows.Dark,
		},
	})
	if err != nil {
		log.Fatalf("run: %v", err)
	}
}

// defaultDataDir returns the per-user data directory for config + discovered jobs.
func defaultDataDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base, _ = os.Getwd()
	}
	return filepath.Join(base, "autoapply")
}
