package syncer

import (
	"errors"
	"testing"

	"golang.org/x/oauth2"
)

func TestBlacklisted(t *testing.T) {
	pats := []string{"spam@x.com", "@ads.example", "promo.net"}
	cases := map[string]bool{
		"spam@x.com":           true,
		"SPAM@X.COM":           true,
		"other@x.com":          false,
		"a@ads.example":        true,
		"a@eu.ads.example":     true,
		"a@notads.example":     false,
		"deals@promo.net":      true,
		"deals@mail.promo.net": true,
		"deals@promo.network":  false,
	}
	for addr, want := range cases {
		if got := Blacklisted(addr, pats); got != want {
			t.Errorf("Blacklisted(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestIsAuthError(t *testing.T) {
	if !IsAuthError(&oauth2.RetrieveError{ErrorCode: "invalid_grant"}) {
		t.Error("invalid_grant not detected")
	}
	if IsAuthError(errors.New("connection refused")) || IsAuthError(nil) {
		t.Error("false positive")
	}
}
