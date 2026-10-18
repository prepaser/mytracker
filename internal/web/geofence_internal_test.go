package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/store"
)

func TestGeofenceStoreFailures(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st}
	const valid = `{"name":"fixture","lat":1,"lon":2,"radius_m":100,"enabled":true}`
	for _, tc := range []struct {
		name, body string
		handler    http.HandlerFunc
		status     int
	}{
		{"create", valid, s.apiGeofenceCreate, 500},
		{"update", valid, s.apiGeofenceUpdate, 500},
		{"enabled", `{"enabled":false}`, s.apiGeofenceUpdate, 500},
		{"delete", "", s.apiGeofenceDelete, 500},
		{"malformed", `{`, s.apiGeofenceCreate, 400},
		{"invalid-radius", strings.Replace(valid, `"radius_m":100`, `"radius_m":0`, 1), s.apiGeofenceCreate, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/geofences/999", strings.NewReader(tc.body))
			r.SetPathValue("id", "999")
			w := httptest.NewRecorder()
			tc.handler(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.status == 500 && (!strings.Contains(w.Body.String(), `"message":"internal error"`) || strings.Contains(w.Body.String(), "database is closed")) {
				t.Fatalf("store error exposed: %s", w.Body.String())
			}
		})
	}
}
