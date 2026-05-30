//go:build windows && !headless

package main

import (
	"log"

	webview2 "github.com/jchv/go-webview2"
)

// runUI opens a native WebView2 desktop window that renders the local GUI.
// WebView2 ships with Windows 10/11 (it's the Microsoft Edge runtime), so this
// needs no C compiler and produces a single self-contained .exe.
//
// Build the browser-based fallback instead with:  go build -tags headless
func runUI(url string, _ bool, cleanup func()) {
	defer cleanup()

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "AutoApply — AI Job Application Assistant",
			Width:  1200,
			Height: 840,
			Center: true,
		},
	})
	if w == nil {
		// Window creation failed (most likely the WebView2 runtime is missing).
		// Keep the server alive so the user can still open the GUI in a browser.
		log.Println("could not create a native window — is the WebView2 runtime installed?")
		log.Printf("you can still use the app in your browser at %s", url)
		select {}
	}
	defer w.Destroy()
	w.Navigate(url)
	w.Run() // blocks until the window is closed
}
