package webhook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSendDoesNotFollowRedirects(t *testing.T) {
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		var calls atomic.Int32
		destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			calls.Add(1)
		}))
		origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, destination.URL, code)
		}))
		previous := client
		trusted := *client
		trusted.Transport = origin.Client().Transport
		client = &trusted
		err := Send(t.Context(), origin.URL, "secret", "geofence.enter", map[string]any{"id": 7})
		client = previous
		origin.Close()
		destination.Close()
		if err == nil || !strings.Contains(err.Error(), "webhook status") || calls.Load() != 0 {
			t.Fatalf("status=%d destination_calls=%d err=%v", code, calls.Load(), err)
		}
	}
}
