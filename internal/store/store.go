// Package store persists discovered jobs and the applications generated for
// them. It uses a single JSON file guarded by a mutex — simple, dependency-free
// and more than adequate for a single-user desktop tool.
package store

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Application status values. The first group is the generation pipeline; the
// last three let the tool double as an application tracker after you apply.
const (
	StatusDiscovered = "discovered" // found by search, not yet processed
	StatusMatched    = "matched"    // passed the AI relevance filter, awaiting tailoring
	StatusGenerating = "generating" // AI is tailoring materials
	StatusReady      = "ready"      // materials ready, awaiting apply/review
	StatusApplied    = "applied"    // application submitted/exported
	StatusSkipped    = "skipped"    // below threshold or user-skipped
	StatusError      = "error"      // generation/apply failed

	// Post-apply tracking states (set manually in the GUI).
	StatusInterviewing = "interviewing"
	StatusOffer        = "offer"
	StatusRejected     = "rejected"
)

// TrackingStatuses are the statuses a user may set manually on an application.
var TrackingStatuses = map[string]bool{
	StatusReady: true, StatusApplied: true, StatusSkipped: true,
	StatusInterviewing: true, StatusOffer: true, StatusRejected: true,
}

// Job is a single job posting discovered from a source.
type Job struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Title    string `json:"title"`
	Company  string `json:"company"`
	Location string `json:"location"`
	Remote   bool   `json:"remote"`
	URL      string `json:"url"`
	// CompanyURL is the employer's own website/careers root, when known.
	CompanyURL string `json:"companyUrl"`
	// ApplyURL is the resolved application URL on the company's own site/ATS
	// (as opposed to the board the posting was discovered on). Filled in by the
	// engine's official-URL resolver.
	ApplyURL     string    `json:"applyUrl"`
	ApplyEmail   string    `json:"applyEmail"`
	Description  string    `json:"description"`
	Salary       string    `json:"salary"`    // human-readable, for display
	SalaryMin    int       `json:"salaryMin"` // annual USD, 0 if unknown
	Tags         []string  `json:"tags"`
	PostedAt     time.Time `json:"postedAt"`
	DiscoveredAt time.Time `json:"discoveredAt"`
}

// MakeJobID derives a stable id from the source and URL so the same posting is
// not stored twice across searches.
func MakeJobID(source, url string) string {
	h := sha1.Sum([]byte(source + "|" + url))
	return hex.EncodeToString(h[:])[:16]
}

// Application is the tailored output for one job plus its lifecycle state.
type Application struct {
	JobID  string `json:"jobId"`
	Status string `json:"status"`
	// PrescreenScore/Reason come from the cheap AI relevance filter (stage 2),
	// before the expensive full tailoring sets MatchScore (stage 3).
	PrescreenScore  int        `json:"prescreenScore"`
	PrescreenReason string     `json:"prescreenReason"`
	MatchScore      int        `json:"matchScore"`
	MatchReason     string     `json:"matchReason"`
	Strengths       []string   `json:"strengths"`
	Gaps            []string   `json:"gaps"`
	Resume          string     `json:"resume"`
	CoverLetter     string     `json:"coverLetter"`
	Notes           string     `json:"notes"`
	Error           string     `json:"error"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	AppliedAt       *time.Time `json:"appliedAt,omitempty"`
}

// Record bundles a job with its application for convenient API responses.
type Record struct {
	Job         Job          `json:"job"`
	Application *Application `json:"application,omitempty"`
}

type data struct {
	Jobs         map[string]Job         `json:"jobs"`
	Applications map[string]Application `json:"applications"`
}

// Store is the concurrency-safe persistent store.
type Store struct {
	mu   sync.RWMutex
	path string
	data data
}

// New loads the store from path, creating an empty one if absent.
func New(path string) (*Store, error) {
	s := &Store{
		path: path,
		data: data{Jobs: map[string]Job{}, Applications: map[string]Application{}},
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, s.save()
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, err
	}
	if s.data.Jobs == nil {
		s.data.Jobs = map[string]Job{}
	}
	if s.data.Applications == nil {
		s.data.Applications = map[string]Application{}
	}
	return s, nil
}

// UpsertJob stores a job if new and reports whether it was newly added.
func (s *Store) UpsertJob(j Job) (added bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Jobs[j.ID]; ok {
		return false
	}
	if j.DiscoveredAt.IsZero() {
		j.DiscoveredAt = time.Now()
	}
	s.data.Jobs[j.ID] = j
	_ = s.save()
	return true
}

// SetJobApplyInfo records the resolved official application/company URLs on a
// stored job (non-empty values only) and returns the updated job.
func (s *Store) SetJobApplyInfo(id, applyURL, companyURL string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.data.Jobs[id]
	if !ok {
		return Job{}, false
	}
	if applyURL != "" {
		j.ApplyURL = applyURL
	}
	if companyURL != "" {
		j.CompanyURL = companyURL
	}
	s.data.Jobs[id] = j
	_ = s.save()
	return j, true
}

// GetJob returns a job by id.
func (s *Store) GetJob(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.data.Jobs[id]
	return j, ok
}

// GetApplication returns an application by job id.
func (s *Store) GetApplication(jobID string) (Application, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.data.Applications[jobID]
	return a, ok
}

// SaveApplication creates or updates an application, stamping UpdatedAt.
func (s *Store) SaveApplication(a Application) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	s.data.Applications[a.JobID] = a
	return s.save()
}

// Records returns every job with its application, newest discovery first.
func (s *Store) Records() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0, len(s.data.Jobs))
	for id, j := range s.data.Jobs {
		r := Record{Job: j}
		if a, ok := s.data.Applications[id]; ok {
			ac := a
			r.Application = &ac
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, k int) bool {
		if !out[i].Job.DiscoveredAt.Equal(out[k].Job.DiscoveredAt) {
			return out[i].Job.DiscoveredAt.After(out[k].Job.DiscoveredAt)
		}
		return out[i].Job.ID < out[k].Job.ID // stable tiebreak for equal timestamps
	})
	return out
}

// Stats summarises the store for the dashboard.
type Stats struct {
	TotalJobs int            `json:"totalJobs"`
	ByStatus  map[string]int `json:"byStatus"`
}

// Stats computes summary counts.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{TotalJobs: len(s.data.Jobs), ByStatus: map[string]int{}}
	for _, a := range s.data.Applications {
		st.ByStatus[a.Status]++
	}
	return st
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
