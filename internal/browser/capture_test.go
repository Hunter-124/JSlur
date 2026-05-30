package browser

import "testing"

func TestCookieHeader(t *testing.T) {
	r := Result{Cookies: []Cookie{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}}}
	if got := r.CookieHeader(); got != "a=1; b=2" {
		t.Errorf("CookieHeader = %q", got)
	}
	if (Result{}).CookieHeader() != "" {
		t.Error("empty result should yield empty header")
	}
}

func TestHasAny(t *testing.T) {
	cks := []Cookie{{Name: "li_at", Value: "x"}}
	if !hasAny(cks, []string{"foo", "LI_AT"}) {
		t.Error("expected case-insensitive match on li_at")
	}
	if hasAny(cks, []string{"sess"}) {
		t.Error("expected no match")
	}
	if hasAny(cks, nil) {
		t.Error("nil names should be false")
	}
}
