package ai

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"autoapply/internal/config"
)

// google talks to the Gemini generateContent API.
type google struct {
	cfg config.ProviderConfig
}

func (g *google) Name() string { return "Google Gemini (" + g.cfg.Model + ")" }

func (g *google) Generate(ctx context.Context, req Request) (string, error) {
	return g.send(ctx, req, []map[string]any{{"text": req.Prompt}})
}

func (g *google) Vision(ctx context.Context, req Request, images []Image) (string, error) {
	parts := make([]map[string]any, 0, len(images)+1)
	parts = append(parts, map[string]any{"text": req.Prompt})
	for _, img := range images {
		parts = append(parts, map[string]any{
			"inline_data": map[string]any{"mime_type": orMime(img.Mime), "data": b64(img.Data)},
		})
	}
	return g.send(ctx, req, parts)
}

// send posts a generateContent request with the given content parts and returns
// the concatenated text. Shared by Generate (text) and Vision (text + images).
func (g *google) send(ctx context.Context, req Request, parts []map[string]any) (string, error) {
	endpoint := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
		url.PathEscape(g.cfg.Model), url.QueryEscape(g.cfg.APIKey),
	)
	body := map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": parts},
		},
		"generationConfig": map[string]any{
			"maxOutputTokens": clampTokens(orDefaultMax(g.cfg.MaxTokens, req.MaxTokens)),
			"temperature":     pickTemp(g.cfg.Temperature, req.Temperature),
		},
	}
	if req.System != "" {
		body["system_instruction"] = map[string]any{
			"parts": []map[string]any{{"text": req.System}},
		}
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := postJSON(ctx, endpoint, nil, body, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("google: %s", out.Error.Message)
	}
	if len(out.Candidates) == 0 {
		return "", fmt.Errorf("google: no candidates returned")
	}
	var sb string
	for _, p := range out.Candidates[0].Content.Parts {
		sb += p.Text
	}
	if sb == "" {
		return "", fmt.Errorf("google: empty response")
	}
	return sb, nil
}

func (g *google) ListModels(ctx context.Context) ([]string, error) {
	u := "https://generativelanguage.googleapis.com/v1beta/models?pageSize=200&key=" + url.QueryEscape(g.cfg.APIKey)
	var out struct {
		Models []struct {
			Name                       string   `json:"name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := getJSON(ctx, u, nil, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("google: %s", out.Error.Message)
	}
	var ids []string
	for _, m := range out.Models {
		// Keep only models usable for text generation.
		ok := len(m.SupportedGenerationMethods) == 0
		for _, s := range m.SupportedGenerationMethods {
			if s == "generateContent" {
				ok = true
			}
		}
		if ok {
			ids = append(ids, strings.TrimPrefix(m.Name, "models/"))
		}
	}
	return ids, nil
}
