// Package ai abstracts over multiple LLM backends behind a single Provider
// interface. Anthropic, Google Gemini, DeepSeek and any OpenAI-compatible local
// server (Ollama, LM Studio, vLLM, ...) are all supported. Every backend is
// reached over plain HTTP so the package has no third-party dependencies.
package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"autoapply/internal/config"
)

// Request is a single text-generation request.
type Request struct {
	System      string
	Prompt      string
	MaxTokens   int
	Temperature float64
}

// Image is a raw image (e.g. a job-board screenshot) sent to a vision-capable
// model alongside a text prompt.
type Image struct {
	// Mime is the image media type, e.g. "image/png". Empty defaults to PNG.
	Mime string
	// Data is the raw, un-encoded image bytes (the backends base64-encode them).
	Data []byte
}

// Provider is implemented by every AI backend.
type Provider interface {
	// Name is a human-readable backend name.
	Name() string
	// Generate returns the model's text completion for the request.
	Generate(ctx context.Context, req Request) (string, error)
	// Vision returns the model's text completion for a prompt accompanied by one
	// or more images. It uses the same configured model as Generate, so the model
	// must be vision-capable; non-vision models return an error from the backend.
	Vision(ctx context.Context, req Request, images []Image) (string, error)
	// ListModels returns the model ids the configured account can use, for the
	// GUI's model picker.
	ListModels(ctx context.Context) ([]string, error)
}

// ListModels builds the requested provider (or the active one when providerID
// is empty) from cfg and returns its available model ids.
func ListModels(ctx context.Context, cfg config.AIConfig, providerID string) ([]string, error) {
	if providerID != "" {
		cfg.Active = providerID
	}
	p, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return p.ListModels(ctx)
}

// New builds the active Provider from configuration. It returns an error if the
// active provider is not configured (e.g. missing API key).
func New(cfg config.AIConfig) (Provider, error) {
	switch cfg.Active {
	case config.ProviderAnthropic:
		if cfg.Anthropic.APIKey == "" {
			return nil, fmt.Errorf("anthropic: API key not set")
		}
		return &anthropic{cfg: cfg.Anthropic}, nil
	case config.ProviderGoogle:
		if cfg.Google.APIKey == "" {
			return nil, fmt.Errorf("google: API key not set")
		}
		return &google{cfg: cfg.Google}, nil
	case config.ProviderDeepSeek:
		if cfg.DeepSeek.APIKey == "" {
			return nil, fmt.Errorf("deepseek: API key not set")
		}
		return &openAICompat{name: "DeepSeek", cfg: cfg.DeepSeek}, nil
	case config.ProviderLocal:
		if cfg.Local.BaseURL == "" {
			return nil, fmt.Errorf("local: base URL not set")
		}
		return &openAICompat{name: "Local", cfg: cfg.Local}, nil
	default:
		return nil, fmt.Errorf("unknown AI provider %q", cfg.Active)
	}
}

// httpClient is shared by all providers. Generation can be slow, especially for
// local models, so the timeout is generous.
var httpClient = &http.Client{Timeout: 180 * time.Second}

// postJSON marshals body, POSTs it to url with the given headers and decodes the
// JSON response into out. Non-2xx responses become errors carrying the body.
func postJSON(ctx context.Context, url string, headers map[string]string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// getJSON performs a GET with the given headers and decodes the JSON response.
func getJSON(ctx context.Context, url string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

func clampTokens(n int) int {
	if n <= 0 {
		return 4096
	}
	return n
}

// b64 base64-encodes raw image bytes for inclusion in a JSON request body.
func b64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// orMime returns the image media type, defaulting to PNG when unset.
func orMime(m string) string {
	if m == "" {
		return "image/png"
	}
	return m
}
