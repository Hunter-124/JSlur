// Package config defines the persisted application configuration: the
// candidate profile, the job-search focus, AI provider settings and the
// apply/automation behaviour. Everything the GUI lets the user edit lives here.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Config is the root configuration object. It is serialised to config.json in
// the application data directory and edited through the GUI.
type Config struct {
	Candidate Candidate     `json:"candidate"`
	Focus     JobFocus      `json:"focus"`
	AI        AIConfig      `json:"ai"`
	Sources   SourcesConfig `json:"sources"`
	Apply     ApplyConfig   `json:"apply"`
}

// Candidate is everything we know about the job seeker. Both the structured
// fields and the free-form BaseResume are fed to the AI when tailoring an
// application, so a user can fill in as much or as little as they like.
type Candidate struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Phone    string `json:"phone"`
	Location string `json:"location"`
	LinkedIn string `json:"linkedin"`
	GitHub   string `json:"github"`
	Website  string `json:"website"`

	// Headline is a short professional tagline, e.g. "Senior Backend Engineer".
	Headline string `json:"headline"`
	// Summary is a paragraph describing the candidate.
	Summary string `json:"summary"`
	// Skills is the candidate's skill set.
	Skills []string `json:"skills"`
	// BaseResume is the master resume in plain text / markdown. The AI uses it
	// as the source of truth and tailors it per job. This is the single most
	// important field for good results.
	BaseResume string `json:"baseResume"`
}

// JobFocus controls what jobs we search for and how we filter them. The app is
// US-focused: searches are scoped to a location + radius (and optionally remote
// US roles).
type JobFocus struct {
	// Interest is a free-text description of the roles the user wants, e.g.
	// "Registered nurse — ICU or ER; open to clinical educator". The AI maps
	// this onto each job board's own category taxonomy to decide what to search.
	Interest string `json:"interest"`
	// Location is the US city/state/ZIP and search radius.
	Location Location `json:"location"`
	// IncludeRemote also pulls in US-wide remote roles (which have no radius).
	IncludeRemote bool `json:"includeRemote"`
	// ExcludeKeywords reject a job if any appears in its title/description.
	ExcludeKeywords []string `json:"excludeKeywords"`
	// MinSalary, when > 0, drops jobs whose advertised salary is clearly below it
	// (only applied to sources that report salary, e.g. Adzuna).
	MinSalary int `json:"minSalary"`
	// Sources is the set of enabled job-source ids (e.g. "themuse").
	Sources []string `json:"sources"`
	// MaxResultsPerSource caps how many jobs each source returns per search.
	MaxResultsPerSource int `json:"maxResultsPerSource"`
	// MinMatchScore is the AI fit score (0-100) below which a job is auto-skipped
	// after full tailoring (stage 3).
	MinMatchScore int `json:"minMatchScore"`
	// MinPrescreenScore is the AI relevance score (0-100) below which a job is
	// dropped by the cheap filter stage (stage 2) before tailoring. 0 disables
	// the filter (everything is tailored).
	MinPrescreenScore int `json:"minPrescreenScore"`
}

// Location is a US location with a search radius.
type Location struct {
	City        string `json:"city"`        // e.g. "Chicago"
	State       string `json:"state"`       // e.g. "IL"
	Zip         string `json:"zip"`         // e.g. "60601"
	RadiusMiles int    `json:"radiusMiles"` // 0 = source default
}

// Query returns the most specific "place" string for board APIs: ZIP if given,
// otherwise "City, State".
func (l Location) Query() string {
	if z := strings.TrimSpace(l.Zip); z != "" {
		return z
	}
	city, state := strings.TrimSpace(l.City), strings.TrimSpace(l.State)
	switch {
	case city != "" && state != "":
		return city + ", " + state
	case city != "":
		return city
	default:
		return state
	}
}

// Provider identifiers.
const (
	ProviderAnthropic = "anthropic"
	ProviderGoogle    = "google"
	ProviderDeepSeek  = "deepseek"
	ProviderLocal     = "local"
)

// AIConfig holds settings for every supported AI backend plus which one is
// currently active.
type AIConfig struct {
	// Active is the provider id used for generation.
	Active string `json:"active"`

	Anthropic ProviderConfig `json:"anthropic"`
	Google    ProviderConfig `json:"google"`
	DeepSeek  ProviderConfig `json:"deepseek"`
	Local     ProviderConfig `json:"local"`
}

// ProviderConfig describes a single AI backend.
type ProviderConfig struct {
	APIKey string `json:"apiKey"`
	Model  string `json:"model"`
	// BaseURL is used by OpenAI-compatible backends (DeepSeek, local models).
	// It is ignored by Anthropic and Google which have fixed endpoints.
	BaseURL     string  `json:"baseUrl"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"maxTokens"`
	// ReasoningEffort, when set ("none", "low", "medium" or "high"), is passed to
	// OpenAI-compatible servers (LM Studio, vLLM, DeepSeek, …) to control how much
	// a "thinking" model reasons before answering. Hidden reasoning still counts
	// against MaxTokens, so a chatty reasoning model can burn the whole budget and
	// return an empty answer; "none" disables it and is dramatically faster. Empty
	// omits the field entirely (and is ignored by Anthropic and Google).
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

// SourcesConfig holds per-board API credentials. Sources without credentials
// are skipped (with a friendly note) rather than failing. Keyless sources
// (The Muse, Remotive) need nothing here.
type SourcesConfig struct {
	Adzuna  AdzunaConfig  `json:"adzuna"`
	USAJobs USAJobsConfig `json:"usajobs"`
	// Accounts holds captured browser sessions keyed by source id (e.g.
	// "linkedin"). Populated by the "connect account" flow and replayed on scrape
	// requests to get logged-in results and pass anti-bot checks. Never a password.
	Accounts map[string]Account `json:"accounts,omitempty"`
	// Browser tunes the AI Browser Search (vision) source. It is not a credential,
	// but it lives here alongside the captured Accounts that source replays.
	Browser BrowserSearchConfig `json:"browser"`
}

// BrowserSearchConfig tunes the vision-based browser search source: instead of
// hitting scrape endpoints, it drives a real browser to each board's normal
// search-results page and has a vision-capable AI model read the listings off
// screenshots — so it isn't rate-limited or blocked the way plain HTTP can be.
type BrowserSearchConfig struct {
	// Boards is the set of board ids to drive ("indeed", "linkedin",
	// "ziprecruiter", "google"). Empty means a sensible default (Indeed + LinkedIn).
	Boards []string `json:"boards"`
	// Headful shows the browser window while searching (default: headless).
	Headful bool `json:"headful"`
	// MaxScreens caps the screenshots captured per board search (default 3). More
	// screens read more of the page (more jobs) but are slower and cost more.
	MaxScreens int `json:"maxScreens"`
	// Engine selects the browser backend: "" / "chromedp" uses the built-in
	// browser; "python" uses a Playwright stealth sidecar (pw-stealth-enhanced)
	// that gets past tougher bot walls like Indeed's, but needs a local Python with
	// `pip install playwright pw-stealth-enhanced` and `playwright install chromium`.
	Engine string `json:"engine,omitempty"`
	// PythonPath overrides the Python executable used for the stealth sidecar.
	// Empty uses "python" from PATH.
	PythonPath string `json:"pythonPath,omitempty"`
}

// Account is a captured browser session for a job board: its cookies (as a
// ready-to-send Cookie header) and the User-Agent they were issued against
// (cf_clearance is bound to the UA, so both must be replayed together).
type Account struct {
	Cookie     string    `json:"cookie"`
	UserAgent  string    `json:"userAgent"`
	CapturedAt time.Time `json:"capturedAt"`
}

// AdzunaConfig holds free Adzuna API credentials (developer.adzuna.com).
type AdzunaConfig struct {
	AppID  string `json:"appId"`
	AppKey string `json:"appKey"`
}

// USAJobsConfig holds free USAJOBS API credentials (developer.usajobs.gov).
// Email is sent as the User-Agent, as the USAJOBS API requires.
type USAJobsConfig struct {
	APIKey string `json:"apiKey"`
	Email  string `json:"email"`
}

// Apply channel identifiers.
const (
	// ApplyReview prepares materials but submits nothing; the user reviews and
	// acts manually. This is the safe default.
	ApplyReview = "review"
	// ApplyExport writes tailored materials to disk for each ready application.
	ApplyExport = "export"
	// ApplyEmail emails the application when the job posting exposes an apply
	// address and SMTP is configured.
	ApplyEmail = "email"
)

// ApplyConfig controls the automation/apply behaviour.
type ApplyConfig struct {
	// AutoMode runs the full pipeline (search -> tailor -> apply) on a timer.
	AutoMode bool `json:"autoMode"`
	// IntervalMinutes is how often AutoMode runs a search cycle.
	IntervalMinutes int `json:"intervalMinutes"`
	// Channel is how ready applications are acted on: review|export|email.
	Channel string `json:"channel"`
	// MaxAppliesPerRun caps applications acted on per cycle (rate limiting).
	MaxAppliesPerRun int `json:"maxAppliesPerRun"`
	// AutoApply, when false, leaves prepared applications for manual approval
	// even in AutoMode. When true the chosen Channel is executed automatically.
	AutoApply bool `json:"autoApply"`

	// ExportDir is where ApplyExport writes files. Empty -> <dataDir>/applications.
	ExportDir string `json:"exportDir"`

	// SMTP settings for ApplyEmail.
	SMTP SMTPConfig `json:"smtp"`
}

// SMTPConfig holds outgoing email settings for the email apply channel.
type SMTPConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
}

// Default returns a sensible starter configuration.
func Default() Config {
	return Config{
		Candidate: Candidate{
			Skills: []string{},
		},
		Focus: JobFocus{
			Interest:            "",
			Location:            Location{RadiusMiles: 25},
			IncludeRemote:       true,
			ExcludeKeywords:     []string{},
			Sources:             []string{"themuse", "linkedin", "indeed", "remotive"},
			MaxResultsPerSource: 25,
			MinMatchScore:       60,
			MinPrescreenScore:   45,
		},
		Sources: SourcesConfig{
			Browser: BrowserSearchConfig{Boards: []string{"indeed", "linkedin"}, MaxScreens: 3},
		},
		AI: AIConfig{
			Active: ProviderAnthropic,
			Anthropic: ProviderConfig{
				Model:       "claude-sonnet-4-6",
				Temperature: 0.7,
				MaxTokens:   4096,
			},
			Google: ProviderConfig{
				Model:       "gemini-2.0-flash",
				Temperature: 0.7,
				MaxTokens:   4096,
			},
			DeepSeek: ProviderConfig{
				Model:       "deepseek-v4-flash",
				BaseURL:     "https://api.deepseek.com",
				Temperature: 0.7,
				MaxTokens:   4096,
			},
			Local: ProviderConfig{
				Model:       "llama3.1",
				BaseURL:     "http://localhost:11434/v1",
				Temperature: 0.7,
				MaxTokens:   4096,
			},
		},
		Apply: ApplyConfig{
			AutoMode:         false,
			IntervalMinutes:  60,
			Channel:          ApplyReview,
			MaxAppliesPerRun: 5,
			AutoApply:        false,
			SMTP:             SMTPConfig{Port: 587},
		},
	}
}

// Store loads and persists the Config atomically and is safe for concurrent use.
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// NewStore loads config from path, creating defaults if the file is absent.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path, cfg: Default()}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := s.save(); err != nil {
				return nil, err
			}
			return s, nil
		}
		return nil, err
	}
	// Start from defaults so newly added fields keep sensible values, then
	// overlay the persisted values.
	cfg := Default()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	s.cfg = cfg
	return s, nil
}

// Get returns a copy of the current configuration.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Set replaces the configuration and persists it.
func (s *Store) Set(cfg Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	return s.save()
}

// save writes the config to disk. Caller must hold the lock (or be in init).
func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
