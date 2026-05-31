package browser

// This file adds an optional second page-capture engine for vision search: a
// Python + Playwright sidecar driven by the pw-stealth-enhanced package. Its
// anti-bot evasions get past walls the built-in chromedp engine can't (notably
// Indeed's PerimeterX "Security Check"), at the cost of requiring a local Python
// with playwright + pw-stealth-enhanced installed. The Go side embeds the script,
// launches it as a long-lived subprocess, and speaks a tiny line-delimited JSON
// protocol that mirrors Session.Shots. When the sidecar can't start, callers fall
// back to the built-in chromedp engine.

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

//go:embed stealth_sidecar.py
var stealthSidecarPy string

// Shooter captures rendered pages (screenshots) for vision search.
type Shooter interface {
	Shots(ctx context.Context, pageURL, cookieHeader, userAgent string, maxScreens int) (Shots, error)
	Close()
}

// Renderer fetches a URL's fully-rendered HTML for the stealth scraping path.
type Renderer interface {
	RenderHTML(ctx context.Context, pageURL, cookieHeader, userAgent string) (string, error)
	Close()
}

// engine is the union of both capabilities; the built-in chromedp Session and
// the Python sidecar each implement it, so one factory serves both interfaces.
type engine interface {
	Shots(ctx context.Context, pageURL, cookieHeader, userAgent string, maxScreens int) (Shots, error)
	RenderHTML(ctx context.Context, pageURL, cookieHeader, userAgent string) (string, error)
	Close()
}

// newEngine builds the requested browser backend: the Python Playwright stealth
// sidecar when opts.Engine == "python" (falling back to the built-in chromedp
// browser if it can't start), else chromedp. The note describes what's in use.
func newEngine(ctx context.Context, opts SessionOptions) (engine, string, error) {
	if strings.EqualFold(opts.Engine, "python") {
		if sh, err := newPyShooter(ctx, opts); err == nil {
			return sh, "Python Playwright stealth engine", nil
		} else {
			s, cerr := NewSession(ctx, opts)
			if cerr != nil {
				return nil, "", fmt.Errorf("python engine unavailable (%v); built-in engine also failed: %w", err, cerr)
			}
			return s, fmt.Sprintf("built-in browser (Python stealth engine unavailable: %s)", truncate(err.Error(), 200)), nil
		}
	}
	s, err := NewSession(ctx, opts)
	if err != nil {
		return nil, "", err
	}
	return s, "built-in browser", nil
}

// NewShooter returns a screenshot engine for vision search (Python sidecar when
// selected, else built-in). The note describes the engine actually in use.
func NewShooter(ctx context.Context, opts SessionOptions) (Shooter, string, error) {
	e, note, err := newEngine(ctx, opts)
	return e, note, err
}

// NewRenderer returns an HTML-render engine for the stealth scraping path,
// picking the backend the same way NewShooter does.
func NewRenderer(ctx context.Context, opts SessionOptions) (Renderer, string, error) {
	e, note, err := newEngine(ctx, opts)
	return e, note, err
}

// pyShooter drives the Python Playwright stealth sidecar over stdin/stdout.
type pyShooter struct {
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	stdin      io.WriteCloser
	scriptPath string
	stderr     *ringBuffer
	lines      chan readResult // one entry per line the sidecar writes

	mu     sync.Mutex
	broken bool
	closed bool
}

type readResult struct {
	line []byte
	err  error
}

func newPyShooter(ctx context.Context, opts SessionOptions) (*pyShooter, error) {
	python := strings.TrimSpace(opts.PythonPath)
	if python == "" {
		python = "python"
	}

	f, err := os.CreateTemp("", "autoapply-stealth-*.py")
	if err != nil {
		return nil, err
	}
	scriptPath := f.Name()
	if _, err := f.WriteString(stealthSidecarPy); err != nil {
		f.Close()
		_ = os.Remove(scriptPath)
		return nil, err
	}
	f.Close()

	args := []string{scriptPath}
	if opts.Headful {
		args = append(args, "--headful")
	}

	procCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(procCtx, python, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		_ = os.Remove(scriptPath)
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = os.Remove(scriptPath)
		return nil, err
	}
	rb := &ringBuffer{max: 4096}
	cmd.Stderr = rb
	if err := cmd.Start(); err != nil {
		cancel()
		_ = os.Remove(scriptPath)
		return nil, fmt.Errorf("start %q: %w", python, err)
	}

	s := &pyShooter{cmd: cmd, cancel: cancel, stdin: stdin, scriptPath: scriptPath, stderr: rb, lines: make(chan readResult, 4)}

	// One owned reader goroutine turns the sidecar's stdout into lines. A 1 MiB
	// line buffer comfortably holds a JSON response carrying base64 screenshots.
	go func() {
		br := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := br.ReadBytes('\n')
			s.lines <- readResult{line: line, err: err}
			if err != nil {
				return
			}
		}
	}()

	// Readiness handshake: the sidecar prints {"ready":true} once the browser is
	// up (or {"error":...} if Python/Playwright/the package is missing).
	line, err := s.await(ctx, 75*time.Second)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("%v; stderr: %s", err, truncate(rb.String(), 300))
	}
	var hs struct {
		Ready bool   `json:"ready"`
		Error string `json:"error"`
	}
	if json.Unmarshal(line, &hs) != nil || !hs.Ready {
		s.Close()
		msg := firstNonBlank(hs.Error, strings.TrimSpace(string(line)), rb.String())
		return nil, fmt.Errorf("sidecar not ready: %s", truncate(msg, 300))
	}
	return s, nil
}

// await reads the next protocol line, bounded by timeout and ctx.
func (s *pyShooter) await(ctx context.Context, timeout time.Duration) ([]byte, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, fmt.Errorf("timed out after %s waiting for the stealth sidecar", timeout)
	case r := <-s.lines:
		if r.err != nil && len(r.line) == 0 {
			return nil, fmt.Errorf("stealth sidecar exited: %v", r.err)
		}
		return r.line, nil
	}
}

// roundtrip sends one request to the sidecar and returns its one response line,
// serialized so concurrent callers can't interleave the protocol. A missed
// response marks the sidecar broken (a desync risk) so it isn't reused.
func (s *pyShooter) roundtrip(ctx context.Context, payload map[string]any, timeout time.Duration) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken {
		return nil, fmt.Errorf("stealth sidecar is no longer running")
	}
	req, _ := json.Marshal(payload)
	if _, err := s.stdin.Write(append(req, '\n')); err != nil {
		s.broken = true
		return nil, fmt.Errorf("write to stealth sidecar: %w", err)
	}
	line, err := s.await(ctx, timeout)
	if err != nil {
		s.broken = true
		return nil, err
	}
	return line, nil
}

func (s *pyShooter) Shots(ctx context.Context, pageURL, cookieHeader, userAgent string, maxScreens int) (Shots, error) {
	if maxScreens <= 0 {
		maxScreens = 3
	}
	line, err := s.roundtrip(ctx, map[string]any{
		"url": pageURL, "cookie": cookieHeader, "ua": userAgent, "maxScreens": maxScreens,
	}, 100*time.Second)
	if err != nil {
		return Shots{}, err
	}
	var resp struct {
		Images []string `json:"images"`
		Links  []Link   `json:"links"`
		Title  string   `json:"title"`
		Text   string   `json:"text"`
		Error  string   `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return Shots{}, fmt.Errorf("bad stealth sidecar response: %w", err)
	}
	if resp.Error != "" {
		return Shots{}, fmt.Errorf("%s", resp.Error)
	}
	out := Shots{Links: resp.Links, Title: resp.Title, Text: resp.Text}
	for _, b64 := range resp.Images {
		if data, err := base64.StdEncoding.DecodeString(b64); err == nil && len(data) > 0 {
			out.Images = append(out.Images, data)
		}
	}
	return out, nil
}

// RenderHTML fetches a URL's fully-rendered page source through the stealth
// browser (replaying cookie + UA), for the HTML scraping sources to parse.
func (s *pyShooter) RenderHTML(ctx context.Context, pageURL, cookieHeader, userAgent string) (string, error) {
	line, err := s.roundtrip(ctx, map[string]any{
		"url": pageURL, "cookie": cookieHeader, "ua": userAgent, "mode": "html",
	}, 100*time.Second)
	if err != nil {
		return "", err
	}
	var resp struct {
		HTML  string `json:"html"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return "", fmt.Errorf("bad stealth sidecar response: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("%s", resp.Error)
	}
	return resp.HTML, nil
}

func (s *pyShooter) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	stdin := s.stdin
	s.mu.Unlock()

	if stdin != nil {
		_, _ = io.WriteString(stdin, `{"quit":true}`+"\n")
		_ = stdin.Close()
	}
	// Give the sidecar a moment to close its browser cleanly, then force-kill.
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		if s.cancel != nil {
			s.cancel()
		}
		<-done
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.scriptPath != "" {
		_ = os.Remove(s.scriptPath)
	}
}

// ringBuffer is a tiny bounded io.Writer that keeps the last max bytes — used to
// capture the sidecar's stderr for diagnostics without unbounded growth.
type ringBuffer struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.b = append(r.b, p...)
	if r.max > 0 && len(r.b) > r.max {
		r.b = r.b[len(r.b)-r.max:]
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.TrimSpace(string(r.b))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
