package ai

import (
	"context"
	"fmt"
	"strings"

	"autoapply/internal/config"
)

// openAICompat talks to any service exposing the OpenAI Chat Completions API.
// This covers DeepSeek as well as local servers such as Ollama, LM Studio and
// vLLM — they differ only in base URL, model name and (optional) API key.
type openAICompat struct {
	name string
	cfg  config.ProviderConfig
}

func (o *openAICompat) Name() string { return o.name + " (" + o.cfg.Model + ")" }

func (o *openAICompat) Generate(ctx context.Context, req Request) (string, error) {
	return o.send(ctx, req, req.Prompt)
}

func (o *openAICompat) Vision(ctx context.Context, req Request, images []Image) (string, error) {
	// OpenAI-compatible vision uses a content array: a text part plus one
	// image_url part per image (a base64 data URL). Vision-capable local models
	// (llava, qwen-vl, …) accept the same shape.
	content := make([]map[string]any, 0, len(images)+1)
	content = append(content, map[string]any{"type": "text", "text": req.Prompt})
	for _, img := range images {
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + orMime(img.Mime) + ";base64," + b64(img.Data)},
		})
	}
	return o.send(ctx, req, content)
}

// send posts a Chat Completions request whose user message has the given content
// (a plain string for text, or a content-part array for vision) and returns the
// reply text.
func (o *openAICompat) send(ctx context.Context, req Request, userContent any) (string, error) {
	messages := []map[string]any{}
	if req.System != "" {
		messages = append(messages, map[string]any{"role": "system", "content": req.System})
	}
	messages = append(messages, map[string]any{"role": "user", "content": userContent})

	body := map[string]any{
		"model":       o.cfg.Model,
		"messages":    messages,
		"max_tokens":  clampTokens(orDefaultMax(o.cfg.MaxTokens, req.MaxTokens)),
		"temperature": pickTemp(o.cfg.Temperature, req.Temperature),
		"stream":      false,
	}
	// A "thinking" model emits hidden reasoning that still counts against
	// max_tokens; left unbounded it can swallow the entire budget and leave the
	// actual answer empty. When the user sets a reasoning effort we forward it so
	// the server can cap or disable that phase. Only sent when set, so servers
	// that don't understand the field (Ollama, llama.cpp, …) are unaffected.
	if eff := strings.TrimSpace(o.cfg.ReasoningEffort); eff != "" {
		body["reasoning_effort"] = eff
	}
	headers := map[string]string{}
	if o.cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.cfg.APIKey
	}

	endpoint := strings.TrimRight(o.cfg.BaseURL, "/") + "/chat/completions"
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				// reasoning_content is where thinking models (and LM Studio) park the
				// chain-of-thought, separate from the answer in content.
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := postJSON(ctx, endpoint, headers, body, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("%s: %s", strings.ToLower(o.name), out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("%s: empty response", strings.ToLower(o.name))
	}

	choice := out.Choices[0]
	// Some models inline reasoning as <think>…</think> in the content itself
	// rather than the separate field; drop it so callers get only the answer.
	if content := stripThink(choice.Message.Content); content != "" {
		return content, nil
	}

	// Empty answer. With a reasoning model this almost always means the hidden
	// thinking used up the whole token budget before any answer was produced.
	// Turn that into an actionable error instead of a silent empty string.
	if choice.FinishReason == "length" {
		fix := "increase Max tokens"
		if strings.TrimSpace(o.cfg.ReasoningEffort) == "" {
			fix = `set Reasoning effort to "none", or raise Max tokens,`
		}
		return "", fmt.Errorf("%s: the model spent its entire token budget on hidden reasoning and returned no answer — %s in Settings", strings.ToLower(o.name), fix)
	}
	if strings.TrimSpace(choice.Message.ReasoningContent) != "" {
		return "", fmt.Errorf(`%s: the model returned only hidden reasoning and no answer — set Reasoning effort to "none" in Settings`, strings.ToLower(o.name))
	}
	return "", fmt.Errorf("%s: empty response", strings.ToLower(o.name))
}

// stripThink removes any <think>…</think> reasoning blocks a model inlines into
// its content and trims the remainder. An unclosed (truncated) block drops
// everything from the opening tag on. Content without such tags is returned
// trimmed and unchanged.
func stripThink(s string) string {
	for {
		open := strings.Index(s, "<think>")
		if open < 0 {
			break
		}
		rest := s[open+len("<think>"):]
		end := strings.Index(rest, "</think>")
		if end < 0 {
			s = s[:open]
			break
		}
		s = s[:open] + rest[end+len("</think>"):]
	}
	return strings.TrimSpace(s)
}

func (o *openAICompat) ListModels(ctx context.Context) ([]string, error) {
	endpoint := strings.TrimRight(o.cfg.BaseURL, "/") + "/models"
	headers := map[string]string{}
	if o.cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.cfg.APIKey
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := getJSON(ctx, endpoint, headers, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s: %s", strings.ToLower(o.name), out.Error.Message)
	}
	var ids []string
	for _, d := range out.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	return ids, nil
}
