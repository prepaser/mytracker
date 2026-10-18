package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/store"
)

func TestJobListStorageFailureClassification(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st}
	for _, tc := range []struct {
		name string
		fn   http.HandlerFunc
	}{
		{"stamp", s.apiStampJobs},
		{"verification", s.apiVerificationJobs},
		{"maintenance", s.apiMaintenanceJobs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.fn(w, httptest.NewRequest(http.MethodGet, "/api/jobs?state=queued", nil))
			if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"message":"internal error"`) {
				t.Fatalf("storage failure: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestJobListInvalidFiltersRemainBadRequest(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "filters.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := &Server{Store: st}
	for _, tc := range []struct {
		name string
		fn   http.HandlerFunc
		path string
	}{
		{"stamp", s.apiStampJobs, "/api/stamp-jobs?state=bad"},
		{"verification", s.apiVerificationJobs, "/api/verification-jobs?state=bad"},
		{"maintenance-kind", s.apiMaintenanceJobs, "/api/maintenance-jobs?kind=bad"},
		{"maintenance-state", s.apiMaintenanceJobs, "/api/maintenance-jobs?state=bad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.fn(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("invalid filter: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
