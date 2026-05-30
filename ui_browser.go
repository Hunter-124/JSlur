//go:build !windows || headless

package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
)

// runUI opens the system browser (when open is true) and blocks until Ctrl+C.
// Used on non-Windows platforms and on `-tags headless` builds.
func runUI(url string, open bool, cleanup func()) {
	if open {
		openBrowser(url)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
	cleanup()
}

// openBrowser best-effort launches the system browser at url.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("could not open browser automatically: %v", err)
	}
}
