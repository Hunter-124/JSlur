// Package resume turns a candidate profile plus a job posting into tailored
// application materials, and also hosts the AI helper that maps a user's
// free-text interest onto a job board's category taxonomy.
package resume

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"autoapply/internal/ai"
	"autoapply/internal/config"
	"autoapply/internal/store"
)

// Result is the AI's tailored output for one job.
type Result struct {
	MatchScore  int      `json:"match_score"`
	MatchReason string   `json:"match_reason"`
	Strengths   []string `json:"strengths"`
	Gaps        []string `json:"gaps"`
	Resume      string   `json:"resume"`
	CoverLetter string   `json:"cover_letter"`
}

// Options tunes a generation run.
type Options struct {
	// Interest is the user's free-text description of what they're after; it
	// gives the model extra context about intent.
	Interest string
	// Instructions, when set, asks the model to revise the previous materials
	// (e.g. "make it more concise", "emphasize leadership").
	Instructions string
	// Previous holds the materials being refined (used only with Instructions).
	PreviousResume string
	PreviousCover  string
}

const systemPrompt = `You are an expert career coach and professional resume writer.
You tailor a candidate's existing resume to a specific job and write a matching cover letter.

Strict rules:
- Be truthful. NEVER invent employers, titles, dates, degrees, certifications or
  skills the candidate does not have. You may rephrase, reorder and emphasise the
  candidate's real experience to highlight relevance, but you must not fabricate.
- Mirror the language and keywords of the job description where the candidate
  genuinely matches them.
- Keep the resume in clean Markdown. Keep the cover letter to 3-4 short paragraphs.
- Score the fit 0-100 honestly based on how well the candidate matches the
  requirements; do not inflate it.
- "strengths": 2-4 short bullet phrases on why the candidate fits.
- "gaps": 1-3 short bullet phrases on requirements the candidate is missing or weak on (empty list if none).

Respond with ONLY a JSON object, no prose, no code fences, of exactly this shape:
{"match_score": <int 0-100>, "match_reason": "<one or two sentences>", "strengths": ["..."], "gaps": ["..."], "resume": "<markdown>", "cover_letter": "<text>"}`

// Generate produces tailored materials for job using the candidate profile.
func Generate(ctx context.Context, p ai.Provider, cand config.Candidate, job store.Job, opts Options) (Result, error) {
	raw, err := p.Generate(ctx, ai.Request{
		System:      systemPrompt,
		Prompt:      buildPrompt(cand, job, opts),
		MaxTokens:   4096,
		Temperature: 0.7,
	})
	if err != nil {
		return Result{}, err
	}
	res, err := parseResult(raw)
	if err != nil {
		return Result{}, fmt.Errorf("could not parse AI response: %w", err)
	}
	if res.MatchScore < 0 {
		res.MatchScore = 0
	}
	if res.MatchScore > 100 {
		res.MatchScore = 100
	}
	return res, nil
}

func buildPrompt(c config.Candidate, j store.Job, opts Options) string {
	var b strings.Builder
	b.WriteString("## CANDIDATE\n")
	writeField(&b, "Name", c.Name)
	writeField(&b, "Headline", c.Headline)
	writeField(&b, "Email", c.Email)
	writeField(&b, "Phone", c.Phone)
	writeField(&b, "Location", c.Location)
	writeField(&b, "LinkedIn", c.LinkedIn)
	writeField(&b, "GitHub", c.GitHub)
	writeField(&b, "Website", c.Website)
	if len(c.Skills) > 0 {
		writeField(&b, "Skills", strings.Join(c.Skills, ", "))
	}
	if opts.Interest != "" {
		writeField(&b, "What the candidate is looking for", opts.Interest)
	}
	if c.Summary != "" {
		b.WriteString("\nSummary:\n")
		b.WriteString(c.Summary)
		b.WriteString("\n")
	}
	if strings.TrimSpace(c.BaseResume) != "" {
		b.WriteString("\nBase resume (source of truth — do not contradict or exceed it):\n")
		b.WriteString(c.BaseResume)
		b.WriteString("\n")
	}

	b.WriteString("\n## JOB\n")
	writeField(&b, "Title", j.Title)
	writeField(&b, "Company", j.Company)
	writeField(&b, "Location", j.Location)
	if j.Salary != "" {
		writeField(&b, "Salary", j.Salary)
	}
	if len(j.Tags) > 0 {
		writeField(&b, "Tags", strings.Join(j.Tags, ", "))
	}
	b.WriteString("\nDescription:\n")
	b.WriteString(truncate(j.Description, 6000))

	b.WriteString("\n\n## TASK\n")
	if strings.TrimSpace(opts.Instructions) != "" {
		b.WriteString("Revise the previous application below according to these instructions: ")
		b.WriteString(opts.Instructions)
		b.WriteString("\n\nPrevious resume:\n")
		b.WriteString(truncate(opts.PreviousResume, 4000))
		b.WriteString("\n\nPrevious cover letter:\n")
		b.WriteString(truncate(opts.PreviousCover, 2000))
		b.WriteString("\n\nReturn the full revised JSON object described in the system message.")
	} else {
		b.WriteString("Tailor the candidate's resume to this job and write a cover letter. Return the JSON object described in the system message.")
	}
	return b.String()
}

// SelectCategories asks the model which of the given category labels best match
// the user's interest, returning the chosen 0-based indices (at most max).
func SelectCategories(ctx context.Context, p ai.Provider, interest string, labels []string, max int) ([]int, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Job seeker's interest:\n%s\n\nAvailable categories (index: label):\n", interest)
	for i, l := range labels {
		fmt.Fprintf(&b, "%d: %s\n", i, l)
	}
	fmt.Fprintf(&b, "\nReturn a JSON array of the integer indices of up to %d categories that best match the interest, most relevant first. If none are a good match, return [].", max)

	raw, err := p.Generate(ctx, ai.Request{
		System:      "You map a job seeker's described interest onto a job board's fixed category list. Reply with ONLY a JSON array of integers (0-based indices), nothing else.",
		Prompt:      b.String(),
		MaxTokens:   200,
		Temperature: 0,
	})
	if err != nil {
		return nil, err
	}
	start, end := strings.Index(raw, "["), strings.LastIndex(raw, "]")
	if start < 0 || end < start {
		return nil, fmt.Errorf("no JSON array in response")
	}
	var idx []int
	if err := json.Unmarshal([]byte(raw[start:end+1]), &idx); err != nil {
		return nil, err
	}
	// Validate, de-dupe and cap.
	seen := map[int]bool{}
	var out []int
	for _, i := range idx {
		if i >= 0 && i < len(labels) && !seen[i] {
			seen[i] = true
			out = append(out, i)
			if len(out) >= max {
				break
			}
		}
	}
	return out, nil
}

// ParsedProfile is the structured profile the AI extracts from a résumé, used
// to set up the candidate and choose what roles to search for.
type ParsedProfile struct {
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	Phone    string   `json:"phone"`
	Location string   `json:"location"`
	Headline string   `json:"headline"`
	Summary  string   `json:"summary"`
	Skills   []string `json:"skills"`
	// Roles are 2–5 concrete job titles / searches this candidate should target,
	// derived from their experience (e.g. "Mechanical Engineer", "Manufacturing
	// Engineer"). They become the multi-role search queries.
	Roles []string `json:"roles"`
}

// ParseProfile asks the model to extract a structured profile + target roles
// from résumé text.
func ParseProfile(ctx context.Context, p ai.Provider, resumeText string) (ParsedProfile, error) {
	prompt := "Résumé:\n" + truncate(resumeText, 9000) + "\n\n" +
		"Extract the candidate's details and decide what jobs they should search for. " +
		"Respond with ONLY a JSON object of exactly this shape:\n" +
		`{"name":"","email":"","phone":"","location":"City, ST","headline":"<short professional title>",` +
		`"summary":"<2-3 sentence professional summary>","skills":["..."],` +
		`"roles":["<2-5 concrete job titles to search for, based on their real experience>"]}` +
		"\nUse empty strings/arrays for anything not present. Do not invent contact details."

	raw, err := p.Generate(ctx, ai.Request{
		System:      "You parse résumés into structured JSON for a job-search tool. Reply with only the JSON object, no prose.",
		Prompt:      prompt,
		MaxTokens:   1200,
		Temperature: 0,
	})
	if err != nil {
		return ParsedProfile{}, err
	}
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return ParsedProfile{}, fmt.Errorf("no JSON object in profile response")
	}
	var pp ParsedProfile
	if err := json.Unmarshal([]byte(raw[start:end+1]), &pp); err != nil {
		return ParsedProfile{}, err
	}
	return pp, nil
}

// Prescreen is the cheap stage-2 relevance check: it scores how well a job fits
// the candidate (0-100) with a one-line reason, without writing any materials.
// Used to filter out obvious mismatches before the expensive tailoring stage.
func Prescreen(ctx context.Context, p ai.Provider, cand config.Candidate, interest string, job store.Job) (int, string, error) {
	var b strings.Builder
	b.WriteString("CANDIDATE\n")
	writeField(&b, "Headline", cand.Headline)
	if len(cand.Skills) > 0 {
		writeField(&b, "Skills", strings.Join(cand.Skills, ", "))
	}
	if interest != "" {
		writeField(&b, "Looking for", interest)
	}
	if cand.Summary != "" {
		writeField(&b, "Summary", truncate(cand.Summary, 600))
	}
	if strings.TrimSpace(cand.BaseResume) != "" {
		b.WriteString("Résumé excerpt: ")
		b.WriteString(truncate(cand.BaseResume, 1200))
		b.WriteString("\n")
	}
	b.WriteString("\nJOB\n")
	writeField(&b, "Title", job.Title)
	writeField(&b, "Company", job.Company)
	writeField(&b, "Location", job.Location)
	b.WriteString("Description: ")
	b.WriteString(truncate(job.Description, 1500))
	b.WriteString("\n\nHow well does this candidate fit this job? Respond with ONLY a JSON object: " +
		`{"score": <int 0-100>, "reason": "<one short sentence>"}. Score honestly on skills/role/seniority/location fit.`)

	raw, err := p.Generate(ctx, ai.Request{
		System:      "You are a recruiter quickly screening job fit. Reply with only the JSON object requested, no prose.",
		Prompt:      b.String(),
		MaxTokens:   150,
		Temperature: 0,
	})
	if err != nil {
		return 0, "", err
	}
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return 0, "", fmt.Errorf("no JSON object in prescreen response")
	}
	var out struct {
		Score  int    `json:"score"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &out); err != nil {
		return 0, "", err
	}
	if out.Score < 0 {
		out.Score = 0
	}
	if out.Score > 100 {
		out.Score = 100
	}
	return out.Score, out.Reason, nil
}

// PrescreenResult is one job's relevance score from a batch prescreen.
type PrescreenResult struct {
	Score  int
	Reason string
}

// PrescreenBatch scores how well the candidate fits each of several jobs in a
// SINGLE model call, returning results keyed by job ID. This is the cheap
// stage-2 filter: one request screens many postings instead of one-per-job.
func PrescreenBatch(ctx context.Context, p ai.Provider, cand config.Candidate, interest string, jobs []store.Job) (map[string]PrescreenResult, error) {
	if len(jobs) == 0 {
		return map[string]PrescreenResult{}, nil
	}
	var b strings.Builder
	b.WriteString("CANDIDATE\n")
	writeField(&b, "Headline", cand.Headline)
	if len(cand.Skills) > 0 {
		writeField(&b, "Skills", strings.Join(cand.Skills, ", "))
	}
	if interest != "" {
		writeField(&b, "Looking for", interest)
	}
	if strings.TrimSpace(cand.BaseResume) != "" {
		b.WriteString("Résumé excerpt: ")
		b.WriteString(truncate(cand.BaseResume, 1200))
		b.WriteString("\n")
	}
	b.WriteString("\nJOBS (index: title @ company — location):\n")
	for i, j := range jobs {
		fmt.Fprintf(&b, "%d: %s @ %s — %s :: %s\n", i, j.Title, orDash(j.Company), orDash(j.Location), oneLine(truncate(j.Description, 220)))
	}
	fmt.Fprintf(&b, "\nScore how well the candidate fits EACH job (0-100, honest). Respond with ONLY a JSON array, one object per index, covering every index: "+
		`[{"i":0,"score":<0-100>,"reason":"<short>"}, ...]. There are %d jobs (indices 0-%d).`, len(jobs), len(jobs)-1)

	maxTok := len(jobs)*70 + 200
	if maxTok > 4096 {
		maxTok = 4096
	}
	raw, err := p.Generate(ctx, ai.Request{
		System:      "You are a recruiter screening job fit in bulk. Reply with only the JSON array requested, no prose.",
		Prompt:      b.String(),
		MaxTokens:   maxTok,
		Temperature: 0,
	})
	if err != nil {
		return nil, err
	}
	start, end := strings.Index(raw, "["), strings.LastIndex(raw, "]")
	if start < 0 || end < start {
		return nil, fmt.Errorf("no JSON array in batch prescreen response")
	}
	var arr []struct {
		I      int    `json:"i"`
		Score  int    `json:"score"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &arr); err != nil {
		return nil, err
	}
	out := make(map[string]PrescreenResult, len(arr))
	for _, e := range arr {
		if e.I < 0 || e.I >= len(jobs) {
			continue
		}
		s := e.Score
		if s < 0 {
			s = 0
		} else if s > 100 {
			s = 100
		}
		out[jobs[e.I].ID] = PrescreenResult{Score: s, Reason: e.Reason}
	}
	return out, nil
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// PickApplyURL asks the model for the company's official application URL for a
// job — the page on the company's own website or applicant-tracking system
// where one actually applies, not the job board it was found on. It may choose
// from candidates (links extracted from the listing) or infer a better one from
// the company name. Returns "" when it can't determine one.
func PickApplyURL(ctx context.Context, p ai.Provider, job store.Job, candidates []string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Job title: %s\nCompany: %s\nListing URL: %s\n", job.Title, job.Company, job.URL)
	if job.CompanyURL != "" {
		fmt.Fprintf(&b, "Known company website: %s\n", job.CompanyURL)
	}
	if len(candidates) > 0 {
		b.WriteString("\nLinks found in the listing:\n")
		for _, c := range candidates {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	b.WriteString("\nJob description (excerpt):\n")
	b.WriteString(truncate(job.Description, 2500))
	b.WriteString("\n\nReturn ONLY a JSON object: {\"url\": \"<the company's own application or careers URL, or an empty string if you cannot determine one>\"}. " +
		"Prefer an applicant-tracking page (Greenhouse, Lever, Ashby, Workday, SmartRecruiters, Workable, etc.) or the company's own careers page. " +
		"NEVER return a job-board/aggregator URL (LinkedIn, Indeed, ZipRecruiter, Monster, SimplyHired, Craigslist, Glassdoor, The Muse, Remotive, etc.).")

	raw, err := p.Generate(ctx, ai.Request{
		System:      "You identify the official application URL for a job on the hiring company's own website or applicant-tracking system. Reply with only the JSON object requested, no prose.",
		Prompt:      b.String(),
		MaxTokens:   200,
		Temperature: 0,
	})
	if err != nil {
		return "", err
	}
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return "", nil
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &out); err != nil {
		return "", nil
	}
	return strings.TrimSpace(out.URL), nil
}

func writeField(b *strings.Builder, k, v string) {
	if strings.TrimSpace(v) != "" {
		fmt.Fprintf(b, "%s: %s\n", k, v)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated]"
}

// parseResult extracts the JSON object from the model output, tolerating code
// fences and surrounding prose.
func parseResult(raw string) (Result, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(s, "json")
		s = strings.TrimPrefix(s, "JSON")
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < 0 || end < start {
		return Result{}, fmt.Errorf("no JSON object found in response")
	}
	var res Result
	if err := json.Unmarshal([]byte(s[start:end+1]), &res); err != nil {
		return Result{}, err
	}
	if res.Resume == "" && res.CoverLetter == "" {
		return Result{}, fmt.Errorf("response contained no resume or cover letter")
	}
	return res, nil
}
