package resume

import (
	"context"
	"strings"
	"testing"

	"autoapply/internal/ai"
	"autoapply/internal/config"
	"autoapply/internal/store"
)

// fakeProvider records the last request it received and returns a canned
// response, so we can assert which prompt the resume package sent.
type fakeProvider struct {
	lastReq ai.Request
	resp    string
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Generate(_ context.Context, req ai.Request) (string, error) {
	f.lastReq = req
	return f.resp, nil
}
func (f *fakeProvider) Vision(_ context.Context, req ai.Request, _ []ai.Image) (string, error) {
	f.lastReq = req
	return f.resp, nil
}
func (f *fakeProvider) ListModels(_ context.Context) ([]string, error) { return nil, nil }

// Reach mode must use the aggressive system prompt (not the conservative
// default) and surface the model's "stretches" list for the human reviewer.
func TestGenerateReachModeUsesAggressivePromptAndParsesStretches(t *testing.T) {
	f := &fakeProvider{resp: `{"match_score":40,"match_reason":"a stretch","strengths":["transferable skills"],"gaps":["no degree"],"stretches":["framed familiarity as proficiency"],"resume":"# Resume","cover_letter":"Dear team"}`}

	res, err := Generate(context.Background(), f, config.Candidate{Name: "X"}, store.Job{Title: "Senior Engineer"}, Options{Reach: true})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(res.Stretches) != 1 || res.Stretches[0] == "" {
		t.Fatalf("expected the stretches list to be parsed, got %#v", res.Stretches)
	}
	if res.MatchScore != 40 {
		t.Fatalf("honest match score should pass through unchanged, got %d", res.MatchScore)
	}
	if !strings.Contains(f.lastReq.System, "aggressive job-application strategist") {
		t.Fatalf("reach mode should use the aggressive system prompt, got: %q", f.lastReq.System)
	}
	if strings.Contains(f.lastReq.System, "expert career coach") {
		t.Fatalf("reach mode must NOT use the conservative default system prompt")
	}
	// The truthfulness floor must remain even in reach mode.
	if !strings.Contains(f.lastReq.System, "NEVER") || !strings.Contains(f.lastReq.System, "background check") {
		t.Fatalf("reach prompt is missing the truthfulness floor: %q", f.lastReq.System)
	}
	if !strings.Contains(f.lastReq.Prompt, "REACH MODE") {
		t.Fatalf("reach task instruction missing from the user prompt")
	}
}

// The default (non-reach) path must keep the truthful, even-handed prompt.
func TestGenerateDefaultUsesTruthfulPrompt(t *testing.T) {
	f := &fakeProvider{resp: `{"match_score":80,"match_reason":"good","strengths":["a"],"gaps":[],"resume":"# R","cover_letter":"hi"}`}

	if _, err := Generate(context.Background(), f, config.Candidate{}, store.Job{Title: "Engineer"}, Options{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(f.lastReq.System, "Be truthful") {
		t.Fatalf("default should use the truthful system prompt, got: %q", f.lastReq.System)
	}
	if strings.Contains(f.lastReq.Prompt, "REACH MODE") {
		t.Fatalf("default task should not mention REACH MODE")
	}
}

// In reach mode the stage-2 filter scores desire (what the candidate is after),
// not readiness, so wanted-but-stretch roles survive instead of being filtered.
func TestPrescreenBatchReachScoresDesireNotReadiness(t *testing.T) {
	jobsList := []store.Job{{ID: "1", Title: "Senior Engineer", Company: "ACME"}}

	reach := &fakeProvider{resp: `[{"i":0,"score":90,"reason":"matches target field"}]`}
	out, err := PrescreenBatch(context.Background(), reach, config.Candidate{}, "engineering roles", jobsList, true)
	if err != nil {
		t.Fatalf("PrescreenBatch reach: %v", err)
	}
	if out["1"].Score != 90 {
		t.Fatalf("expected the kept score 90, got %d", out["1"].Score)
	}
	if !strings.Contains(reach.lastReq.Prompt, "LOOKING FOR") {
		t.Fatalf("reach prescreen should score what the candidate is looking for, got: %q", reach.lastReq.Prompt)
	}

	normal := &fakeProvider{resp: `[{"i":0,"score":30,"reason":"underqualified"}]`}
	if _, err := PrescreenBatch(context.Background(), normal, config.Candidate{}, "engineering roles", jobsList, false); err != nil {
		t.Fatalf("PrescreenBatch normal: %v", err)
	}
	if strings.Contains(normal.lastReq.Prompt, "LOOKING FOR") {
		t.Fatalf("default prescreen should score honest fit, not desire")
	}
	if !strings.Contains(normal.lastReq.Prompt, "honest") {
		t.Fatalf("default prescreen should ask for an honest fit score")
	}
}
