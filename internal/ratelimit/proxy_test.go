package ratelimit_test

import (
	"net/http/httptest"
	"testing"

	"github.com/prepaser/mytracker/internal/ratelimit"
)

func TestClientKeyTrustsConfiguredProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.20")
	got := ratelimit.ClientKey(req, []string{"10.0.0.1"})
	if got != "203.0.113.20" {
		t.Fatalf("key=%q", got)
	}
}

func TestClientKeyIgnoresForwardedWithoutTrust(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.20")
	got := ratelimit.ClientKey(req, nil)
	if got != "10.0.0.1" {
		t.Fatalf("key=%q", got)
	}
}
