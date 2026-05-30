// Command autoapply is an AI-powered job search & application assistant. It runs
// a local web GUI from which you set your candidate profile and job focus, then
// it searches public job boards, tailors a resume + cover letter per posting
// using the AI provider of your choice, and prepares applications for review.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"autoapply/internal/config"
	"autoapply/internal/engine"
	"autoapply/internal/server"
	"autoapply/internal/store"
)

//go:embed all:web
var webFS embed.FS

func main() {
	addr := flag.String("addr", "127.0.0.1:8765", "address to listen on")
	openBrowserFlag := flag.Bool("open", true, "open the GUI in the default browser on start")
	dataDirFlag := flag.String("data", "", "data directory (default: per-user config dir)")
	flag.Parse()

	dataDir := *dataDirFlag
	if dataDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			base, _ = os.Getwd()
		}
		dataDir = filepath.Join(base, "autoapply")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	log.Printf("data directory: %s", dataDir)

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

	static, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("static fs: %v", err)
	}
	handler := server.New(cfgStore, db, eng, static)

	// Bind the listener up front so we know the real URL (and can fall back to
	// an ephemeral port if the preferred one is taken).
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Printf("could not bind %s (%v); falling back to an automatic port", *addr, err)
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatalf("listen: %v", err)
		}
	}
	url := fmt.Sprintf("http://%s", ln.Addr().String())

	srv := &http.Server{Handler: handler}

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────┐")
	fmt.Println("  │   AutoApply — AI job application assistant    │")
	fmt.Println("  └─────────────────────────────────────────────┘")
	fmt.Printf("\n  GUI ready at:  %s\n\n", url)
	fmt.Println("  Set up the Profile and AI & Apply tabs, then run a search.")
	fmt.Println("  Close the window (or press Ctrl+C here) to quit.")
	fmt.Println()

	// Start automation automatically if the user previously enabled it.
	if cfgStore.Get().Apply.AutoMode {
		eng.Start()
	}

	cleanup := func() {
		eng.Stop()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		log.Println("bye")
	}

	// runUI blocks until the user quits. It is implemented per build target: a
	// native WebView2 desktop window on Windows (default), or the system browser
	// on other platforms / when built with `-tags headless`.
	runUI(url, *openBrowserFlag, cleanup)
}
