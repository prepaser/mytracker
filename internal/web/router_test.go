package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/testutil"
)

func TestRouteContractMatrix(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	handler := srv.Handler()

	for _, tc := range []struct {
		method, path, contentType string
	}{
		{"GET", "/api/track", ""},
		{"GET", "/api/timeline", ""},
		{"GET", "/api/geofences", ""},
		{"GET", "/api/geofence-events", ""},
		{"GET", "/api/export/gpx", ""},
		{"GET", "/api/locations", ""},
		{"GET", "/api/identities", ""},
		{"GET", "/api/status", ""},
		{"GET", "/api/events", ""},
		{"GET", "/api/stamp-jobs", ""},
		{"GET", "/api/verification-jobs", ""},
		{"GET", "/api/maintenance-jobs", ""},
		{"GET", "/api/maintenance-runs", ""},
		{"POST", "/api/maintenance-jobs/1/retry", "application/json"},
		{"POST", "/api/stamps/1/retry", "application/json"},
		{"POST", "/api/stamps/retry", "application/json"},
		{"POST", "/api/backups", "application/json"},
		{"POST", "/api/owntracks-url", "application/json"},
		{"GET", "/api/sessions", ""},
		{"DELETE", "/api/sessions/1", ""},
		{"POST", "/api/sessions/revoke-others", "application/json"},
		{"GET", "/api/stats", ""},
		{"POST", "/api/ots/upgrade", "application/json"},
		{"GET", "/api/export", ""},
		{"GET", "/api/shares", ""},
		{"POST", "/api/shares", "application/json"},
		{"POST", "/api/shares/1/rotate", "application/json"},
		{"DELETE", "/api/shares/1", ""},
		{"POST", "/api/import", "multipart/form-data; boundary=x"},
		{"GET", "/api/delete-preview", ""},
		{"POST", "/api/delete", "application/x-www-form-urlencoded"},
		{"GET", "/api/verify/1", ""},
		{"POST", "/api/verify/1", "application/json"},
		{"GET", "/api/verify/1/history", ""},
		{"GET", "/metrics", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.contentType != "" {
			req.Header.Set("Content-Type", tc.contentType)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: got=%d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Content-Type") != "application/json" ||
			!strings.Contains(rec.Body.String(), `"request_id"`) {
			t.Fatalf("%s %s contract: headers=%v body=%s", tc.method, tc.path, rec.Header(), rec.Body.String())
		}
	}

	for _, path := range []string{"/", "/map", "/settings"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Fatalf("GET %s: code=%d location=%q", path, rec.Code, rec.Header().Get("Location"))
		}
	}

	for _, tc := range []struct {
		path, contentType string
	}{
		{"/healthz", "application/json"},
		{"/readyz", "application/json"},
		{"/login", ""},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != tc.contentType {
			t.Fatalf("GET %s: code=%d content-type=%q", tc.path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}
