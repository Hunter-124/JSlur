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
	headers := map[string]string{}
	if o.cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.cfg.APIKey
	}

	endpoint := strings.TrimRight(o.cfg.BaseURL, "/") + "/chat/completions"
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
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
	return out.Choices[0].Message.Content, nil
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
