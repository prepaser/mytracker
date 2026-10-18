package ratelimit

import (
	"fmt"
	"net"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiterAllowsUntilLimit(t *testing.T) {
	now := time.Unix(100, 0)
	rl := New(2, time.Minute)
	rl.now = func() time.Time { return now }

	if ok, _ := rl.Allow("ip"); !ok {
		t.Fatal("first request blocked")
	}
	if ok, _ := rl.Allow("ip"); !ok {
		t.Fatal("second request blocked")
	}
	if ok, retry := rl.Allow("ip"); ok || retry <= 0 {
		t.Fatalf("third request: ok=%v retry=%v", ok, retry)
	}

	now = now.Add(time.Minute + time.Second)
	if ok, _ := rl.Allow("ip"); !ok {
		t.Fatal("request after window blocked")
	}
}

func TestLimiterReset(t *testing.T) {
	rl := New(1, time.Minute)
	rl.Allow("ip")
	if ok, _ := rl.Allow("ip"); ok {
		t.Fatal("expected block")
	}
	rl.Reset("ip")
	if ok, _ := rl.Allow("ip"); !ok {
		t.Fatal("blocked after reset")
	}
}

func TestClientKeyUsesForwardedForFromLoopback(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.10, 10.0.0.1")
	if got := ClientKey(req, []string{"10.0.0.1"}); got != "203.0.113.10" {
		t.Fatalf("key=%q", got)
	}
}

func TestClientKeyRejectsSpoofedForwardedPrefix(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.99, 203.0.113.10")
	req.Header.Set("X-Real-IP", "198.51.100.98")
	if got := ClientKey(req, nil); got != "203.0.113.10" {
		t.Fatalf("key=%q", got)
	}
}

func TestLimiterBoundsAndKeyHelpers(t *testing.T) {
	now := time.Unix(100, 0)
	rl := New(1, time.Second)
	rl.now = func() time.Time { return now }
	rl.SetMaxIPs(1)
	if ok, _ := rl.Allow(""); !ok {
		t.Fatal("empty key blocked")
	}
	rl.Allow("old")
	now = now.Add(3 * time.Second)
	rl.Allow("new")
	rl.Allow("third")
	if len(rl.hits) != 1 {
		t.Fatalf("bounded hits=%d", len(rl.hits))
	}
	if got := IngestKey("", ""); got != "ingest:owntracks/phone" {
		t.Fatalf("default ingest key=%q", got)
	}
	if got := IngestKey("u", "d"); got != "ingest:u/d" {
		t.Fatalf("ingest key=%q", got)
	}
}

func TestLimiterCleanupIsBoundedAndConverges(t *testing.T) {
	now := time.Unix(100, 0)
	rl := New(1, time.Second)
	rl.now = func() time.Time { return now }
	const entries = 5000
	for i := range entries {
		rl.Allow(fmt.Sprintf("ip-%d", i))
	}

	now = now.Add(3 * time.Second)
	rl.Allow("fresh")
	if len(rl.hits) < entries-gcBudget || len(rl.hits) >= entries+1 {
		t.Fatalf("cleanup was not bounded: remaining=%d", len(rl.hits))
	}

	for i := range entries/gcBudget + 2 {
		rl.Allow(fmt.Sprintf("next-%d", i))
	}
	if len(rl.hits) >= entries/2 {
		t.Fatalf("cleanup did not converge: remaining=%d", len(rl.hits))
	}
}

func TestLimiterEvictsLeastRecentlyUsedKeyAtCap(t *testing.T) {
	now := time.Unix(100, 0)
	rl := New(1, time.Minute)
	rl.now = func() time.Time { return now }
	rl.SetMaxIPs(2)

	rl.Allow("old")
	now = now.Add(time.Nanosecond)
	rl.Allow("keep")
	now = now.Add(time.Nanosecond)
	rl.Allow("old")
	now = now.Add(time.Nanosecond)
	rl.Allow("new")

	if len(rl.hits) != 2 {
		t.Fatalf("bounded hits=%d", len(rl.hits))
	}
	if _, ok := rl.hits["old"]; !ok {
		t.Fatal("least recently used key was evicted")
	}
	if _, ok := rl.hits["keep"]; ok {
		t.Fatal("stale key survived eviction")
	}
}

func TestClientKeyAndTrustedProxyBranches(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "invalid")
	req.Header.Set("X-Real-IP", "203.0.113.20")
	if got := ClientKey(req, nil); got != "127.0.0.1" {
		t.Fatalf("real ip=%q", got)
	}
	req.RemoteAddr = "not-an-ip"
	if got := ClientKey(req, nil); got != "not-an-ip" {
		t.Fatalf("raw host=%q", got)
	}
	if !IsTrustedProxy(net.ParseIP("10.0.0.5"), []string{"", "10.0.0.0/24"}) ||
		!IsTrustedProxy(net.ParseIP("192.0.2.1"), []string{"192.0.2.1"}) ||
		IsTrustedProxy(net.ParseIP("198.51.100.1"), []string{"bad"}) {
		t.Fatal("trusted proxy validation")
	}
}
