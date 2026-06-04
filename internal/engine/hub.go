package engine

import (
	"sync"
	"time"
)

// Event is a message broadcast to GUI clients over SSE.
type Event struct {
	Type    string    `json:"type"`            // "log" | "refresh" | "attention" | "attention-clear"
	Level   string    `json:"level,omitempty"` // info | warn | error | success
	Time    time.Time `json:"time"`
	Message string    `json:"message,omitempty"`

	// The fields below carry an interactive "attention" prompt: when a stealth
	// scrape hits a sign-in/captcha wall in the visible browser, the engine asks
	// the user to deal with it and resume. "attention" raises the prompt;
	// "attention-clear" (same ID) takes it down once resolved/expired.
	ID   string `json:"id,omitempty"`   // unique prompt id; the GUI replies to /api/attention/{id}
	Kind string `json:"kind,omitempty"` // "login" | "captcha" | "cloudflare"
	Host string `json:"host,omitempty"` // the board host the prompt is about
}

// Hub is a tiny in-process publish/subscribe broker with a bounded history so
// late-joining clients can render recent log lines.
type Hub struct {
	mu      sync.Mutex
	subs    map[chan Event]struct{}
	history []Event
}

// NewHub creates an empty hub.
func NewHub() *Hub {
	return &Hub{subs: map[chan Event]struct{}{}}
}

// Publish stamps the event, records it in history and fans it out. Slow
// subscribers are skipped rather than blocking the publisher.
func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	h.history = append(h.history, e)
	if len(h.history) > 500 {
		h.history = h.history[len(h.history)-500:]
	}
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Subscribe returns a channel of future events and an unsubscribe function.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// History returns a copy of recent events.
func (h *Hub) History() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Event, len(h.history))
	copy(out, h.history)
	return out
}
