package web

import (
	"net/url"
	"testing"
	"time"
)

func TestParseLocationRangeRejectsInvalidAndReversedValues(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	for _, values := range []url.Values{
		{"hours": {"bad"}},
		{"hours": {"87601"}},
		{"from": {"bad"}},
		{"to": {"-1"}},
		{"from": {"20"}, "to": {"10"}},
	} {
		if _, _, err := parseLocationRange(values, now, true); err == nil {
			t.Fatalf("accepted %v", values)
		}
	}
	from, to, err := parseLocationRange(url.Values{"hours": {"24"}, "from": {"10"}, "to": {"20"}}, now, true)
	if err != nil || from != 10 || to != 20 {
		t.Fatalf("range=(%d,%d) err=%v", from, to, err)
	}
}

func TestParseTrackLimit(t *testing.T) {
	for _, raw := range []string{"bad", "-1", "20001"} {
		if _, err := parseTrackLimit(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"", "0", "1", "20000"} {
		if _, err := parseTrackLimit(raw); err != nil {
			t.Fatalf("rejected %q: %v", raw, err)
		}
	}
}
