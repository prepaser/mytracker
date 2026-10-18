package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/password"
	"github.com/prepaser/mytracker/internal/ratelimit"
	"github.com/prepaser/mytracker/internal/store"
)

const (
	loginBodyLimit = 8 << 10
	passwordMin    = 14
	passwordMax    = 256
)

func (s *Server) clientKey(r *http.Request) string {
	return ratelimit.ClientKey(r, s.Cfg.Snapshot().TrustedProxies)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.currentSession(r); err != nil {
			if !errors.Is(err, store.ErrSessionNotFound) {
				if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/metrics" {
					jsonErr(w, err, http.StatusServiceUnavailable)
				} else {
					http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				}
				return
			}
			if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/metrics" {
				jsonErr(w, fmt.Errorf("authentication required"), http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.validCSRF(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				jsonErr(w, fmt.Errorf("bad csrf token"), http.StatusForbidden)
			} else {
				http.Error(w, "bad csrf token", http.StatusForbidden)
			}
			return
		}
		next(w, r)
	}
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, err := s.currentSession(r); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	} else if !errors.Is(err, store.ErrSessionNotFound) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	page(w, "login.html", map[string]any{"Error": ""})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, loginBodyLimit)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	key := s.clientKey(r)
	s.loginMu.RLock()
	rl := s.loginRL
	s.loginMu.RUnlock()
	if ok, retry := rl.Allow(key); !ok {
		rateLimited(w, retry)
		return
	}
	pass := []byte(r.PostFormValue("password"))
	if len(pass) == 0 || len(pass) > passwordMax {
		slog.Warn("security login failed")
		page(w, "login.html", map[string]any{"Error": "login failed"})
		return
	}
	select {
	case s.loginSem <- struct{}{}:
		defer func() { <-s.loginSem }()
	default:
		http.Error(w, "login busy", http.StatusServiceUnavailable)
		return
	}
	cfg := s.Cfg.Snapshot()
	ok, rehash := password.Verify(cfg.AdminHash, pass)
	if !ok {
		slog.Warn("security login failed")
		page(w, "login.html", map[string]any{"Error": "login failed"})
		return
	}
	if rehash {
		hash, err := password.Hash(pass)
		if err != nil {
			http.Error(w, "login failed", http.StatusInternalServerError)
			return
		}
		cfg, err = s.Cfg.Update(func(next *config.Settings) error {
			if next.AdminHash != cfg.AdminHash {
				return errors.New("admin password changed during login")
			}
			next.AdminHash = hash
			return nil
		})
		if err != nil {
			http.Error(w, "login failed", http.StatusInternalServerError)
			return
		}
	}
	token, err := config.RandToken(43)
	if err != nil {
		http.Error(w, "login failed", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	if _, err := s.Store.CreateSessionForPassword(token, cfg.AdminHash, now.Unix(), now.Add(cfg.SessionTTL()).Unix()); err != nil {
		http.Error(w, "login failed", http.StatusInternalServerError)
		return
	}
	rl.Reset(key)
	setSessionCookie(w, r, token, cfg.SessionTTL(), s.secureCookie(r))
	if err := s.Cfg.RemoveInitialPassword(); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("initial password cleanup failed")
	}
	slog.Info("security login succeeded")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	var deleteErr error
	if c, err := r.Cookie("session"); err == nil {
		deleteErr = s.Store.DeleteSessionToken(c.Value)
	}
	clearSessionCookie(w, r, s.secureCookie(r))
	if deleteErr != nil {
		http.Error(w, "logout failed", http.StatusInternalServerError)
		return
	}
	slog.Info("security session revoked", "scope", "current")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, ttl time.Duration, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: "session", Value: token, Path: "/", HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteStrictMode, MaxAge: int(ttl.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: "session", Value: "", Path: "/", HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
}

func (s *Server) currentSession(r *http.Request) (*store.Session, error) {
	c, err := r.Cookie("session")
	if err != nil || len(c.Value) < 40 || len(c.Value) > 128 {
		return nil, store.ErrSessionNotFound
	}
	now := time.Now().Unix()
	cfg := s.Cfg.Snapshot()
	session, err := s.Store.GetSession(c.Value, now, sessionIdleSeconds(cfg))
	if err != nil {
		return nil, err
	}
	if now-session.LastSeenAt >= sessionTouchSeconds(cfg) {
		_ = s.Store.TouchSession(session.ID, now)
		session.LastSeenAt = now
	}
	return session, nil
}

func (s *Server) currentSessionValid(r *http.Request) bool {
	_, err := s.currentSession(r)
	return err == nil
}

func sessionIdleSeconds(cfg *config.Config) int64 {
	seconds := int64(cfg.SessionIdleDuration() / time.Second)
	if seconds < 2 {
		return 2
	}
	return seconds
}

func sessionTouchSeconds(cfg *config.Config) int64 {
	seconds := sessionIdleSeconds(cfg) / 2
	if seconds < 1 {
		return 1
	}
	if seconds > 5*60 {
		return 5 * 60
	}
	return seconds
}

func (s *Server) csrfToken(r *http.Request) string {
	c, err := r.Cookie("session")
	if err != nil || c.Value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("csrf:" + c.Value))
	return hex.EncodeToString(sum[:])
}

func (s *Server) validCSRF(r *http.Request) bool {
	want := s.csrfToken(r)
	if want == "" {
		return false
	}
	got := r.Header.Get("X-CSRF-Token")
	if got == "" {
		got = r.FormValue("csrf")
	}
	return hmac.Equal([]byte(got), []byte(want))
}

func (s *Server) apiSessions(w http.ResponseWriter, r *http.Request) {
	current, err := s.currentSession(r)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			jsonErr(w, fmt.Errorf("authentication required"), http.StatusUnauthorized)
		} else {
			jsonErr(w, err, http.StatusServiceUnavailable)
		}
		return
	}
	now := time.Now().Unix()
	sessions, err := s.Store.ListSessions(now, sessionIdleSeconds(s.Cfg.Snapshot()))
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, map[string]any{
			"id": session.ID, "created_at": session.CreatedAt, "last_seen_at": session.LastSeenAt,
			"expires_at": session.ExpiresAt, "current": session.ID == current.ID,
		})
	}
	jsonOK(w, out)
}

func (s *Server) apiSessionDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonErr(w, fmt.Errorf("invalid session id"), http.StatusBadRequest)
		return
	}
	if err := s.Store.DeleteSession(id); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, store.ErrSessionNotFound) {
			code = http.StatusNotFound
		}
		jsonErr(w, err, code)
		return
	}
	slog.Info("security session revoked", "session_id", id)
	jsonOK(w, map[string]bool{"ok": true})
}

func (s *Server) apiSessionsRevokeOthers(w http.ResponseWriter, r *http.Request) {
	current, err := s.currentSession(r)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			jsonErr(w, fmt.Errorf("authentication required"), http.StatusUnauthorized)
		} else {
			jsonErr(w, err, http.StatusServiceUnavailable)
		}
		return
	}
	if err := s.Store.DeleteOtherSessions(current.ID); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	slog.Info("security session revoked", "scope", "others")
	jsonOK(w, map[string]bool{"ok": true})
}

func (s *Server) secureCookie(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil &&
		ratelimit.IsTrustedProxy(ip, s.Cfg.Snapshot().TrustedProxies) &&
		strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
