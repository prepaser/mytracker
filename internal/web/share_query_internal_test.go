package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
)

func TestShareQueryAdmissionLimits(t *testing.T) {
	s := &Server{}
	for range maxShareQueriesPerID {
		if !s.acquireShareQuery(1) {
			t.Fatal("share query rejected below per-link limit")
		}
	}
	if s.acquireShareQuery(1) {
		t.Fatal("share query accepted above per-link limit")
	}
	for range maxShareQueriesPerID {
		s.releaseShareQuery(1)
	}

	for id := int64(1); id <= maxShareQueries; id++ {
		if !s.acquireShareQuery(id) {
			t.Fatal("share query rejected below global limit")
		}
	}
	if s.acquireShareQuery(maxShareQueries + 1) {
		t.Fatal("share query accepted above global limit")
	}
	for id := int64(1); id <= maxShareQueries; id++ {
		s.releaseShareQuery(id)
	}
}

func TestBeginShareQueryCancellationReleasesAdmission(t *testing.T) {
	s := &Server{}
	parent, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(parent, http.MethodGet, "/share/token/api/track", nil)
	w := httptest.NewRecorder()
	ctx, done, ok := s.beginShareQuery(w, req, 1)
	if !ok {
		t.Fatal("share query was rejected")
	}
	cancel()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("query context did not observe request cancellation")
	}
	done()
	if s.shareQueries != 0 || len(s.shareQueryByID) != 0 {
		t.Fatalf("query admission leaked: total=%d by_id=%v", s.shareQueries, s.shareQueryByID)
	}
	if !s.acquireShareQuery(1) {
		t.Fatal("query admission was not released")
	}
	s.releaseShareQuery(1)
}

func TestShareQueryAdmissionResponse(t *testing.T) {
	s := &Server{}
	for id := int64(1); id <= maxShareQueries; id++ {
		if !s.acquireShareQuery(id) {
			t.Fatal("share query was rejected below global limit")
		}
	}
	rec := httptest.NewRecorder()
	_, _, ok := s.beginShareQuery(rec, httptest.NewRequest(http.MethodGet, "/share/token/api/track", nil), maxShareQueries+1)
	if ok || rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "10" {
		t.Fatalf("ok=%v code=%d headers=%v body=%s", ok, rec.Code, rec.Header(), rec.Body.String())
	}
	for id := int64(1); id <= maxShareQueries; id++ {
		s.releaseShareQuery(id)
	}
}

func TestWithShareReturnsServiceUnavailableOnStoreError(t *testing.T) {
	cfg := config.Default()
	st, err := store.Open(filepath.Join(t.TempDir(), "shares.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Server{Cfg: config.NewManager(cfg, st), Store: st, shareRL: shareLimiter(cfg)}
	req := httptest.NewRequest(http.MethodGet, "/share/token/api/track", nil)
	req.SetPathValue("token", "token")
	rec := httptest.NewRecorder()
	called := false
	s.withShare(rec, req, func(*store.ShareLink) { called = true })
	if called || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("called=%t status=%d body=%s", called, rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" || !strings.Contains(rec.Body.String(), `"code":"internal_error"`) {
		t.Fatalf("headers=%v body=%s", rec.Header(), rec.Body.String())
	}
}
