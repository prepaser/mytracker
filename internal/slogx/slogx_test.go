package slogx

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevelAndInit(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"WARN":  slog.LevelWarn,
		"error": slog.LevelError,
		"other": slog.LevelInfo,
	}
	for input, want := range cases {
		if got := parseLevel(input); got != want {
			t.Fatalf("%q: got %v want %v", input, got, want)
		}
	}
	Init("debug")
}

func TestSafeError(t *testing.T) {
	got := SafeError(errors.New("Post https://example.com/hook?token=secret: failed password=hunter2"))
	if strings.Contains(got, "/hook") || strings.Contains(got, "secret") || strings.Contains(got, "hunter2") {
		t.Fatalf("sensitive error=%q", got)
	}
	if !strings.Contains(got, "https://example.com") || SafeError(nil) != "" {
		t.Fatalf("redacted error=%q", got)
	}
	if got := SafeError(errors.New(`Post "https://example.com/private?token=value": failed`)); got != `Post "https://example.com": failed` {
		t.Fatalf("quoted URL error=%q", got)
	}
	if len(SafeError(errors.New(strings.Repeat("x", 1200)))) != 1000 {
		t.Fatal("error length was not bounded")
	}
}
