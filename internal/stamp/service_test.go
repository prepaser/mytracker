package stamp_test

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/stamp"
	"github.com/prepaser/mytracker/internal/store"
)

func TestProcessTSAFailure(t *testing.T) {
	tsaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer tsaSrv.Close()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.TSAURL = tsaSrv.URL
	cfg.OTSCalendars = []string{"http://127.0.0.1:1/nope"}
	cfg.StampRetries = 1
	cfg.StampWorkers = 2
	cfgPath := filepath.Join(dir, "config.toml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(cfg, nil)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	id, err := st.Insert(&store.Location{
		User: "a", Device: "p", Tst: time.Now().UnixNano(), Lat: 1, Lon: 1,
		Payload: []byte(`{}`), Digest: []byte{1, 2, 3},
	})
	if err != nil {
		t.Fatal(err)
	}

	svc := stamp.New(mgr, st)
	digest := sha256.Sum256([]byte("x"))
	svc.Process(id, digest)
	deadline := time.Now().Add(5 * time.Second)
	for {
		loc, err := st.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if loc.TsaStatus == "fail" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tsa status=%q", loc.TsaStatus)
		}
		time.Sleep(50 * time.Millisecond)
	}
	svc.Shutdown()
}

func TestTSAPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proofs")
	if got := stamp.TSAPath(dir, 42); got != filepath.Join(dir, "42.tsr") {
		t.Fatalf("tsa path: %s", got)
	}
}
