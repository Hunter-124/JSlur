// Command headless serves the AutoApply GUI + API over plain HTTP for local
// preview/verification (the production app is a Wails desktop window). Build:
//   go build -o autoapply-headless.exe ./cmd/headless
package main

import (
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"autoapply/internal/config"
	"autoapply/internal/engine"
	"autoapply/internal/server"
	"autoapply/internal/store"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8799", "listen address")
	dataDir := flag.String("data", ".rundata", "data directory")
	flag.Bool("open", false, "(ignored) open a browser")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	cfgStore, err := config.NewStore(filepath.Join(*dataDir, "config.json"))
	if err != nil {
		log.Fatal(err)
	}
	db, err := store.New(filepath.Join(*dataDir, "data.json"))
	if err != nil {
		log.Fatal(err)
	}
	hub := engine.NewHub()
	eng := engine.New(cfgStore, db, hub, *dataDir)
	assets := os.DirFS("web")
	handler := server.New(cfgStore, db, eng, assets.(fs.FS))
	log.Printf("AutoApply headless on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, handler))
}
