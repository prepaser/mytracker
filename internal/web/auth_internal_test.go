package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
	"golang.org/x/crypto/bcrypt"
)

type passwordRotatingConfigManager struct {
	ConfigManager
	replacement string
}

func (m *passwordRotatingConfigManager) Update(fn func(*config.Settings) error) (*config.Config, error) {
	if _, err := m.ConfigManager.Update(func(next *config.Settings) error {
		next.AdminHash = m.replacement
		return nil
	}); err != nil {
		return nil, err
	}
	return m.ConfigManager.Update(fn)
}

func TestLoginRejectsPasswordChangeDuringRehash(t *testing.T) {
	pass := "the original password"
	oldHash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte("a different password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AdminHash = string(oldHash)
	cfg.PubPass = "pub-secret"
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	data, err := json.Marshal(cfg.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSettings(data, false); err != nil {
		t.Fatal(err)
	}
	mgr := &passwordRotatingConfigManager{
		ConfigManager: config.NewManager(cfg, st),
		replacement:   string(newHash),
	}
	s := &Server{Cfg: mgr, Store: st, loginRL: newLoginRL(cfg), loginSem: make(chan struct{}, 2)}
	form := url.Values{"password": {pass}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleLogin(rec, req)
	if rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) != "login failed" {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if count, err := st.SessionCount(); err != nil || count != 0 {
		t.Fatalf("sessions=%d err=%v", count, err)
	}
}

func TestSessionIdleIntervalsUseWholeSeconds(t *testing.T) {
	cfg := config.Default()
	cfg.SessionIdle = "500ms"
	if got := sessionIdleSeconds(cfg); got != 2 || sessionTouchSeconds(cfg) != 1 {
		t.Fatalf("idle seconds=%d", got)
	}
	cfg.SessionIdle = "2s"
	if got := sessionIdleSeconds(cfg); got != 2 || sessionTouchSeconds(cfg) != 1 {
		t.Fatalf("2s idle=%d touch=%d", got, sessionTouchSeconds(cfg))
	}
	cfg.SessionIdle = "2.5s"
	if got := sessionIdleSeconds(cfg); got != 2 || sessionTouchSeconds(cfg) != 1 {
		t.Fatalf("2.5s idle=%d touch=%d", got, sessionTouchSeconds(cfg))
	}
	cfg.SessionIdle = "2m"
	if got := sessionIdleSeconds(cfg); got != 120 || sessionTouchSeconds(cfg) != 60 {
		t.Fatalf("idle=%d touch=%d", got, sessionTouchSeconds(cfg))
	}
	cfg.SessionIdle = "24h"
	if got := sessionTouchSeconds(cfg); got != 5*60 {
		t.Fatalf("long idle touch=%d", got)
	}
}

func TestSessionIdleTwoMinutesRefreshesActiveSession(t *testing.T) {
	cfg := config.Default()
	cfg.SessionIdle = "2m"
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now().Unix()
	token := strings.Repeat("a", 43)
	if _, err := st.CreateSession(token, now-70, now+3600); err != nil {
		t.Fatal(err)
	}
	s := &Server{Cfg: config.NewManager(cfg, st), Store: st}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: token})
	session, err := s.currentSession(req)
	if err != nil || session.LastSeenAt < now {
		t.Fatalf("session=%+v err=%v", session, err)
	}
}

func TestAuthReturnsServiceUnavailableOnSessionStoreError(t *testing.T) {
	cfg := config.Default()
	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Server{Cfg: config.NewManager(cfg, st), Store: st}
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: strings.Repeat("a", 43)})
	rec := httptest.NewRecorder()
	called := false
	s.auth(func(http.ResponseWriter, *http.Request) { called = true })(rec, req)
	if called || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("called=%t status=%d body=%s", called, rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" || !strings.Contains(rec.Body.String(), `"code":"internal_error"`) {
		t.Fatalf("headers=%v body=%s", rec.Header(), rec.Body.String())
	}
}
