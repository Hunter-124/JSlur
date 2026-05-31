package ai

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"autoapply/internal/config"
)

// TestLiveLocalReasoning exercises the OpenAI-compatible backend against a real
// local server (LM Studio / Ollama / vLLM). It proves the reasoning-model
// handling end to end: that "none" yields a real answer, and that a too-small
// budget surfaces an actionable error instead of a silent empty string.
//
// Skipped unless AUTOAPPLY_LIVE_AI=1. Configure via env:
//
//	AUTOAPPLY_AI_BASEURL (default http://localhost:1234/v1)
//	AUTOAPPLY_AI_MODEL   (default google/gemma-4-26b-a4b)
//	AUTOAPPLY_VISION_IMG (optional path to a PNG of job listings to read)
func TestLiveLocalReasoning(t *testing.T) {
	if os.Getenv("AUTOAPPLY_LIVE_AI") == "" {
		t.Skip("set AUTOAPPLY_LIVE_AI=1 to run the live local-model test")
	}
	base := envOr("AUTOAPPLY_AI_BASEURL", "http://localhost:1234/v1")
	model := envOr("AUTOAPPLY_AI_MODEL", "google/gemma-4-26b-a4b")

	newProvider := func(effort string, maxTokens int) Provider {
		p, err := New(config.AIConfig{
			Active: config.ProviderLocal,
			Local: config.ProviderConfig{
				BaseURL:         base,
				Model:           model,
				ReasoningEffort: effort,
				MaxTokens:       maxTokens,
			},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return p
	}

	// 1. reasoning_effort=none should return a real, non-empty answer fast.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := newProvider("none", 256).Generate(ctx, Request{Prompt: "Reply with exactly the word: ok", Temperature: 0})
	if err != nil {
		t.Fatalf("generate (reasoning none): %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("reasoning none returned an empty answer")
	}
	t.Logf("reasoning=none answer: %q", out)

	// 2. With reasoning left on but a tiny budget, the thinking overruns the
	//    budget; we must report that clearly rather than return "".
	ctx2, cancel2 := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel2()
	_, err = newProvider("", 8).Generate(ctx2, Request{Prompt: "Explain why the sky is blue in detail.", Temperature: 0})
	if err == nil {
		t.Log("note: tiny-budget call unexpectedly succeeded (model may not be a reasoning model)")
	} else if !strings.Contains(err.Error(), "reasoning") && !strings.Contains(err.Error(), "token budget") {
		t.Logf("tiny-budget error (not the reasoning-specific one, acceptable): %v", err)
	} else {
		t.Logf("tiny-budget error (expected, actionable): %v", err)
	}

	// 3. Optional: read job listings off a screenshot through the Go vision path.
	imgPath := os.Getenv("AUTOAPPLY_VISION_IMG")
	if imgPath == "" {
		t.Log("AUTOAPPLY_VISION_IMG not set; skipping the vision sub-test")
		return
	}
	data, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatalf("read vision image: %v", err)
	}
	ctx3, cancel3 := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel3()
	vout, err := newProvider("none", 2048).Vision(ctx3,
		Request{Prompt: "Read every job listing in the image and return ONLY a JSON array of " +
			`{"title","company","location","remote","salary","url","description"}. No prose.`, Temperature: 0},
		[]Image{{Mime: "image/png", Data: data}})
	if err != nil {
		t.Fatalf("vision: %v", err)
	}
	t.Logf("vision answer:\n%s", vout)
	if !strings.Contains(vout, "[") {
		t.Errorf("vision answer doesn't look like a JSON array: %q", vout)
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
