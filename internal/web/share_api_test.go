package web_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/testutil"
)

func TestShareDelayShiftsWholeWindow(t *testing.T) {
	_, st, _, _, srv, _ := testutil.Setup(t)
	now := time.Now()
	for i, age := range []time.Duration{150 * time.Minute, 90 * time.Minute, 30 * time.Minute} {
		if _, err := st.Insert(&store.Location{
			User: "u", Device: "d", Tst: now.Add(-age).Unix(), Lat: float64(i + 1), Lon: 2,
			Payload: []byte(`{}`), Digest: []byte{byte(i + 1)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	link := &store.ShareLink{User: "u", Device: "d", Hours: 1, DelayMinutes: 120, PrecisionM: 100}
	if _, err := st.CreateShare(link); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/share/"+link.Token+"/api/track", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Metadata struct {
			Total int `json:"total_points"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Metadata.Total != 1 {
		t.Fatalf("delayed one-hour window returned %d points, want one point from 2-3 hours ago", body.Metadata.Total)
	}
}
