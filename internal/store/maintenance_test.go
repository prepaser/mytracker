package store_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/store"
)

func TestEmptyDashboardJobLists(t *testing.T) {
	st := openTestDB(t)
	stamps, err := st.ListStampJobsByState("failed", 20)
	if err != nil || stamps == nil || len(stamps) != 0 {
		t.Fatalf("stamp jobs=%v err=%v", stamps, err)
	}
	jobs, err := st.ListMaintenanceJobs("", "")
	if err != nil || jobs == nil || len(jobs) != 0 {
		t.Fatalf("maintenance jobs=%v err=%v", jobs, err)
	}
	runs, err := st.ListMaintenanceRuns(0, 10)
	if err != nil || runs == nil || len(runs) != 0 {
		t.Fatalf("maintenance runs=%v err=%v", runs, err)
	}
}

func TestMaintenanceGenerationAndAtomicRun(t *testing.T) {
	st := openTestDB(t)
	job, err := st.EnsureMaintenanceJob("backup", true, 100, 100)
	if err != nil || job.State != "queued" || job.Generation != 1 {
		t.Fatalf("initial=%+v err=%v", job, err)
	}
	claimed, err := st.ClaimMaintenanceJob(101, "owner-1", 191)
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	enqueued, coalesced, err := st.EnqueueMaintenance("backup", "manual", 102)
	if err != nil || !coalesced || enqueued.State != "running" || enqueued.Generation != 2 {
		t.Fatalf("enqueue=%+v coalesced=%t err=%v", enqueued, coalesced, err)
	}
	run := &store.MaintenanceRun{
		Result: "success", StartedAt: 101, FinishedAt: 103, DurationMS: 2000,
		Summary: json.RawMessage(`{"file":"safe.mtbackup"}`),
	}
	if err := st.FinishMaintenanceJob(claimed, run, false, 103, 1000); err != nil {
		t.Fatal(err)
	}
	jobs, _ := st.ListMaintenanceJobs("backup", "")
	runs, _ := st.ListMaintenanceRuns(0, 10)
	if len(jobs) != 1 || jobs[0].State != "queued" || jobs[0].Attempts != 0 || len(runs) != 1 || runs[0].Trigger != "scheduled" {
		t.Fatalf("jobs=%+v runs=%+v", jobs, runs)
	}
	followUp, err := st.ClaimMaintenanceJob(104, "owner-2", 194)
	if err != nil || followUp == nil || followUp.Trigger != "manual" {
		t.Fatalf("follow-up=%+v err=%v", followUp, err)
	}
	if err := st.FinishMaintenanceJob(followUp, run, false, 105, 1000); err != nil {
		t.Fatal(err)
	}
	if idle, coalesced, err := st.EnqueueMaintenance("backup", "manual", 106); err != nil || coalesced || idle.State != "queued" {
		t.Fatalf("idle enqueue=%+v coalesced=%t err=%v", idle, coalesced, err)
	}
}

func TestScheduledMaintenanceAfterManualRun(t *testing.T) {
	st := openTestDB(t)
	if _, _, err := st.EnqueueMaintenance("backup", "manual", 100); err != nil {
		t.Fatal(err)
	}
	manual, err := st.ClaimMaintenanceJob(101, "owner", 191)
	if err != nil || manual == nil {
		t.Fatalf("manual=%+v err=%v", manual, err)
	}
	run := &store.MaintenanceRun{Result: "success", StartedAt: 101, FinishedAt: 102, Summary: json.RawMessage(`{}`)}
	if err := st.FinishMaintenanceJob(manual, run, false, 102, 200); err != nil {
		t.Fatal(err)
	}
	if run.Trigger != "manual" {
		t.Fatalf("manual history trigger=%q", run.Trigger)
	}
	if _, err := st.EnsureMaintenanceJob("backup", true, 200, 200); err != nil {
		t.Fatal(err)
	}
	scheduled, err := st.ClaimMaintenanceJob(201, "next-owner", 291)
	if err != nil || scheduled == nil || scheduled.Trigger != "scheduled" {
		t.Fatalf("scheduled=%+v err=%v", scheduled, err)
	}
	run = &store.MaintenanceRun{Result: "success", StartedAt: 201, FinishedAt: 202, Summary: json.RawMessage(`{}`)}
	if err := st.FinishMaintenanceJob(scheduled, run, false, 202, 300); err != nil {
		t.Fatal(err)
	}
	if run.Trigger != "scheduled" {
		t.Fatalf("scheduled history trigger=%q", run.Trigger)
	}
}

func TestMaintenanceClaimResetsAttemptsForNewGeneration(t *testing.T) {
	st := openTestDB(t)
	if _, _, err := st.EnqueueMaintenance("backup", "manual", 100); err != nil {
		t.Fatal(err)
	}

	for attempt, now := range []int64{101, 111, 121, 131, 141} {
		job, err := st.ClaimMaintenanceJob(now, "owner", now+1)
		if err != nil || job == nil || job.Attempts != attempt+1 {
			t.Fatalf("claim %d=%+v err=%v", attempt+1, job, err)
		}
		if attempt == 4 {
			break
		}
		run := &store.MaintenanceRun{Result: "error", StartedAt: now, FinishedAt: now + 1,
			Summary: json.RawMessage(`{}`), Error: "retry"}
		if err := st.FinishMaintenanceJob(job, run, true, now, 0); err != nil {
			t.Fatal(err)
		}
	}
	if job, _, err := st.EnqueueMaintenance("backup", "manual", 150); err != nil || job.Generation != 2 {
		t.Fatalf("new generation=%+v err=%v", job, err)
	}
	job, err := st.ClaimMaintenanceJob(160, "replacement", 161)
	if err != nil || job == nil || job.Generation != 2 || job.Attempts != 1 {
		t.Fatalf("new generation claim=%+v err=%v", job, err)
	}
}

func TestListMaintenanceRunsAllowsLookahead(t *testing.T) {
	st := openTestDB(t)
	const count = 201
	for i := range int64(count) {
		now := int64(100000) + i*2
		if _, _, err := st.EnqueueMaintenance("backup", "manual", now); err != nil {
			t.Fatal(err)
		}
		job, err := st.ClaimMaintenanceJob(now, "owner", now+90)
		if err != nil || job == nil {
			t.Fatalf("claim=%+v err=%v", job, err)
		}
		run := &store.MaintenanceRun{Result: "success", StartedAt: now, FinishedAt: now + 1, Summary: json.RawMessage(`{}`)}
		if err := st.FinishMaintenanceJob(job, run, false, now+1, now+100); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := st.ListMaintenanceRuns(0, count)
	if err != nil || len(runs) != count {
		t.Fatalf("runs=%d err=%v", len(runs), err)
	}
}

func TestMaintenanceLeaseLossDoesNotSaveRun(t *testing.T) {
	st := openTestDB(t)
	if _, err := st.EnsureMaintenanceJob("proof_gc", true, 100, 100); err != nil {
		t.Fatal(err)
	}
	first, _ := st.ClaimMaintenanceJob(101, "owner-1", 102)
	if ok, err := st.RenewMaintenanceJob(first.ID, "owner-1", 150, 101); err != nil || !ok {
		t.Fatalf("renew=%t err=%v", ok, err)
	}
	if ok, err := st.RenewMaintenanceJob(first.ID, "wrong", 150, 101); err != nil || ok {
		t.Fatalf("wrong renew=%t err=%v", ok, err)
	}
	if n, _ := st.MaintenanceExpiredLeaseCount(151); n != 1 {
		t.Fatalf("expired=%d", n)
	}
	if ok, err := st.RenewMaintenanceJob(first.ID, "owner-1", 200, 151); err != nil || ok {
		t.Fatalf("expired renew=%t err=%v", ok, err)
	}
	second, err := st.ClaimMaintenanceJob(151, "owner-2", 241)
	if err != nil || second == nil {
		t.Fatalf("reclaim=%+v err=%v", second, err)
	}
	run := &store.MaintenanceRun{Result: "success", StartedAt: 101, FinishedAt: 152, Summary: json.RawMessage(`{}`)}
	if err := st.FinishMaintenanceJob(first, run, false, 152, 200); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("lost lease=%v", err)
	}
	if runs, _ := st.ListMaintenanceRuns(0, 10); len(runs) != 0 {
		t.Fatalf("lost owner saved run: %+v", runs)
	}
	if err := st.FinishMaintenanceJob(second, run, false, 153, 200); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredMaintenanceLeaseCannotSaveRun(t *testing.T) {
	st := openTestDB(t)
	if _, err := st.EnsureMaintenanceJob("backup", true, 100, 100); err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimMaintenanceJob(101, "owner", 102)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	run := &store.MaintenanceRun{Result: "success", StartedAt: 101, FinishedAt: 103, Summary: json.RawMessage(`{}`)}
	if err := st.FinishMaintenanceJob(job, run, false, 103, 200); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("expired lease err=%v", err)
	}
	if runs, _ := st.ListMaintenanceRuns(0, 10); len(runs) != 0 {
		t.Fatalf("expired owner saved run: %+v", runs)
	}
}

func TestMaintenanceRetryRecoveryFilteringAndRedaction(t *testing.T) {
	st := openTestDB(t)
	if _, _, err := st.EnqueueMaintenance("ots_upgrade", "manual", 100); err != nil {
		t.Fatal(err)
	}
	job, _ := st.ClaimMaintenanceJob(101, "owner", 191)
	run := &store.MaintenanceRun{
		Result: "error", StartedAt: 101, FinishedAt: 102, Summary: json.RawMessage(`{}`),
		Error: "https://user:pass@example.test/path?token=secret",
	}
	if err := st.FinishMaintenanceJob(job, run, true, 102, 1000); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListMaintenanceJobs("ots_upgrade", "retry")
	if err != nil || len(jobs) != 1 || strings.Contains(jobs[0].LastError, "secret") {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	runs, _ := st.ListMaintenanceRuns(0, 10)
	if len(runs) != 1 || strings.Contains(runs[0].Error, "secret") {
		t.Fatalf("runs=%+v", runs)
	}
	job, err = st.ClaimMaintenanceJob(200, "dead", 300)
	if err != nil || job == nil || job.Attempts != 2 || job.Trigger != "manual" {
		t.Fatalf("retry claim=%+v err=%v", job, err)
	}
	if n, err := st.RecoverMaintenanceJobs(201); err != nil || n != 1 {
		t.Fatalf("recover=%d err=%v", n, err)
	}
	if _, err := st.ListMaintenanceJobs("bad", ""); err == nil {
		t.Fatal("invalid kind accepted")
	}
	if _, err := st.ListMaintenanceJobs("", "bad"); err == nil {
		t.Fatal("invalid state accepted")
	}
}

func TestMaintenanceDisabledAndRetry(t *testing.T) {
	st := openTestDB(t)
	job, err := st.EnsureMaintenanceJob("retention", false, 0, 100)
	if err != nil || job.State != "disabled" {
		t.Fatalf("disabled=%+v err=%v", job, err)
	}
	job, err = st.EnsureMaintenanceJob("retention", true, 100, 100)
	if err != nil || job.State != "queued" || job.Generation != 1 {
		t.Fatalf("enabled=%+v err=%v", job, err)
	}
	claimed, _ := st.ClaimMaintenanceJob(101, "owner", 191)
	claimed.Attempts = claimed.MaxAttempts
	run := &store.MaintenanceRun{
		Result: "error", StartedAt: 101, FinishedAt: 102, Summary: json.RawMessage(`{}`), Error: "disk",
	}
	if err := st.FinishMaintenanceJob(claimed, run, true, 102, 1000); err != nil {
		t.Fatal(err)
	}
	jobs, _ := st.ListMaintenanceJobs("", "failed")
	if len(jobs) != 1 {
		t.Fatalf("failed=%+v", jobs)
	}
	retried, err := st.RetryMaintenanceJob(jobs[0].ID, 103)
	if err != nil || retried.State != "queued" {
		t.Fatalf("retry=%+v err=%v", retried, err)
	}
	if _, err := st.RetryMaintenanceJob(retried.ID, 104); err == nil {
		t.Fatal("active job retried")
	}
}

func TestEnsureMaintenanceJobNoOpDoesNotWrite(t *testing.T) {
	st := openTestDB(t)
	job, err := st.EnsureMaintenanceJob("backup", true, 200, 100)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := st.ListMaintenanceJobs("backup", "")
	if err != nil || len(initial) != 1 {
		t.Fatalf("initial=%+v err=%v", initial, err)
	}
	if _, err := st.EnsureMaintenanceJob("backup", true, 200, 101); err != nil {
		t.Fatal(err)
	}
	current, err := st.ListMaintenanceJobs("backup", "")
	if err != nil || len(current) != 1 {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	if current[0].ID != job.ID || current[0].UpdatedAt != initial[0].UpdatedAt {
		t.Fatalf("no-op changed job: initial=%+v current=%+v", initial[0], current[0])
	}
	manual, _, err := st.EnqueueMaintenance("backup", "manual", 102)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureMaintenanceJob("backup", true, 9999, 103); err != nil {
		t.Fatal(err)
	}
	queued, err := st.ListMaintenanceJobs("backup", "")
	if err != nil || len(queued) != 1 || queued[0].State != "queued" || queued[0].NextRunAt != manual.NextRunAt {
		t.Fatalf("manual enqueue was overwritten: jobs=%+v err=%v", queued, err)
	}

	if _, err := st.EnsureMaintenanceJob("backup", true, 100, 101); err != nil {
		t.Fatal(err)
	}
	queued, err = st.ListMaintenanceJobs("backup", "")
	if err != nil || len(queued) != 1 || queued[0].State != "queued" || queued[0].Generation != 1 {
		t.Fatalf("due transition=%+v err=%v", queued, err)
	}
}

func TestMaintenanceStatusExpiryOverdueAndPagination(t *testing.T) {
	st := openTestDB(t)
	if _, err := st.EnsureMaintenanceJob("backup", true, 1000, 100); err != nil {
		t.Fatal(err)
	}
	if overdue, err := st.MaintenanceOverdueCount(1001); err != nil || overdue != 1 {
		t.Fatalf("overdue=%d err=%v", overdue, err)
	}
	if status, err := st.MaintenanceStatus(); err != nil || status.Scheduled != 1 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if _, _, err := st.EnqueueMaintenance("session_cleanup", "manual", 100); err != nil {
		t.Fatal(err)
	}
	var job *store.MaintenanceJob
	for attempt := 1; attempt <= 5; attempt++ {
		var err error
		job, err = st.ClaimMaintenanceJob(int64(100+attempt), "owner", int64(100+attempt))
		if err != nil || job == nil {
			t.Fatalf("claim %d=%+v err=%v", attempt, job, err)
		}
	}
	if expired, err := st.ExpireMaintenanceLeases(200); err != nil || expired != 1 {
		t.Fatalf("expired=%d err=%v", expired, err)
	}
	if status, _ := st.MaintenanceStatus(); status.Failed != 1 {
		t.Fatalf("status=%+v", status)
	}
	run := &store.MaintenanceRun{
		Result: "success", StartedAt: 1, FinishedAt: 2, Summary: json.RawMessage(`{}`),
	}
	if _, err := st.EnsureMaintenanceJob("backup", true, 1000, 1001); err != nil {
		t.Fatal(err)
	}
	backup, err := st.ClaimMaintenanceJob(1001, "backup", 1091)
	if err != nil || backup == nil {
		t.Fatalf("backup=%+v err=%v", backup, err)
	}
	if err := st.FinishMaintenanceJob(backup, run, false, 1002, 2000); err != nil {
		t.Fatal(err)
	}
	runs, err := st.ListMaintenanceRuns(0, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	if older, err := st.ListMaintenanceRuns(runs[0].ID, 1); err != nil || len(older) != 0 {
		t.Fatalf("older=%+v err=%v", older, err)
	}
}

func TestExpiredMaintenanceLeaseQueuesNewGeneration(t *testing.T) {
	st := openTestDB(t)
	if _, err := st.EnsureMaintenanceJob("backup", true, 100, 100); err != nil {
		t.Fatal(err)
	}
	claims := []int64{101, 103, 106, 111, 120}
	for attempt, now := range claims {
		job, err := st.ClaimMaintenanceJob(now, "owner", now+90)
		if err != nil || job == nil {
			t.Fatalf("claim %d=%+v err=%v", attempt+1, job, err)
		}
		if attempt == len(claims)-1 {
			continue
		}
		run := &store.MaintenanceRun{
			Result: "error", StartedAt: now, FinishedAt: now + 1,
			Summary: json.RawMessage(`{}`), Error: "retry",
		}
		if err := st.FinishMaintenanceJob(job, run, true, now+1, 0); err != nil {
			t.Fatal(err)
		}
	}
	job, _, err := st.EnqueueMaintenance("backup", "manual", 121)
	if err != nil || job.State != "running" || job.Generation != 2 {
		t.Fatalf("enqueue=%+v err=%v", job, err)
	}
	if expired, err := st.ExpireMaintenanceLeases(300); err != nil || expired != 1 {
		t.Fatalf("expired=%d err=%v", expired, err)
	}
	jobs, err := st.ListMaintenanceJobs("backup", "")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	job = &jobs[0]
	if job.State != "queued" || job.Generation != 2 || job.ClaimedGeneration != 1 ||
		job.Attempts != 0 || job.NextRunAt != 0 || job.LastError != "" {
		t.Fatalf("new generation was not queued: %+v", job)
	}
	claimed, err := st.ClaimMaintenanceJob(300, "replacement", 390)
	if err != nil || claimed == nil || claimed.Generation != 2 {
		t.Fatalf("replacement=%+v err=%v", claimed, err)
	}
}

func TestPendingOTSBatchAndCount(t *testing.T) {
	st := openTestDB(t)
	for i := range 3 {
		loc := sampleLocation()
		loc.Tst += int64(i)
		if _, err := st.Insert(loc); err != nil {
			t.Fatal(err)
		}
	}
	locs, err := st.PendingOTSLimit(2)
	if err != nil || len(locs) != 2 {
		t.Fatalf("locs=%d err=%v", len(locs), err)
	}
	if count, err := st.CountPendingOTS(); err != nil || count != 3 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestOTSUpgradeCandidatesAdvanceByID(t *testing.T) {
	st := openTestDB(t)
	var ids []int64
	for i := range 3 {
		loc := sampleLocation()
		loc.Tst += int64(i)
		loc.TsaStatus = "ok"
		loc.OtsStatus = "submitted"
		id, err := st.ImportLocation(loc)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	first, err := st.OTSUpgradeCandidates(0, 2)
	if err != nil || len(first) != 2 || first[1].ID != ids[1] {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := st.OTSUpgradeCandidates(first[1].ID, 2)
	if err != nil || len(second) != 1 || second[0].ID != ids[2] {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if n, err := st.CountSubmittedOTS(); err != nil || n != 3 {
		t.Fatalf("submitted=%d err=%v", n, err)
	}
}
