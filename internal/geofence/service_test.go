package geofence

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
)

func TestServiceDeliversQueuedEvent(t *testing.T) {
	var calls atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer hook.Close()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.WebhookURL = hook.URL
	mgr := config.NewManager(cfg, nil)
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err = st.CreateGeofence(&store.Geofence{Name: "Home", Lat: 0, Lon: 0, RadiusM: 100, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for i, lat := range []float64{0, .01} {
		if _, err = st.Insert(&store.Location{User: "u", Device: "d", Tst: int64(i + 1), Lat: lat, Lon: 0, Payload: []byte(`{}`), Digest: make([]byte, 32)}); err != nil {
			t.Fatal(err)
		}
	}
	stoppedCtx, stopNow := context.WithCancel(context.Background())
	stopNow()
	stopped := &Service{ctx: stoppedCtx, st: st}
	stopped.wg.Add(1)
	stopped.run()
	svc := New(mgr, st)
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = svc.ShutdownContext(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	if !svc.Healthy() {
		t.Fatal("unhealthy heartbeat")
	}
}
