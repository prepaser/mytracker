package proofverify_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/proofverify"
	"github.com/prepaser/mytracker/internal/store"
)

func TestVerifyMissingInvalidAndStale(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = root
	cfg.EsploraURLs = []string{"http://127.0.0.1:1"}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	raw := map[string]any{
		"_type": "location", "tst": float64(123), "lat": 37.5, "lon": 127.0,
	}
	payload, _ := json.Marshal(raw)
	rec, err := canonical.FromLocation("u", "d", raw)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := rec.Digest()
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Insert(&store.Location{
		User: "u", Device: "d", Tst: 123, Lat: 37.5, Lon: 127,
		Payload: payload, Digest: digest[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(cfg, st)
	svc := proofverify.New(mgr, st)
	defer svc.Shutdown()

	if _, err := svc.EnqueueAll(id, "manual"); err != nil {
		t.Fatal(err)
	}
	waitLatestResult(t, svc, id, "tsa", "missing")
	waitLatestResult(t, svc, id, "ots", "missing")
	view := waitVerificationIdle(t, svc, id)
	if view.TSA.Result != "missing" || view.OTS.Result != "missing" ||
		view.TSAStale || view.OTSStale {
		t.Fatalf("missing=%+v", view)
	}
	if _, err := mgr.Update(func(next *config.Settings) error {
		next.OTSConfirmations = 7
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	view, err = svc.Latest(id)
	if err != nil || !view.OTSStale {
		t.Fatalf("policy stale=%+v err=%v", view, err)
	}
	if _, err := mgr.Update(func(next *config.Settings) error {
		next.OTSConfirmations = 6
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ProofsDir(), "1.tsr"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	view, err = svc.Latest(id)
	if err != nil || (!view.TSAStale && view.TSA.Result != "invalid") {
		t.Fatalf("stale=%+v err=%v", view, err)
	}
	if _, _, err := svc.Enqueue(id, "tsa", "manual"); err != nil {
		t.Fatal(err)
	}
	view = waitLatestResult(t, svc, id, "tsa", "invalid")
	err = nil
	if err != nil || view.TSA.Result != "invalid" {
		t.Fatalf("invalid=%+v err=%v", view, err)
	}
	history, err := svc.History(id, 0, 20)
	if err != nil || len(history) < 3 {
		t.Fatalf("history=%d err=%v", len(history), err)
	}
}

func waitVerificationIdle(t *testing.T, svc *proofverify.Service, id int64) *proofverify.View {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, err := svc.Latest(id)
		if err == nil {
			idle := len(view.Jobs) > 0
			for _, job := range view.Jobs {
				idle = idle && (job.State == "done" || job.State == "failed")
			}
			if idle {
				return view
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	view, err := svc.Latest(id)
	t.Fatalf("verification did not become idle: %+v err=%v", view, err)
	return nil
}

func waitLatestResult(t *testing.T, svc *proofverify.Service, id int64, kind, result string) *proofverify.View {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, err := svc.Latest(id)
		if err == nil {
			run := view.TSA
			if kind == "ots" {
				run = view.OTS
			}
			if run != nil && run.Result == result {
				return view
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	view, err := svc.Latest(id)
	t.Fatalf("%s result=%+v err=%v", kind, view, err)
	return nil
}

func TestDurableQueueLifecycle(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = root
	cfg.EsploraURLs = []string{"http://127.0.0.1:1"}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id := insertLocation(t, st, 456)
	mgr := config.NewManager(cfg, nil)
	svc := proofverify.New(mgr, st)
	defer svc.Shutdown()
	if _, err := svc.EnqueueAll(999999, "manual"); err == nil {
		t.Fatal("missing location enqueued")
	}
	if _, err := svc.Latest(999999); err == nil {
		t.Fatal("missing latest returned")
	}
	if _, err := svc.History(999999, 0, 10); err == nil {
		t.Fatal("missing history returned")
	}
	if _, _, err := svc.Enqueue(id, "bad", "manual"); err == nil {
		t.Fatal("invalid kind enqueued")
	}

	jobs, err := svc.EnqueueAll(id, "manual")
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	if _, _, err := svc.Enqueue(id, "tsa", "stamp"); err != nil {
		t.Fatal(err)
	}
	waitDurableQueue(t, svc, id)
	if n, err := svc.PolicyStaleCount(); err != nil || n != 0 {
		t.Fatalf("policy stale=%d err=%v", n, err)
	}
	if !svc.Healthy() || svc.LeaseSeconds() != 90 {
		t.Fatalf("healthy=%v lease=%d", svc.Healthy(), svc.LeaseSeconds())
	}
	svc.Reload()
	svc.Trigger(id, "bogus", "bad")
	svc.Shutdown()
	if _, _, err := svc.Enqueue(id, "tsa", "manual"); !errors.Is(err, context.Canceled) {
		t.Fatalf("enqueue after shutdown=%v", err)
	}
}

func TestDurableQueueRetriesProofReadErrors(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = root
	cfg.EsploraURLs = []string{"http://127.0.0.1:1"}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id := insertLocation(t, st, 789)
	if err := os.Mkdir(filepath.Join(cfg.ProofsDir(), "1.tsr"), 0700); err != nil {
		t.Fatal(err)
	}
	svc := proofverify.New(config.NewManager(cfg, nil), st)
	defer svc.Shutdown()
	if _, _, err := svc.Enqueue(id, "tsa", "manual"); err != nil {
		t.Fatal(err)
	}
	waitJobState(t, st, id, "tsa", "retry")
	run, err := st.LatestVerification(id, "tsa")
	if err != nil || run.Result != "error" || run.ErrorCode != "proof_read" {
		t.Fatalf("run=%+v err=%v", run, err)
	}
}

func insertLocation(t *testing.T, st *store.Store, tst int64) int64 {
	t.Helper()
	raw := map[string]any{
		"_type": "location", "tst": float64(tst), "lat": 37.5, "lon": 127.0,
	}
	payload, _ := json.Marshal(raw)
	rec, err := canonical.FromLocation("u", "d", raw)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := rec.Digest()
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Insert(&store.Location{
		User: "u", Device: "d", Tst: tst, Lat: 37.5, Lon: 127,
		Payload: payload, Digest: digest[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func waitJobState(t *testing.T, st *store.Store, id int64, kind, state string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := st.ListLocationVerificationJobs(id)
		if err == nil {
			for _, job := range jobs {
				if job.Kind == kind && job.State == state {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	jobs, err := st.ListLocationVerificationJobs(id)
	t.Fatalf("%s state %s not reached: jobs=%+v err=%v", kind, state, jobs, err)
}

func waitDurableQueue(t *testing.T, svc *proofverify.Service, id int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, viewErr := svc.Latest(id)
		stats, stale, summaryErr := svc.Summary()
		status, statusErr := svc.JobStatus()
		done := viewErr == nil && len(view.Jobs) == 2 && view.TSA != nil && view.OTS != nil &&
			view.TSA.Result == "missing" && view.OTS.Result == "missing"
		if done {
			for _, job := range view.Jobs {
				done = done && job.State == "done"
			}
		}
		if done && summaryErr == nil && stale == 0 && stats.Results["missing"] == 2 &&
			statusErr == nil && status.Queued == 0 && status.Running == 0 && status.Retry == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	view, viewErr := svc.Latest(id)
	stats, stale, summaryErr := svc.Summary()
	status, statusErr := svc.JobStatus()
	t.Fatalf("durable queue did not settle: view=%+v view_err=%v stats=%+v stale=%d summary_err=%v status=%+v status_err=%v",
		view, viewErr, stats, stale, summaryErr, status, statusErr)
}
