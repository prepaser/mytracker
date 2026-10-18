package maintenance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/stamp"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/worker"
)

func maintenanceTestDeps(t *testing.T) (*config.Manager, *store.Store, *stamp.Service) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.TSAURL = "http://127.0.0.1:1"
	cfg.OTSCalendars = []string{"http://127.0.0.1:1"}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := config.NewManager(cfg, st)
	stampSvc := stamp.New(mgr, st)
	t.Cleanup(stampSvc.Shutdown)
	return mgr, st, stampSvc
}

func bareService(cfg *config.Manager, st *store.Store, stampSvc *stamp.Service) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		cfg: cfg, store: st, stamp: stampSvc, ctx: ctx, cancel: cancel,
		wake: worker.NewSignal(), reload: worker.NewSignal(), now: time.Now,
	}
	s.heartbeat = worker.NewHeartbeat(func() time.Time { return s.now() })
	s.accepting.Store(true)
	return s
}

func TestHandleJobCoalescesFollowUpAndStoresRun(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	svc := bareService(cfg, st, stampSvc)
	svc.run = func(context.Context, string) (map[string]any, error) {
		if _, _, err := svc.Enqueue("backup", "manual"); err != nil {
			t.Fatal(err)
		}
		return map[string]any{"file": "backup.mtbackup"}, nil
	}
	if _, _, err := svc.Enqueue("backup", "manual"); err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimMaintenanceJob(time.Now().Unix(), "owner", time.Now().Add(time.Minute).Unix())
	if err != nil || job == nil {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	svc.handleJob(job)
	jobs, _ := st.ListMaintenanceJobs("backup", "")
	runs, _ := st.ListMaintenanceRuns(0, 10)
	if len(jobs) != 1 || jobs[0].State != "queued" || jobs[0].Generation != 2 || len(runs) != 1 {
		t.Fatalf("jobs=%+v runs=%+v", jobs, runs)
	}
}

func TestOTSBatchYieldDoesNotLoopImmediately(t *testing.T) {
	cases := []struct {
		name      string
		ctx       func() context.Context
		run       func(context.Context) (stamp.UpgradeResult, error)
		wantErr   error
		wantYield bool
	}{
		{
			name: "budget",
			ctx:  context.Background,
			run: func(ctx context.Context) (stamp.UpgradeResult, error) {
				<-ctx.Done()
				return stamp.UpgradeResult{Remaining: 1}, ctx.Err()
			},
			wantYield: true,
		},
		{
			name: "provider deadline",
			ctx:  context.Background,
			run: func(context.Context) (stamp.UpgradeResult, error) {
				return stamp.UpgradeResult{}, context.DeadlineExceeded
			},
			wantErr: context.DeadlineExceeded,
		},
		{
			name: "budget interruption",
			ctx:  context.Background,
			run: func(ctx context.Context) (stamp.UpgradeResult, error) {
				<-ctx.Done()
				return stamp.UpgradeResult{}, errors.New("interrupted database query")
			},
			wantYield: true,
		},
		{
			name: "parent cancellation",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			run: func(ctx context.Context) (stamp.UpgradeResult, error) {
				return stamp.UpgradeResult{}, ctx.Err()
			},
			wantErr: context.Canceled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			budget := otsBatchBudget
			if tc.wantYield {
				budget = time.Nanosecond
			}
			summary, err := runOTSBatchWithBudget(tc.ctx(), budget, tc.run)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err=%v want=%v", err, tc.wantErr)
				}
				return
			}
			if err != nil || summary["yielded"] != tc.wantYield {
				t.Fatalf("summary=%+v err=%v", summary, err)
			}
			if _, ok := summary["pending"]; ok {
				t.Fatal("yield reported an unrefreshed pending count")
			}
		})
	}
}

func TestHandleJobYieldSchedulesNextPass(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	svc := bareService(cfg, st, stampSvc)
	now := time.Unix(1000, 0)
	svc.now = func() time.Time { return now }
	svc.run = func(context.Context, string) (map[string]any, error) {
		return map[string]any{"yielded": true, "remaining": 1}, nil
	}
	if _, _, err := svc.Enqueue("ots_upgrade", "manual"); err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimMaintenanceJob(now.Unix(), "owner", now.Add(leaseDuration).Unix())
	if err != nil || job == nil {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	if _, _, err := svc.Enqueue("backup", "manual"); err != nil {
		t.Fatal(err)
	}
	svc.handleJob(job)
	jobs, err := st.ListMaintenanceJobs("ots_upgrade", "")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	if jobs[0].State != "scheduled" || jobs[0].NextRunAt != now.Add(otsYieldDelay).Unix() {
		t.Fatalf("job=%+v", jobs[0])
	}
	runs, err := st.ListMaintenanceRuns(0, 1)
	if err != nil || len(runs) != 1 || runs[0].Result != "success" {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	if !strings.Contains(string(runs[0].Summary), `"yielded":true`) {
		t.Fatalf("yield not recorded: %s", runs[0].Summary)
	}
	next, err := st.ClaimMaintenanceJob(now.Unix(), "next-owner", now.Add(leaseDuration).Unix())
	if err != nil || next == nil || next.Kind != "backup" {
		t.Fatalf("queued backup did not get a turn: job=%+v err=%v", next, err)
	}
}

func TestHandleJobPanicAndCancellationRetry(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	svc := bareService(cfg, st, stampSvc)
	svc.run = func(context.Context, string) (map[string]any, error) {
		panic("injected")
	}
	if _, _, err := svc.Enqueue("proof_gc", "manual"); err != nil {
		t.Fatal(err)
	}
	job, _ := st.ClaimMaintenanceJob(time.Now().Unix(), "owner", time.Now().Add(time.Minute).Unix())
	svc.handleJob(job)
	jobs, _ := st.ListMaintenanceJobs("proof_gc", "retry")
	if len(jobs) != 1 || jobs[0].LastError == "" {
		t.Fatalf("panic jobs=%+v", jobs)
	}

	svc.run = func(ctx context.Context, _ string) (map[string]any, error) {
		return nil, ctx.Err()
	}
	job, _ = st.ClaimMaintenanceJob(time.Now().Add(time.Minute).Unix(), "owner-2", time.Now().Add(2*time.Minute).Unix())
	svc.cancel()
	svc.handleJob(job)
	runs, _ := st.ListMaintenanceRuns(0, 10)
	if len(runs) != 2 || runs[0].Result != "canceled" {
		t.Fatalf("runs=%+v", runs)
	}
}

func TestServiceInitialScheduleReloadAndShutdown(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	svc := New(cfg, st, stampSvc)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		jobs, _ := st.ListMaintenanceJobs("", "")
		if len(jobs) == 5 && svc.Healthy() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	jobs, _ := st.ListMaintenanceJobs("", "")
	if len(jobs) != 5 {
		t.Fatalf("jobs=%+v", jobs)
	}
	retentionJobs, _ := st.ListMaintenanceJobs("retention", "")
	if len(retentionJobs) != 1 || retentionJobs[0].State != "disabled" {
		t.Fatalf("retention=%+v", retentionJobs)
	}
	if _, err := cfg.Update(func(next *config.Settings) error {
		next.RetentionDays = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc.Reload()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		retentionJobs, _ = st.ListMaintenanceJobs("retention", "")
		if len(retentionJobs) == 1 && retentionJobs[0].State != "disabled" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := svc.ShutdownContext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Enqueue("backup", "manual"); !errors.Is(err, context.Canceled) {
		t.Fatalf("enqueue after shutdown=%v", err)
	}
}

func TestReconcileIdleDoesNotWriteJobs(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	svc := bareService(cfg, st, stampSvc)
	svc.now = func() time.Time { return time.Unix(100, 0) }

	svc.reconcile()
	before, err := st.ListMaintenanceJobs("", "")
	if err != nil || len(before) != 5 {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	svc.reconcile()
	after, err := st.ListMaintenanceJobs("", "")
	if err != nil || len(after) != len(before) {
		t.Fatalf("after=%+v err=%v", after, err)
	}
	for i := range before {
		if after[i].UpdatedAt != before[i].UpdatedAt {
			t.Fatalf("idle reconcile wrote %s: before=%+v after=%+v", after[i].Kind, before[i], after[i])
		}
	}
}

func TestReconcilePreservesTerminalBackupFailureCooldown(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	svc := bareService(cfg, st, stampSvc)
	t.Cleanup(svc.cancel)
	now := time.Unix(1000, 0)
	svc.now = func() time.Time { return now }
	svc.run = func(context.Context, string) (map[string]any, error) {
		return map[string]any{}, nil
	}
	if _, _, err := svc.Enqueue("backup", "manual"); err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimMaintenanceJob(now.Unix(), "initial", now.Add(leaseDuration).Unix())
	if err != nil || job == nil {
		t.Fatalf("initial claim=%+v err=%v", job, err)
	}
	svc.handleJob(job)
	jobs, err := st.ListMaintenanceJobs("backup", "")
	if err != nil || len(jobs) != 1 || jobs[0].State != "scheduled" {
		t.Fatalf("after success=%+v err=%v", jobs, err)
	}
	now = time.Unix(jobs[0].NextRunAt, 0)
	if _, err := st.EnsureMaintenanceJob("backup", true, now.Unix(), now.Unix()); err != nil {
		t.Fatal(err)
	}
	svc.run = func(context.Context, string) (map[string]any, error) {
		return nil, errors.New("backup failed")
	}
	for attempt := 1; attempt <= 5; attempt++ {
		job, err = st.ClaimMaintenanceJob(now.Unix(), fmt.Sprintf("failure-%d", attempt), now.Add(leaseDuration).Unix())
		if err != nil || job == nil {
			t.Fatalf("claim %d=%+v err=%v", attempt, job, err)
		}
		svc.handleJob(job)
		jobs, err = st.ListMaintenanceJobs("backup", "")
		if err != nil || len(jobs) != 1 {
			t.Fatalf("after failure %d=%+v err=%v", attempt, jobs, err)
		}
		if attempt < 5 {
			now = time.Unix(jobs[0].NextRunAt, 0)
		}
	}
	failed := jobs[0]
	if failed.State != "failed" || failed.Attempts != failed.MaxAttempts {
		t.Fatalf("terminal failure=%+v", failed)
	}
	wantNext := now.Add(24 * time.Hour).Unix()
	if failed.NextRunAt != wantNext {
		t.Fatalf("terminal next=%d want=%d", failed.NextRunAt, wantNext)
	}
	svc.reconcile()
	jobs, err = st.ListMaintenanceJobs("backup", "")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("after reconcile=%+v err=%v", jobs, err)
	}
	got := jobs[0]
	if got.State != "failed" || got.Attempts != failed.Attempts || got.NextRunAt != failed.NextRunAt {
		t.Fatalf("reconcile changed terminal failure=%+v before=%+v", got, failed)
	}
	if _, err := cfg.Update(func(next *config.Settings) error {
		next.BackupInterval = "12h"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc.reconcile()
	jobs, err = st.ListMaintenanceJobs("backup", "")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("after interval change=%+v err=%v", jobs, err)
	}
	updated := jobs[0]
	wantNext = time.Unix(updated.LastFinishedAt, 0).Add(12 * time.Hour).Unix()
	if updated.State != "failed" || updated.Attempts != failed.Attempts || updated.NextRunAt != wantNext {
		t.Fatalf("interval change lost failed cooldown=%+v want_next=%d", updated, wantNext)
	}
	now = time.Unix(updated.NextRunAt, 0)
	svc.reconcile()
	jobs, err = st.ListMaintenanceJobs("backup", "")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("after failed due=%+v err=%v", jobs, err)
	}
	if jobs[0].State != "queued" || jobs[0].Attempts != 0 {
		t.Fatalf("failed due transition=%+v", jobs[0])
	}
}

func TestRunKindsAndRetryValidation(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	svc := bareService(cfg, st, stampSvc)
	for _, kind := range []string{"backup", "retention", "ots_upgrade", "proof_gc", "session_cleanup"} {
		summary, err := svc.runKind(t.Context(), kind)
		if err != nil || summary == nil {
			t.Fatalf("%s summary=%+v err=%v", kind, summary, err)
		}
	}
	if _, err := svc.runKind(t.Context(), "bad"); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, _, err := svc.Enqueue("bad", "manual"); err == nil {
		t.Fatal("bad enqueue accepted")
	}
	if _, err := svc.Retry(999); err == nil {
		t.Fatal("missing retry accepted")
	}
	if svc.LeaseSeconds() != 90 {
		t.Fatalf("lease=%d", svc.LeaseSeconds())
	}
	if next := svc.initialNext("backup", time.Now()); next == 0 {
		t.Fatal("backup next run missing")
	}
	if svc.interval("backup") != 24*time.Hour {
		t.Fatalf("backup interval=%s", svc.interval("backup"))
	}
	if n, ok := intValue(1); !ok || n != 1 {
		t.Fatalf("int value=%d %t", n, ok)
	}
	if _, ok := intValue("bad"); ok {
		t.Fatal("string converted to int")
	}
}

func TestBackupScheduleAfterClockCorrection(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	svc := bareService(cfg, st, stampSvc)
	now := time.Now().Truncate(time.Second)
	future := now.Add(48 * time.Hour)
	svc.now = func() time.Time { return now }
	path := filepath.Join(cfg.Snapshot().BackupsDir(), "mytracker-future.mtbackup")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if next := svc.initialNext("backup", now); next != now.Unix() {
		t.Fatalf("future backup scheduled at %d, want %d", next, now.Unix())
	}
	if _, _, err := st.EnqueueMaintenance("backup", "manual", future.Unix()); err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimMaintenanceJob(future.Unix(), "future", future.Add(leaseDuration).Unix())
	if err != nil || job == nil {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	run := &store.MaintenanceRun{Kind: "backup", Trigger: "manual", Result: "success", StartedAt: future.Unix(), FinishedAt: future.Unix(), Summary: []byte("{}")}
	if err := st.FinishMaintenanceJob(job, run, false, future.Unix(), future.Add(24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	svc.reconcile()
	jobs, err := st.ListMaintenanceJobs("backup", "")
	if err != nil || len(jobs) != 1 || jobs[0].State != "queued" || jobs[0].NextRunAt != now.Unix() {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
}

func TestRunKindsHonorCanceledContext(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	svc := bareService(cfg, st, stampSvc)
	for _, kind := range []string{"backup", "retention", "ots_upgrade", "proof_gc", "session_cleanup"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := svc.runKind(ctx, kind); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s err=%v, want context.Canceled", kind, err)
		}
	}
}

func TestProofGCSummaryUsesReadyWork(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	svc := bareService(cfg, st, stampSvc)
	id, err := st.Insert(&store.Location{
		User: "u", Device: "gc", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: make([]byte, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cfg.Snapshot().ProofsDir(), fmt.Sprintf("%d.tsr", id)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteIDs([]int64{id}); err != nil {
		t.Fatal(err)
	}
	summary, err := svc.runKind(t.Context(), "proof_gc")
	if err != nil {
		t.Fatal(err)
	}
	if remaining, ok := intValue(summary["remaining"]); !ok || remaining != 0 {
		t.Fatalf("summary=%+v", summary)
	}
	if pending, ok := intValue(summary["pending"]); !ok || pending != 1 {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestFollowUpRunsAndSuccessfulRetry(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	svc := bareService(cfg, st, stampSvc)
	svc.run = func(_ context.Context, kind string) (map[string]any, error) {
		return map[string]any{"remaining": 1, "kind": kind}, nil
	}
	for _, kind := range []string{"ots_upgrade", "proof_gc"} {
		if _, _, err := svc.Enqueue(kind, "manual"); err != nil {
			t.Fatal(err)
		}
		job, err := st.ClaimMaintenanceJob(time.Now().Unix(), "owner-"+kind, time.Now().Add(time.Minute).Unix())
		if err != nil || job == nil {
			t.Fatalf("%s claim=%+v err=%v", kind, job, err)
		}
		svc.handleJob(job)
		jobs, _ := st.ListMaintenanceJobs(kind, "")
		if len(jobs) != 1 || jobs[0].State != "queued" || jobs[0].Generation != job.ClaimedGeneration+1 {
			t.Fatalf("%s jobs=%+v", kind, jobs)
		}
		if _, err := st.EnsureMaintenanceJob(kind, false, 0, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := svc.Enqueue("backup", "manual"); err != nil {
		t.Fatal(err)
	}
	job, _ := st.ClaimMaintenanceJob(time.Now().Unix(), "failed-owner", time.Now().Add(time.Minute).Unix())
	job.Attempts = job.MaxAttempts
	run := &store.MaintenanceRun{
		Result: "error", StartedAt: 1, FinishedAt: 2, Summary: []byte(`{}`), Error: "failed",
	}
	if err := st.FinishMaintenanceJob(job, run, true, time.Now().Unix(), time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	retried, err := svc.Retry(job.ID)
	if err != nil || retried.State != "queued" {
		t.Fatalf("retry=%+v err=%v", retried, err)
	}
}

func TestFailedBatchWithRemainingWorkKeepsRetryCooldown(t *testing.T) {
	for _, kind := range []string{"ots_upgrade", "proof_gc"} {
		t.Run(kind, func(t *testing.T) {
			cfg, st, stampSvc := maintenanceTestDeps(t)
			svc := bareService(cfg, st, stampSvc)
			now := time.Unix(1000, 0)
			svc.now = func() time.Time { return now }
			svc.run = func(context.Context, string) (map[string]any, error) {
				return map[string]any{"remaining": 1}, errors.New("batch failed")
			}
			if _, _, err := svc.Enqueue(kind, "manual"); err != nil {
				t.Fatal(err)
			}
			var job *store.MaintenanceJob
			for attempt := 1; attempt <= 5; attempt++ {
				var err error
				job, err = st.ClaimMaintenanceJob(now.Unix(), fmt.Sprintf("owner-%d", attempt), now.Add(leaseDuration).Unix())
				if err != nil || job == nil {
					t.Fatalf("claim %d=%+v err=%v", attempt, job, err)
				}
				svc.handleJob(job)
				jobs, err := st.ListMaintenanceJobs(kind, "")
				if err != nil || len(jobs) != 1 {
					t.Fatalf("after failure %d=%+v err=%v", attempt, jobs, err)
				}
				job = &jobs[0]
				if attempt < 5 {
					if job.State != "retry" {
						t.Fatalf("attempt %d state=%+v", attempt, job)
					}
					now = time.Unix(job.NextRunAt, 0)
				}
			}
			failed := *job
			if failed.State != "failed" || failed.Attempts != failed.MaxAttempts {
				t.Fatalf("terminal failure=%+v", failed)
			}
			svc.reconcile()
			jobs, err := st.ListMaintenanceJobs(kind, "")
			if err != nil || len(jobs) != 1 {
				t.Fatalf("after reconcile=%+v err=%v", jobs, err)
			}
			if got := jobs[0]; got.State != "failed" || got.Generation != failed.Generation || got.Attempts != failed.Attempts {
				t.Fatalf("reconcile reset terminal failure=%+v before=%+v", got, failed)
			}
		})
	}
}

func TestShutdownDeadline(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	svc := bareService(cfg, st, stampSvc)
	svc.wg.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := svc.ShutdownContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown=%v", err)
	}
	svc.wg.Done()
}

func TestLeaseLossAndInvalidBackupInterval(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	svc := bareService(cfg, st, stampSvc)
	if _, _, err := svc.Enqueue("backup", "manual"); err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimMaintenanceJob(time.Now().Unix(), "owner", time.Now().Add(time.Minute).Unix())
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	var replacement *store.MaintenanceJob
	svc.run = func(context.Context, string) (map[string]any, error) {
		replacement, err = st.ClaimMaintenanceJob(
			time.Now().Add(2*time.Minute).Unix(), "replacement", time.Now().Add(3*time.Minute).Unix())
		return map[string]any{}, err
	}
	svc.handleJob(job)
	if replacement == nil {
		t.Fatal("lease was not reclaimed")
	}
	if runs, _ := st.ListMaintenanceRuns(0, 10); len(runs) != 0 {
		t.Fatalf("lost lease stored run=%+v", runs)
	}

	bad := cfg.Snapshot()
	bad.BackupInterval = "bad"
	badMgr := config.NewManager(bad, nil)
	badSvc := bareService(badMgr, st, stampSvc)
	if got := badSvc.interval("backup"); got != 24*time.Hour {
		t.Fatalf("fallback interval=%s", got)
	}
}

func TestStartupRecoversRunningJob(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	if _, _, err := st.EnqueueMaintenance("backup", "manual", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if job, err := st.ClaimMaintenanceJob(
		time.Now().Unix(), "interrupted", time.Now().Add(time.Minute).Unix()); err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	svc := New(cfg, st, stampSvc)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := svc.ShutdownContext(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := st.MaintenanceStatus()
	if err != nil || status.Running != 0 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestRunReadyExpiresExhaustedLease(t *testing.T) {
	cfg, st, stampSvc := maintenanceTestDeps(t)
	svc := bareService(cfg, st, stampSvc)
	if _, _, err := svc.Enqueue("session_cleanup", "manual"); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Unix()
	for attempt := 1; attempt <= 5; attempt++ {
		job, err := st.ClaimMaintenanceJob(base+int64(attempt), "expired", base+int64(attempt))
		if err != nil || job == nil {
			t.Fatalf("claim %d=%+v err=%v", attempt, job, err)
		}
	}
	svc.now = func() time.Time { return time.Unix(base+10, 0) }
	if svc.runReady("replacement") {
		t.Fatal("expired job was executed")
	}
	status, _ := st.MaintenanceStatus()
	if status.Failed != 1 {
		t.Fatalf("status=%+v", status)
	}
}
