package ai

import "testing"

func TestStripThink(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ok", "ok"},
		{"  spaced  ", "spaced"},
		{"<think>reasoning here</think>answer", "answer"},
		{"before<think>mid</think>after", "beforeafter"},
		{"<think>a</think> [1,2] <think>b</think>", "[1,2]"},
		// Unclosed (truncated) think block drops everything from the tag on.
		{"partial<think>cut off", "partial"},
		{"<think>only reasoning, no answer</think>", ""},
	}
	for _, c := range cases {
		if got := stripThink(c.in); got != c.want {
			t.Errorf("stripThink(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
