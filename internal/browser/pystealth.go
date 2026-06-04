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
	"path/filepath"
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

// Connector captures a signed-in board session by opening its login page in a
// real, headful stealth browser and waiting for the user to sign in. Unlike the
// built-in chromedp capture, it drives the user's actual installed browser
// (Chrome/Edge) through pw-stealth-enhanced, so SSO providers accept it
// (no "this browser is insecure") and a Cloudflare clearance is issued to a
// fingerprint the scrapers can reproduce.
type Connector interface {
	// Connect returns the captured Cookie header and the browser's User-Agent.
	Connect(ctx context.Context, loginURL string, authCookies []string, timeout time.Duration) (cookieHeader, userAgent string, err error)
	Close()
}

// NewConnector builds the stealth connect-account browser. It always uses the
// Python pw-stealth-enhanced sidecar (the entire point is a real, undetectable
// headful browser) and forces headful so the user can sign in. Returns an error
// when the sidecar can't start, so the caller can fall back to chromedp.Capture.
func NewConnector(ctx context.Context, opts SessionOptions) (Connector, error) {
	opts.Headful = true // the user must see the page to sign in
	opts.OnBlock = nil  // the connect flow has its own wait loop; no block prompts
	return newPyShooter(ctx, opts)
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

// pyShooter drives the Python Playwright stealth sidecar over stdin/stdout. The
// protocol is multiplexed: each request carries an "id" and the sidecar runs
// several requests concurrently (one tab each), echoing the id on each response.
// A single reader goroutine routes responses to the waiting caller by id, so
// many Shots/RenderHTML calls can be in flight at once.
type pyShooter struct {
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	stdin      io.WriteCloser
	scriptPath string
	stderr     *ringBuffer
	notify     func(level, msg string)
	onBlock    func(ctx context.Context, host, kind, reason string) bool
	handshake  chan readResult // unkeyed startup lines (ready / fatal error)

	writeMu sync.Mutex // serializes stdin writes (whole lines)

	mu      sync.Mutex
	nextID  int
	pending map[int]*waiter // id -> waiter
	broken  bool
	closed  bool
}

// waiter holds the response channel for one in-flight request plus an optional
// handler for the interim "event" lines (e.g. an interactive block prompt) the
// sidecar may emit before the final response.
type waiter struct {
	resp    chan readResult
	onEvent func(line []byte)
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
	// A persistent profile dir is what lets a solved Cloudflare challenge and any
	// sign-in survive across requests and runs (no re-login). Empty => ephemeral.
	if dir := strings.TrimSpace(opts.ProfileDir); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			// Clear stale Chromium singleton locks left by a crashed/killed sidecar
			// (no clean Close). Without this, launch_persistent_context would fail and
			// the whole stealth engine would silently fall back to the built-in
			// browser. Sidecars on a given profile run one at a time, so no live
			// browser owns these.
			for _, name := range []string{"SingletonLock", "SingletonCookie", "SingletonSocket"} {
				_ = os.Remove(filepath.Join(dir, name))
			}
			args = append(args, "--profile", dir)
		}
	}
	concurrency := opts.MaxConcurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	args = append(args, "--concurrency", fmt.Sprintf("%d", concurrency))

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

	s := &pyShooter{
		cmd: cmd, cancel: cancel, stdin: stdin, scriptPath: scriptPath, stderr: rb,
		notify:    opts.Notify,
		onBlock:   opts.OnBlock,
		handshake: make(chan readResult, 1),
		pending:   make(map[int]*waiter),
	}

	// One owned reader goroutine turns the sidecar's stdout into lines and routes
	// each to its waiter by id. A 1 MiB line buffer comfortably holds a JSON
	// response carrying base64 screenshots.
	go func() {
		br := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				s.route(line)
			}
			if err != nil {
				s.failAll(err)
				return
			}
		}
	}()

	// Readiness handshake: the sidecar prints {"ready":true} once the browser is
	// up (or {"error":...} if Python/Playwright/the package is missing).
	line, err := s.awaitHandshake(ctx, 75*time.Second)
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

// route dispatches one sidecar line: out-of-band notes go to the Notify hook,
// id-tagged responses go to the matching waiter, and unkeyed lines (the startup
// handshake / a fatal startup error) go to the handshake channel.
func (s *pyShooter) route(line []byte) {
	var msg struct {
		ID    *int    `json:"id"`
		Note  *string `json:"note"`
		Level string  `json:"level"`
		Event string  `json:"event"`
	}
	_ = json.Unmarshal(line, &msg)
	if msg.Note != nil {
		if s.notify != nil {
			level := msg.Level
			if level == "" {
				level = "info"
			}
			s.notify(level, *msg.Note)
		}
		return
	}
	if msg.ID != nil {
		s.mu.Lock()
		w := s.pending[*msg.ID]
		s.mu.Unlock()
		if w == nil {
			return
		}
		// An interim "event" line (e.g. a block prompt) goes to the request's event
		// handler, which must return quickly (it dispatches its own goroutine). The
		// final response goes to the response channel.
		if msg.Event != "" {
			if w.onEvent != nil {
				w.onEvent(line)
			}
			return
		}
		// Buffered (cap 1); non-blocking so a stray duplicate response can never
		// stall the reader. The first (expected) response always fits.
		select {
		case w.resp <- readResult{line: line}:
		default:
		}
		return
	}
	select {
	case s.handshake <- readResult{line: line}:
	default:
	}
}

// failAll marks the sidecar broken and unblocks every waiter (and the handshake)
// with the read error, so no caller hangs after the sidecar exits.
func (s *pyShooter) failAll(err error) {
	s.mu.Lock()
	s.broken = true
	pending := s.pending
	s.pending = make(map[int]*waiter)
	s.mu.Unlock()
	for _, w := range pending {
		// Non-blocking: the waiter's channel is buffered (cap 1); if it's already
		// full or the waiter gave up, we must not block the reader during shutdown.
		select {
		case w.resp <- readResult{err: err}:
		default:
		}
	}
	select {
	case s.handshake <- readResult{err: err}:
	default:
	}
}

// awaitHandshake waits for the startup line, bounded by timeout and ctx.
func (s *pyShooter) awaitHandshake(ctx context.Context, timeout time.Duration) ([]byte, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, fmt.Errorf("timed out after %s waiting for the stealth sidecar", timeout)
	case r := <-s.handshake:
		if r.err != nil {
			return nil, fmt.Errorf("stealth sidecar exited: %v", r.err)
		}
		return r.line, nil
	}
}

// roundtrip sends one request to the sidecar and returns its response line,
// matched by id. Several roundtrips can be in flight at once; a per-request
// timeout no longer breaks the shared sidecar (a late response simply has no
// waiter and is dropped). When an interactive block handler (onBlock) is wired,
// the request is flagged interactive, the timeout is stretched to allow a human
// sign-in, and interim block-prompt events are dispatched to onBlock.
func (s *pyShooter) roundtrip(ctx context.Context, payload map[string]any, timeout time.Duration) ([]byte, error) {
	interactive := s.onBlock != nil
	if interactive {
		payload["interactive"] = true
		if timeout < 17*time.Minute {
			timeout = 17 * time.Minute // room for a human to sign in / solve a captcha
		}
	}

	s.mu.Lock()
	if s.broken {
		s.mu.Unlock()
		return nil, fmt.Errorf("stealth sidecar is no longer running")
	}
	s.nextID++
	id := s.nextID
	w := &waiter{resp: make(chan readResult, 1)}
	s.pending[id] = w
	s.mu.Unlock()

	// Tie any interactive block prompt to this request's lifetime: when the call
	// returns (response arrived, timed out or cancelled), promptCtx is cancelled so
	// a still-open prompt is taken down (e.g. the user signed in and the sidecar
	// auto-continued before they clicked anything).
	promptCtx, cancelPrompt := context.WithCancel(ctx)
	defer cancelPrompt()
	if interactive {
		var once sync.Once
		w.onEvent = func(line []byte) {
			var ev struct {
				Event, Kind, Reason, Host string
			}
			_ = json.Unmarshal(line, &ev)
			if ev.Event != "attention" {
				return
			}
			// Dispatch off the reader goroutine — onBlock blocks until the user acts.
			once.Do(func() {
				go func() {
					action := "skip"
					if s.onBlock(promptCtx, ev.Host, ev.Kind, ev.Reason) {
						action = "resume"
					}
					s.writeControl(id, action)
				}()
			})
		}
	}

	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	payload["id"] = id
	req, _ := json.Marshal(payload)
	s.writeMu.Lock()
	_, werr := s.stdin.Write(append(req, '\n'))
	s.writeMu.Unlock()
	if werr != nil {
		s.mu.Lock()
		s.broken = true
		s.mu.Unlock()
		return nil, fmt.Errorf("write to stealth sidecar: %w", werr)
	}

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, fmt.Errorf("timed out after %s waiting for the stealth sidecar", timeout)
	case r := <-w.resp:
		if r.err != nil {
			return nil, r.err
		}
		return r.line, nil
	}
}

// markBroken flags the sidecar unusable so later roundtrips fail immediately.
func (s *pyShooter) markBroken() {
	s.mu.Lock()
	s.broken = true
	s.mu.Unlock()
}

// mentionsClosedBrowser reports whether a sidecar error string indicates the
// shared browser/context/page is gone (crash or the window was closed) — the
// case that otherwise makes every concurrent scraper fail with a cryptic
// "Target page, context or browser has been closed".
func mentionsClosedBrowser(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "has been closed") || strings.Contains(m, "browser has been closed") ||
		strings.Contains(m, "target page, context or browser") || strings.Contains(m, "browser.close")
}

// writeControl sends a mid-request control answer (resume/skip) for an
// interactive block prompt back to the sidecar.
func (s *pyShooter) writeControl(id int, action string) {
	req, _ := json.Marshal(map[string]any{"id": id, "control": action})
	s.writeMu.Lock()
	_, _ = s.stdin.Write(append(req, '\n'))
	s.writeMu.Unlock()
}

// Connect drives the connect-account capture: it tells the sidecar to open
// loginURL in the headful stealth window and waits (up to timeout) for the user
// to sign in — finishing as soon as any of authCookies appears, or when the user
// closes the window (authCookies empty). It returns the captured Cookie header
// for the board's site plus the browser's User-Agent.
func (s *pyShooter) Connect(ctx context.Context, loginURL string, authCookies []string, timeout time.Duration) (string, string, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if authCookies == nil {
		authCookies = []string{} // marshal as [] not null, so the sidecar's "wait for close" path is unambiguous
	}
	// The sidecar blocks for up to `timeout` while the user signs in; give the
	// roundtrip a little headroom on top so it doesn't time out the live capture.
	line, err := s.roundtrip(ctx, map[string]any{
		"mode":        "connect",
		"url":         loginURL,
		"authCookies": authCookies,
		"timeout":     int(timeout / time.Second),
	}, timeout+30*time.Second)
	if err != nil {
		return "", "", err
	}
	var resp struct {
		Cookies string `json:"cookies"`
		UA      string `json:"ua"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return "", "", fmt.Errorf("bad stealth sidecar response: %w", err)
	}
	if resp.Error != "" {
		return "", "", fmt.Errorf("%s", resp.Error)
	}
	return resp.Cookies, resp.UA, nil
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
		Images      []string `json:"images"`
		Links       []Link   `json:"links"`
		Title       string   `json:"title"`
		Text        string   `json:"text"`
		Blocked     string   `json:"blocked"`
		BlockReason string   `json:"blockReason"`
		Error       string   `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return Shots{}, fmt.Errorf("bad stealth sidecar response: %w", err)
	}
	if resp.Error != "" {
		if mentionsClosedBrowser(resp.Error) {
			s.markBroken()
			return Shots{}, fmt.Errorf("the stealth browser was closed (it may have been closed manually or crashed) — rerun the search")
		}
		return Shots{}, fmt.Errorf("%s", resp.Error)
	}
	out := Shots{Links: resp.Links, Title: resp.Title, Text: resp.Text, Blocked: resp.Blocked, BlockReason: resp.BlockReason}
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
		HTML        string `json:"html"`
		Title       string `json:"title"`
		Blocked     string `json:"blocked"`
		BlockReason string `json:"blockReason"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return "", fmt.Errorf("bad stealth sidecar response: %w", err)
	}
	if resp.Error != "" {
		// A "context/page/browser has been closed" error means the shared browser
		// died (crashed or the window was closed) — mark the sidecar broken so the
		// other in-flight scrapers fail fast with a clear reason instead of each
		// stalling against a dead browser.
		if mentionsClosedBrowser(resp.Error) {
			s.markBroken()
			return "", fmt.Errorf("the stealth browser was closed (it may have been closed manually or crashed) — rerun the search")
		}
		return "", fmt.Errorf("%s", resp.Error)
	}
	if resp.Blocked != "" {
		return resp.HTML, &BlockedError{Host: hostOnly(pageURL), Kind: resp.Blocked, Reason: resp.BlockReason}
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
