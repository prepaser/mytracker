package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/metrics"
)

type requestIDKey struct{}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.Cfg.Snapshot()
		requestID, _ := config.RandToken(12)
		w.Header().Set("X-Request-ID", requestID)
		securityHeaders(w, r, cfg, s.secureCookie(r))
		if !validateAPIRequest(w, r, cfg) {
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, requestID))
		start := time.Now()
		rec := &statusWriter{ResponseWriter: w}
		if !rejectUnknownAPIMethod(rec, r) {
			s.mux.ServeHTTP(rec, r)
		}
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		stream := r.Pattern == "GET /api/events" || r.Pattern == "GET /share/{token}/api/events"
		metrics.HTTP(r.Method, status, time.Since(start), stream)
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			slog.Info("http request", "request_id", requestID, "method", r.Method,
				"route", route, "status", status, "bytes", rec.bytes,
				"duration_ms", time.Since(start).Milliseconds())
		}
	})
}

func rejectUnknownAPIMethod(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return false
	default:
		w.Header().Set("Allow", "GET, HEAD, POST, PUT, PATCH, DELETE")
		jsonErr(w, fmt.Errorf("API method not allowed"), http.StatusMethodNotAllowed)
		return true
	}
}

func securityHeaders(w http.ResponseWriter, r *http.Request, cfg *config.Config, secure bool) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Origin-Agent-Cluster", "?1")
	if secure {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	if !strings.HasPrefix(r.URL.Path, "/static/") {
		w.Header().Set("Cache-Control", "no-store")
	}
	imgSrc := "'self' data:"
	tileURL := cfg.TileURL
	if tileURL != "" {
		if u, err := url.Parse(strings.ReplaceAll(tileURL, "{s}.", "*.")); err == nil && u.Host != "" {
			imgSrc += " " + u.Scheme + "://" + u.Host
		}
	}
	frameAncestors := "'none'"
	if r.URL.Path == "/api/export" || r.URL.Path == "/api/export/gpx" {
		frameAncestors = "'self'"
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src "+imgSrc+"; connect-src 'self'; frame-ancestors "+frameAncestors+"; base-uri 'self'; form-action 'self'")
}

func validateAPIRequest(w http.ResponseWriter, r *http.Request, cfg *config.Config) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		if r.Method == http.MethodPost && isFormEndpoint(r.URL.Path) {
			contentType := r.Header.Get("Content-Type")
			if contentType != "" && !mediaTypeIs(contentType, "application/x-www-form-urlencoded") {
				http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
				return false
			}
			limit := int64(64 << 10)
			if r.ContentLength > limit {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return false
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		return true
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPatch && r.Method != http.MethodPut {
		return true
	}
	contentType := r.Header.Get("Content-Type")
	allowed := contentType == "" && r.ContentLength == 0 ||
		mediaTypeIs(contentType, "application/x-www-form-urlencoded") ||
		mediaTypeIs(contentType, "application/json")
	if r.URL.Path == "/api/import" || r.URL.Path == "/api/import/gpx" {
		allowed = mediaTypeIs(contentType, "multipart/form-data")
	}
	if !allowed {
		jsonErr(w, fmt.Errorf("unsupported content type"), http.StatusUnsupportedMediaType)
		return false
	}
	limit := int64(1 << 20)
	switch r.URL.Path {
	case "/api/settings":
		limit = 64 << 10
	case "/api/import", "/api/import/gpx":
		limit = cfg.ImportMaxBytes + (1 << 20)
	}
	if r.ContentLength > limit {
		jsonErr(w, fmt.Errorf("request body too large"), http.StatusRequestEntityTooLarge)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return true
}

func mediaTypeIs(contentType, want string) bool {
	got, _, err := mime.ParseMediaType(contentType)
	return err == nil && got == want
}

func isFormEndpoint(path string) bool {
	switch path {
	case "/login", "/logout", "/settings":
		return true
	default:
		return false
	}
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonOKStatus(w http.ResponseWriter, v any, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	msg := err.Error()
	if code >= 500 {
		slog.Error("api error", "request_id", w.Header().Get("X-Request-ID"), "status", code, "code", httpErrorCode(code))
		msg = "internal error"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":       httpErrorCode(code),
			"message":    msg,
			"request_id": w.Header().Get("X-Request-ID"),
		},
	})
}

func httpErrorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusRequestEntityTooLarge:
		return "too_large"
	case http.StatusTooManyRequests:
		return "rate_limited"
	default:
		if status >= 500 {
			return "internal_error"
		}
		return "request_error"
	}
}

func rateLimited(w http.ResponseWriter, retry time.Duration) {
	if retry < time.Second {
		retry = time.Second
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
	http.Error(w, "too many requests", http.StatusTooManyRequests)
}
