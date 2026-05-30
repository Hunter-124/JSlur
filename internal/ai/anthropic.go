package ai

import (
	"context"
	"fmt"

	"autoapply/internal/config"
)

// anthropic talks to the Anthropic Messages API.
type anthropic struct {
	cfg config.ProviderConfig
}

func (a *anthropic) Name() string { return "Anthropic (" + a.cfg.Model + ")" }

func (a *anthropic) Generate(ctx context.Context, req Request) (string, error) {
	return a.send(ctx, req, []map[string]any{{"role": "user", "content": req.Prompt}})
}

func (a *anthropic) Vision(ctx context.Context, req Request, images []Image) (string, error) {
	content := make([]map[string]any, 0, len(images)+1)
	for _, img := range images {
		content = append(content, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": orMime(img.Mime),
				"data":       b64(img.Data),
			},
		})
	}
	content = append(content, map[string]any{"type": "text", "text": req.Prompt})
	return a.send(ctx, req, []map[string]any{{"role": "user", "content": content}})
}

// send posts a Messages request with the given message list and returns the
// concatenated text. Shared by Generate (text) and Vision (text + images).
func (a *anthropic) send(ctx context.Context, req Request, messages []map[string]any) (string, error) {
	body := map[string]any{
		"model":       a.cfg.Model,
		"max_tokens":  clampTokens(orDefaultMax(a.cfg.MaxTokens, req.MaxTokens)),
		"temperature": pickTemp(a.cfg.Temperature, req.Temperature),
		"messages":    messages,
	}
	if req.System != "" {
		body["system"] = req.System
	}
	headers := map[string]string{
		"x-api-key":         a.cfg.APIKey,
		"anthropic-version": "2023-06-01",
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := postJSON(ctx, "https://api.anthropic.com/v1/messages", headers, body, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("anthropic: %s", out.Error.Message)
	}
	var sb string
	for _, c := range out.Content {
		if c.Type == "text" {
			sb += c.Text
		}
	}
	if sb == "" {
		return "", fmt.Errorf("anthropic: empty response")
	}
	return sb, nil
}

func (a *anthropic) ListModels(ctx context.Context) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	headers := map[string]string{"x-api-key": a.cfg.APIKey, "anthropic-version": "2023-06-01"}
	if err := getJSON(ctx, "https://api.anthropic.com/v1/models?limit=100", headers, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("anthropic: %s", out.Error.Message)
	}
	var ids []string
	for _, d := range out.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	return ids, nil
}
