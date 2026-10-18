package slogx

import (
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
)

var (
	urlPattern    = regexp.MustCompile(`https?://[^\s"'<>]+`)
	secretPattern = regexp.MustCompile(`(?i)(password|token|secret)=([^&\s]+)`)
)

func Init(level string) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(level)})))
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func SafeError(err error) string {
	if err == nil {
		return ""
	}
	msg := urlPattern.ReplaceAllStringFunc(err.Error(), func(raw string) string {
		end := ""
		for len(raw) > 0 && strings.ContainsRune(`.,;:)]}`, rune(raw[len(raw)-1])) {
			end = raw[len(raw)-1:] + end
			raw = raw[:len(raw)-1]
		}
		u, parseErr := url.Parse(raw)
		if parseErr != nil || u.Host == "" {
			return "[redacted-url]" + end
		}
		return u.Scheme + "://" + u.Host + end
	})
	msg = secretPattern.ReplaceAllString(msg, "$1=[redacted]")
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	return msg
}
