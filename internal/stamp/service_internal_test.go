package stamp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	libots "github.com/nbd-wtf/opentimestamps"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/ots"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/tsa"
	"github.com/prepaser/mytracker/internal/worker"
)

func TestWorkerPanicReturnsJobToFailed(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.StampWorkers = 1
	cfg.StampRetries = 1
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	digest := sha256.Sum256([]byte("panic"))
	id, err := st.Insert(&store.Location{
		User: "a", Device: "panic", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: digest[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobMaxAttempts(id, 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	svc := &Service{
		cfg: config.NewManager(cfg, nil), store: st, ctx: ctx, cancel: cancel,
		wake: worker.NewSignal(), heartbeat: worker.NewHeartbeat(time.Now),
		tsaStamp: func(context.Context, *config.Config, [32]byte) (*tsa.Result, error) {
			panic("injected")
		},
		otsStamp: func(context.Context, *config.Config, [32]byte) ([]byte, error) {
			return nil, errors.New("injected")
		},
	}
	svc.resize(1)
	defer svc.Shutdown()
	svc.signal()
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, err := st.JobStatus()
		if err != nil {
			t.Fatal(err)
		}
		if status.Failed == 2 && status.Running == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("jobs=%+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	jobs, err := st.ListLocationStampJobs(id)
	if err != nil {
		t.Fatal(err)
	}
	if jobs[0].LeaseOwner != "" || jobs[0].LeaseUntil != 0 {
		t.Fatalf("lease retained: %+v", jobs[0])
	}
}

func TestRunReadyReclaimsExpiredLease(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	mgr := config.NewManager(cfg, nil)
	svc := &Service{
		cfg: mgr, store: st, wake: worker.NewSignal(),
		tsaStamp: func(context.Context, *config.Config, [32]byte) (*tsa.Result, error) {
			return &tsa.Result{GenTime: time.Unix(100, 0), Token: []byte("tsa")}, nil
		},
		otsStamp: func(context.Context, *config.Config, [32]byte) ([]byte, error) {
			return []byte("ots"), nil
		},
	}
	digest := sha256.Sum256([]byte("reclaim"))
	id, err := st.Insert(&store.Location{
		User: "a", Device: "reclaim", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: digest[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if job, err := st.ClaimStampJob(now.Unix(), "dead", now.Add(-time.Second).Unix()); err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if !svc.runReady(context.Background(), "replacement") {
		t.Fatal("no work reclaimed")
	}
	loc, err := st.Get(id)
	if err != nil || loc.TsaStatus != "ok" || loc.OtsStatus != "submitted" {
		t.Fatalf("location=%+v err=%v", loc, err)
	}
	if svc.LeaseSeconds() != 90 {
		t.Fatalf("lease=%d", svc.LeaseSeconds())
	}
}

func TestSuccessfulDurableWorkersAndReload(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.StampWorkers = 1
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(cfg, nil)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	svc := New(mgr, st)
	defer func() { svc.Shutdown() }()
	svc.tsaStamp = func(context.Context, *config.Config, [32]byte) (*tsa.Result, error) {
		return &tsa.Result{GenTime: time.Unix(100, 0), Token: []byte("tsa")}, nil
	}
	svc.otsStamp = func(context.Context, *config.Config, [32]byte) ([]byte, error) {
		return []byte("ots"), nil
	}
	digest := sha256.Sum256([]byte("location"))
	id, err := st.Insert(&store.Location{
		User: "a", Device: "p", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: digest[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.Process(id, digest)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		loc, getErr := st.Get(id)
		if getErr == nil && loc.TsaStatus == "ok" && loc.OtsStatus == "submitted" && svc.QueueDepth() == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	loc, err := st.Get(id)
	if err != nil || loc.TsaStatus != "ok" || loc.OtsStatus != "submitted" || svc.QueueDepth() != 0 || !svc.Healthy() {
		t.Fatalf("loc=%+v err=%v depth=%d healthy=%t", loc, err, svc.QueueDepth(), svc.Healthy())
	}
	jobs, err := svc.EnqueueVerification(id)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("verification jobs=%+v err=%v", jobs, err)
	}
	if _, err := svc.LatestVerification(id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerificationHistory(id, 0, 10); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.VerificationSummary(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerificationPolicyStale(); err != nil {
		t.Fatal(err)
	}
	if !svc.VerificationHealthy() || svc.VerificationLeaseSeconds() != 90 {
		t.Fatalf("verify healthy=%v lease=%d", svc.VerificationHealthy(), svc.VerificationLeaseSeconds())
	}
	svc.TriggerVerification(id)
	svc.Shutdown()
	cfg.StampWorkers = 2
	svc = New(config.NewManager(cfg, nil), st)
	svc.Reload()
	svc.Shutdown()
	cfg.StampWorkers = 1
	svc = New(config.NewManager(cfg, nil), st)
	svc.Reload()
	if err := svc.Retry(id); err == nil {
		t.Fatal("completed jobs should not retry")
	}
}

func TestRetryAllAndUpgradeBatch(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	mgr := config.NewManager(cfg, st)
	svc := &Service{cfg: mgr, store: st, wake: worker.NewSignal()}

	failedID, err := st.Insert(&store.Location{
		User: "a", Device: "failed", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: make([]byte, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobMaxAttempts(failedID, 1); err != nil {
		t.Fatal(err)
	}
	for {
		job, err := st.ClaimStampJob(time.Now().Unix(), "test", time.Now().Add(leaseDuration).Unix())
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			break
		}
		if _, err := st.FailStampJob(job, errors.New("injected failure")); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := svc.RetryAll(); err != nil || n != 2 {
		t.Fatalf("retried=%d", n)
	}

	calendar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seq := libots.Sequence{{Attestation: &libots.Attestation{BitcoinBlockHeight: 123}}}
		_, _ = w.Write((libots.File{Sequences: []libots.Sequence{seq}}).SerializeInstructionSequences())
	}))
	defer calendar.Close()
	if _, err := mgr.Update(func(next *config.Settings) error {
		next.OTSCalendars = []string{calendar.URL}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("pending"))
	pendingID, err := st.ImportLocation(&store.Location{
		User: "a", Device: "pending", Tst: 2, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: digest[:],
		TsaStatus: "ok", OtsStatus: "submitted", CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	seq := libots.Sequence{{Attestation: &libots.Attestation{CalendarServerURL: calendar.URL}}}
	proof := (libots.File{Digest: digest[:], Sequences: []libots.Sequence{seq}}).SerializeToFile()
	if err := ots.SaveProof(cfg.ProofsDir(), pendingID, proof); err != nil {
		t.Fatal(err)
	}
	if out, err := svc.UpgradeBatch(context.Background(), 100, UpgradeCursor{}); err != nil || out.Upgraded != 1 {
		t.Fatalf("upgrade=%+v err=%v", out, err)
	}
	loc, _ := st.Get(pendingID)
	if loc.OtsStatus != "confirmed" {
		t.Fatalf("ots=%s", loc.OtsStatus)
	}
}

func TestUpgradeDoesNotConfirmProofWithoutBitcoinAnchor(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	digest := sha256.Sum256([]byte("zero-anchor"))
	id, err := st.ImportLocation(&store.Location{
		User: "u", Device: "zero-anchor", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: digest[:], OtsStatus: "submitted",
	})
	if err != nil {
		t.Fatal(err)
	}
	proof := (libots.File{
		Digest: digest[:],
		Sequences: []libots.Sequence{{{
			Attestation: &libots.Attestation{BitcoinBlockHeight: 1},
		}}},
	}).SerializeToFile()
	proof[len(proof)-1] = 0
	if err := ots.SaveProof(cfg.ProofsDir(), id, proof); err != nil {
		t.Fatal(err)
	}
	svc := &Service{cfg: config.NewManager(cfg, nil), store: st}
	if _, err := svc.UpgradeBatch(t.Context(), 1, UpgradeCursor{}); err != nil {
		t.Fatal(err)
	}
	loc, err := st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if loc.OtsStatus != "submitted" {
		t.Fatalf("ots=%s", loc.OtsStatus)
	}
}

func TestUpgradeBatchReportsCalendarOutage(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	cfg.OTSCalendars = []string{server.URL}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	digest := sha256.Sum256([]byte("calendar-outage"))
	id, err := st.ImportLocation(&store.Location{
		User: "u", Device: "outage", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: digest[:], OtsStatus: "submitted",
	})
	if err != nil {
		t.Fatal(err)
	}
	proof := (libots.File{
		Digest: digest[:],
		Sequences: []libots.Sequence{{{
			Attestation: &libots.Attestation{CalendarServerURL: server.URL},
		}}},
	}).SerializeToFile()
	if err := ots.SaveProof(cfg.ProofsDir(), id, proof); err != nil {
		t.Fatal(err)
	}
	svc := &Service{cfg: config.NewManager(cfg, nil), store: st}
	out, err := svc.UpgradeBatch(t.Context(), 1, UpgradeCursor{})
	if err == nil || out.Errors != 1 || len(out.FailedIDs) != 1 || out.FailedIDs[0] != id {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestProofReplacementRollsBackOnCompletionFailure(t *testing.T) {
	for _, kind := range []string{"tsa", "ots"} {
		t.Run(kind, func(t *testing.T) {
			svc, _, job, path, old := newClaimedStampService(t, kind)
			svc.completeJob = func(*store.StampJob, string, *int64, ...[]byte) error {
				return errors.New("injected completion failure")
			}
			if err := runStampJob(svc, job); err == nil {
				t.Fatal("completion failure was ignored")
			}
			assertProofUnchanged(t, path, old)
		})
	}
}

func TestProofReplacementRollsBackAfterLeaseLoss(t *testing.T) {
	for _, kind := range []string{"tsa", "ots"} {
		t.Run(kind, func(t *testing.T) {
			svc, st, job, path, old := newClaimedStampService(t, kind)
			svc.completeJob = func(j *store.StampJob, status string, ts *int64, nonce ...[]byte) error {
				future := time.Now().Unix() + int64(leaseDuration/time.Second) + 1
				claimed, err := st.ClaimStampJob(future, "replacement", future+int64(leaseDuration/time.Second))
				if err != nil {
					return err
				}
				if claimed == nil || claimed.ID != j.ID {
					return fmt.Errorf("unexpected replacement job: %+v", claimed)
				}
				return st.CompleteStampJob(j, status, ts, nonce...)
			}
			if err := runStampJob(svc, job); !errors.Is(err, store.ErrLeaseLost) {
				t.Fatalf("error=%v", err)
			}
			assertProofUnchanged(t, path, old)
		})
	}
}

func TestProofMutationLockHonorsStampContext(t *testing.T) {
	for _, kind := range []string{"tsa", "ots"} {
		t.Run(kind, func(t *testing.T) {
			svc, st, job, _, _ := newClaimedStampService(t, kind)
			held, err := st.LockProofMutationContext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			done := make(chan error, 1)
			go func() { done <- runStampJobContext(svc, job, ctx) }()
			select {
			case err := <-done:
				held()
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error=%v", err)
				}
			case <-time.After(time.Second):
				held()
				err := <-done
				t.Fatalf("lock ignored cancellation: %v", err)
			}
		})
	}
}

func TestProofReplacementCommitRemovesBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "1.tsr")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	replacement, err := stageProof(path, []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.commit(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("proof=%q err=%v", got, err)
	}
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".1.tsr.old-*")); err != nil || len(matches) != 0 {
		t.Fatalf("backup artifacts=%v err=%v", matches, err)
	}
}

func newClaimedStampService(t *testing.T, kind string) (*Service, *store.Store, *store.StampJob, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	digest := sha256.Sum256([]byte("replacement-" + kind))
	id, err := st.Insert(&store.Location{
		User: "a", Device: "replacement-" + kind, Tst: time.Now().UnixNano(), Lat: 1, Lon: 2,
		Payload: []byte(`{"v":1}`), Digest: digest[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListLocationStampJobs(id)
	if err != nil {
		t.Fatal(err)
	}
	var target *store.StampJob
	for i := range jobs {
		if jobs[i].Kind == kind {
			target = &jobs[i]
			break
		}
	}
	if target == nil {
		t.Fatalf("missing %s job", kind)
	}
	now := time.Now()
	claimed, err := st.ClaimStampJob(now.Unix(), "original", now.Add(leaseDuration).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.Kind != "tsa" {
		t.Fatalf("initial claim=%+v", claimed)
	}
	if kind == "tsa" {
		target = claimed
	} else {
		stampTime := now.Unix()
		if err := st.CompleteStampJob(claimed, "ok", &stampTime); err != nil {
			t.Fatal(err)
		}
		claimed, err = st.ClaimStampJob(time.Now().Unix(), "original", time.Now().Add(leaseDuration).Unix())
		if err != nil {
			t.Fatal(err)
		}
		if claimed == nil || claimed.Kind != kind {
			t.Fatalf("target claim=%+v", claimed)
		}
		target = claimed
	}
	path := TSAPath(cfg.ProofsDir(), id)
	if kind == "ots" {
		path = filepath.Join(cfg.ProofsDir(), fmt.Sprintf("%d.ots", id))
	}
	old := []byte("existing-proof-" + kind)
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(cfg, nil)
	svc := &Service{
		cfg: mgr, store: st,
		tsaStamp: func(context.Context, *config.Config, [32]byte) (*tsa.Result, error) {
			return &tsa.Result{GenTime: time.Unix(100, 0), Token: []byte("new-tsa")}, nil
		},
		otsStamp: func(context.Context, *config.Config, [32]byte) ([]byte, error) {
			return []byte("new-ots"), nil
		},
	}
	return svc, st, target, path, old
}

func runStampJob(svc *Service, job *store.StampJob) error {
	return runStampJobContext(svc, job, context.Background())
}

func runStampJobContext(svc *Service, job *store.StampJob, ctx context.Context) error {
	if job.Kind == "tsa" {
		return svc.doTSA(ctx, job)
	}
	return svc.doOTS(ctx, job)
}

func assertProofUnchanged(t *testing.T, path string, old []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read proof: %v", err)
	}
	if !bytes.Equal(got, old) {
		t.Fatalf("proof changed: got %q want %q", got, old)
	}
	for _, pattern := range []string{
		filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".old-*"),
		filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".new-*"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Fatalf("left replacement artifact %q", matches[0])
		}
	}
}

func TestRetryResult(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(cfg, nil)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	svc := &Service{cfg: mgr, store: st, wake: worker.NewSignal()}
	if _, err := svc.RetryResult(999); err == nil {
		t.Fatal("missing location retried")
	}
	id, err := st.Insert(&store.Location{
		User: "a", Device: "retry", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: make([]byte, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTSA(id, "fail", nil); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.RetryResult(id); err != nil || n != 2 {
		t.Fatalf("retried=%d err=%v", n, err)
	}
	if svc.QueueDepth() == 0 {
		t.Fatal("retry did not queue work")
	}
}

func TestUpgradeBatchAdvancesPastUnchangedProofs(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(cfg, nil)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	svc := New(mgr, st)
	defer svc.Shutdown()
	var ids []int64
	for i := range 3 {
		digest := sha256.Sum256([]byte(fmt.Sprintf("pending-%d", i)))
		id, err := st.ImportLocation(&store.Location{
			User: "u", Device: fmt.Sprintf("d-%d", i), Tst: int64(i + 1), Lat: 1, Lon: 2,
			Payload: []byte(`{}`), Digest: digest[:], TsaStatus: "ok", OtsStatus: "submitted",
		})
		if err != nil {
			t.Fatal(err)
		}
		seq := libots.Sequence{{Attestation: &libots.Attestation{CalendarServerURL: "http://127.0.0.1:1"}}}
		proof := (libots.File{Digest: digest[:], Sequences: []libots.Sequence{seq}}).SerializeToFile()
		if err := ots.SaveProof(cfg.ProofsDir(), id, proof); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	first, err := svc.UpgradeBatch(t.Context(), 2, UpgradeCursor{})
	if err != nil || first.Processed != 2 || first.LastID != ids[1] || first.Remaining == 0 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := svc.UpgradeBatch(t.Context(), 2, UpgradeCursor{AfterID: first.LastID})
	if err != nil || second.Processed != 1 || second.LastID != ids[2] || second.Remaining != 0 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func TestUpgradeBatchRetriesFailedIDsWithoutBlockingProgress(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(cfg, nil)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	svc := &Service{cfg: mgr, store: st}

	digest := sha256.Sum256([]byte("failed-upgrade"))
	failedID, err := st.ImportLocation(&store.Location{
		User: "u", Device: "failed", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: digest[:], OtsStatus: "submitted",
	})
	if err != nil {
		t.Fatal(err)
	}
	goodDigest := sha256.Sum256([]byte("successful-upgrade"))
	goodID, err := st.ImportLocation(&store.Location{
		User: "u", Device: "good", Tst: 2, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: goodDigest[:], OtsStatus: "submitted",
	})
	if err != nil {
		t.Fatal(err)
	}
	proof := (libots.File{
		Digest: goodDigest[:],
		Sequences: []libots.Sequence{{{
			Attestation: &libots.Attestation{BitcoinBlockHeight: 123},
		}}},
	}).SerializeToFile()
	if err := ots.SaveProof(cfg.ProofsDir(), goodID, proof); err != nil {
		t.Fatal(err)
	}

	first, err := svc.UpgradeBatch(context.Background(), 1, UpgradeCursor{})
	if err == nil || first.LastID != failedID || first.Errors != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := svc.UpgradeBatch(context.Background(), 1, UpgradeCursor{
		AfterID: first.LastID, RetryIDs: first.FailedIDs,
	})
	if err != nil || second.LastID != goodID || second.Upgraded != 1 || second.Processed != 1 || second.Errors != 0 || second.Remaining != 0 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	loc, err := st.Get(goodID)
	if err != nil || loc.OtsStatus != "confirmed" {
		t.Fatalf("location=%+v err=%v", loc, err)
	}
}

func TestUpgradeBatchKeepsTotalWorkWithinBudget(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(cfg, nil)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	svc := &Service{cfg: mgr, store: st}
	var ids []int64
	for i := range 4 {
		digest := sha256.Sum256([]byte(fmt.Sprintf("budget-%d", i)))
		id, err := st.ImportLocation(&store.Location{
			User: "u", Device: fmt.Sprintf("budget-%d", i), Tst: int64(i + 1), Lat: 1, Lon: 2,
			Payload: []byte(`{}`), Digest: digest[:], OtsStatus: "submitted",
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	out, err := svc.UpgradeBatch(context.Background(), 2, UpgradeCursor{RetryIDs: []int64{ids[3]}})
	if err == nil || out.Processed != 2 || out.Errors != 2 || out.Remaining != 4 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(out.FailedIDs) != 3 || out.FailedIDs[2] != ids[3] {
		t.Fatalf("deferred retry ids=%v", out.FailedIDs)
	}
}

func TestUpgradeBatchPreservesRetryIDsWhenCanceled(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	mgr := config.NewManager(cfg, nil)
	svc := &Service{cfg: mgr, store: st}
	digest := sha256.Sum256([]byte("canceled-retry"))
	id, err := st.ImportLocation(&store.Location{
		User: "u", Device: "canceled", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: digest[:], OtsStatus: "submitted",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := svc.UpgradeBatch(ctx, 1, UpgradeCursor{RetryIDs: []int64{id}})
	if !errors.Is(err, context.Canceled) || len(out.FailedIDs) != 1 || out.FailedIDs[0] != id {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestUpgradeDoesNotLeaveProofAfterLocationDeletion(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(cfg, nil)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	digest := sha256.Sum256([]byte("deleted-during-upgrade"))
	id, err := st.ImportLocation(&store.Location{
		User: "u", Device: "deleted", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: digest[:], OtsStatus: "submitted",
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		seq := libots.Sequence{{Attestation: &libots.Attestation{BitcoinBlockHeight: 123}}}
		_, _ = w.Write((libots.File{Sequences: []libots.Sequence{seq}}).SerializeInstructionSequences())
	}))
	defer srv.Close()
	cfg.OTSCalendars = []string{srv.URL}
	proof := (libots.File{
		Digest:    digest[:],
		Sequences: []libots.Sequence{{{Attestation: &libots.Attestation{CalendarServerURL: srv.URL}}}},
	}).SerializeToFile()
	if err := ots.SaveProof(cfg.ProofsDir(), id, proof); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(cfg.ProofsDir(), fmt.Sprintf("%d.ots", id)))
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{cfg: mgr, store: st}
	done := make(chan error, 1)
	go func() {
		_, err := svc.upgradeLocation(context.Background(), cfg, store.Location{
			ID: id, Digest: digest[:], OtsStatus: "submitted",
		})
		done <- err
	}()
	<-started
	if err := st.DeleteIDs([]int64{id}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("upgrade error=%v", err)
	}
	got, err := os.ReadFile(filepath.Join(cfg.ProofsDir(), fmt.Sprintf("%d.ots", id)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("deleted location proof was rewritten")
	}
	if _, err := st.ProcessProofGC(cfg.ProofsDir(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.ProofsDir(), fmt.Sprintf("%d.ots", id))); !os.IsNotExist(err) {
		t.Fatalf("orphan proof error=%v", err)
	}
}

func TestUpgradeBatchProofMutationLockHonorsContext(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(cfg, nil)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	digest := sha256.Sum256([]byte("upgrade-lock"))
	id, err := st.ImportLocation(&store.Location{
		User: "u", Device: "upgrade-lock", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: digest[:], OtsStatus: "submitted",
	})
	if err != nil {
		t.Fatal(err)
	}
	proof := (libots.File{
		Digest: digest[:],
		Sequences: []libots.Sequence{{{
			Attestation: &libots.Attestation{BitcoinBlockHeight: 123},
		}}},
	}).SerializeToFile()
	if err := ots.SaveProof(cfg.ProofsDir(), id, proof); err != nil {
		t.Fatal(err)
	}
	svc := &Service{cfg: mgr, store: st}
	held, err := st.LockProofMutationContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := svc.UpgradeBatch(ctx, 1, UpgradeCursor{})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		held()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		held()
		err := <-done
		t.Fatalf("upgrade lock ignored cancellation: %v", err)
	}
}
