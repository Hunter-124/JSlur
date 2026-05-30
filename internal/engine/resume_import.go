package engine

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ledongthuc/pdf"

	"autoapply/internal/ai"
	"autoapply/internal/resume"
)

// ResumeImport summarises what importing a résumé did, for the GUI.
type ResumeImport struct {
	Parsed   bool     `json:"parsed"`
	Headline string   `json:"headline"`
	Skills   []string `json:"skills"`
	Roles    []string `json:"roles"`
	Chars    int      `json:"chars"`
	Note     string   `json:"note"`
}

// ImportResume extracts text from an uploaded résumé (txt/md/pdf/docx), stores it
// as the base résumé, and — when an AI provider is configured — parses it into a
// profile and target roles, merging the results into the configuration. This is
// the heart of the simple "upload résumé, let the AI take it from here" flow.
func (e *Engine) ImportResume(ctx context.Context, filename, rawText string, data []byte) (ResumeImport, error) {
	text := strings.TrimSpace(rawText)
	if text == "" {
		var err error
		if text, err = extractText(filename, data); err != nil {
			return ResumeImport{}, err
		}
		text = strings.TrimSpace(text)
	}
	if text == "" {
		return ResumeImport{}, fmt.Errorf("could not extract any text from the résumé")
	}

	cfg := e.cfg.Get()
	cfg.Candidate.BaseResume = text
	out := ResumeImport{Chars: len(text)}

	provider, perr := ai.New(cfg.AI)
	if perr != nil {
		_ = e.cfg.Set(cfg)
		out.Note = "Saved your résumé. Add an AI provider in Settings to auto-fill your profile and target roles."
		e.logf("info", "imported résumé (%d chars); no AI configured to parse it yet", len(text))
		e.refresh()
		return out, nil
	}

	pp, err := resume.ParseProfile(ctx, provider, text)
	if err != nil {
		_ = e.cfg.Set(cfg)
		out.Note = "Saved your résumé, but couldn't auto-parse it: " + err.Error()
		e.logf("warn", "résumé parse failed: %v", err)
		e.refresh()
		return out, nil
	}

	c := &cfg.Candidate
	// Fill contact details only if not already set (don't clobber user input).
	if c.Name == "" {
		c.Name = pp.Name
	}
	if c.Email == "" {
		c.Email = pp.Email
	}
	if c.Phone == "" {
		c.Phone = pp.Phone
	}
	if c.Location == "" {
		c.Location = pp.Location
	}
	// Derived fields: refresh from the résumé.
	if pp.Headline != "" {
		c.Headline = pp.Headline
	}
	if pp.Summary != "" {
		c.Summary = pp.Summary
	}
	if len(pp.Skills) > 0 {
		c.Skills = pp.Skills
	}
	// Importing a résumé means "figure out what to search for", so the parsed
	// roles drive the multi-role search interest.
	if len(pp.Roles) > 0 {
		cfg.Focus.Interest = strings.Join(pp.Roles, " / ")
	}
	if err := e.cfg.Set(cfg); err != nil {
		return out, err
	}

	out.Parsed = true
	out.Headline = c.Headline
	out.Skills = c.Skills
	out.Roles = pp.Roles
	out.Note = "Parsed your résumé and set up your profile and target roles."
	e.logf("success", "imported résumé — %s; roles → %s", orPlaceholder(c.Headline), strings.Join(pp.Roles, ", "))
	e.refresh()
	return out, nil
}

// extractText pulls plain text out of a résumé file by extension.
func extractText(filename string, data []byte) (string, error) {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".pdf":
		return pdfText(data)
	case ".docx":
		return docxText(data)
	default: // .txt, .md, or unknown → treat as UTF-8 text
		return string(data), nil
	}
}

func pdfText(data []byte) (string, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("read pdf: %w", err)
	}
	b, err := r.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("extract pdf text: %w", err)
	}
	var buf strings.Builder
	if _, err := io.Copy(&buf, b); err != nil {
		return "", err
	}
	return buf.String(), nil
}

var xmlTagRe = regexp.MustCompile(`<[^>]+>`)

func docxText(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("read docx: %w", err)
	}
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		raw, _ := io.ReadAll(rc)
		rc.Close()
		s := string(raw)
		s = strings.ReplaceAll(s, "</w:p>", "\n")
		s = strings.ReplaceAll(s, "</w:tr>", "\n")
		s = xmlTagRe.ReplaceAllString(s, "")
		s = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'").Replace(s)
		return s, nil
	}
	return "", fmt.Errorf("not a valid .docx (no word/document.xml)")
}
