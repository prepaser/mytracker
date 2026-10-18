package store_test

import (
	"errors"
	"testing"

	"github.com/prepaser/mytracker/internal/store"
)

func TestVerificationJobGenerationAndAtomicCompletion(t *testing.T) {
	st := openTestDB(t)
	loc := sampleLocation()
	id, err := st.Insert(loc)
	if err != nil {
		t.Fatal(err)
	}
	job, coalesced, err := st.EnqueueVerification(id, "tsa", "scheduled", 100)
	if err != nil || coalesced || job.Generation != 1 {
		t.Fatalf("first job=%+v coalesced=%v err=%v", job, coalesced, err)
	}
	job, coalesced, err = st.EnqueueVerification(id, "tsa", "manual", 101)
	if err != nil || !coalesced || job.Generation != 2 || job.ID == 0 {
		t.Fatalf("coalesced job=%+v coalesced=%v err=%v", job, coalesced, err)
	}
	claimed, err := st.ClaimVerificationJob(102, "owner-1", 192)
	if err != nil || claimed == nil || claimed.ClaimedGeneration != 2 {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	if _, _, err := st.EnqueueVerification(id, "tsa", "stamp", 103); err != nil {
		t.Fatal(err)
	}
	run := &store.VerificationRun{
		LocationID: id, Kind: "tsa", Trigger: "manual", Result: "missing", CheckedAt: 103,
	}
	if err := st.FinishVerificationJob(claimed, run, false, nil, 104); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListLocationVerificationJobs(id)
	if err != nil || len(jobs) != 1 || jobs[0].State != "queued" || jobs[0].Attempts != 0 {
		t.Fatalf("follow-up=%+v err=%v", jobs, err)
	}
	claimed, err = st.ClaimVerificationJob(105, "owner-2", 195)
	if err != nil || claimed == nil || claimed.ClaimedGeneration != 3 {
		t.Fatalf("second claim=%+v err=%v", claimed, err)
	}
	run = &store.VerificationRun{
		LocationID: id, Kind: "tsa", Trigger: "stamp", Result: "missing", CheckedAt: 105,
	}
	if err := st.FinishVerificationJob(claimed, run, false, nil, 106); err != nil {
		t.Fatal(err)
	}
	jobs, _ = st.ListLocationVerificationJobs(id)
	history, _ := st.VerificationHistory(id, 0, 10)
	if jobs[0].State != "done" || len(history) != 2 {
		t.Fatalf("jobs=%+v history=%+v", jobs, history)
	}
	if idle, coalesced, err := st.EnqueueVerification(id, "tsa", "manual", 107); err != nil || coalesced || idle.State != "queued" {
		t.Fatalf("idle enqueue=%+v coalesced=%t err=%v", idle, coalesced, err)
	}
}

func TestVerificationClaimResetsAttemptsForNewGeneration(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnqueueVerification(id, "tsa", "manual", 100); err != nil {
		t.Fatal(err)
	}

	for attempt, now := range []int64{101, 111, 121, 131, 141} {
		job, err := st.ClaimVerificationJob(now, "owner", now+1)
		if err != nil || job == nil || job.Attempts != attempt+1 {
			t.Fatalf("claim %d=%+v err=%v", attempt+1, job, err)
		}
		if attempt == 4 {
			break
		}
		if err := st.FinishVerificationJob(job, nil, true, errors.New("retry"), now); err != nil {
			t.Fatal(err)
		}
	}
	if job, _, err := st.EnqueueVerification(id, "tsa", "manual", 150); err != nil || job.Generation != 2 {
		t.Fatalf("new generation=%+v err=%v", job, err)
	}
	job, err := st.ClaimVerificationJob(160, "replacement", 161)
	if err != nil || job == nil || job.Generation != 2 || job.Attempts != 1 {
		t.Fatalf("new generation claim=%+v err=%v", job, err)
	}
}

func TestEnqueueVerificationPairIsAtomic(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	jobs, coalesced, err := st.EnqueueVerificationPair(id, "manual", 100)
	if err != nil || len(jobs) != 2 || len(coalesced) != 2 {
		t.Fatalf("jobs=%+v coalesced=%v err=%v", jobs, coalesced, err)
	}
	if _, _, err := st.EnqueueVerificationPair(id+1000, "manual", 101); err == nil {
		t.Fatal("missing location was accepted")
	}
	missing, err := st.ListLocationVerificationJobs(id + 1000)
	if err != nil || len(missing) != 0 {
		t.Fatalf("partial jobs=%+v err=%v", missing, err)
	}
}

func TestIterateVerificationReconcile(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveVerification(&store.VerificationRun{
		LocationID: id, Kind: "tsa", Trigger: "manual", Result: "verified",
		CheckedAt: 100, ProofSHA256: []byte{1}, PolicySHA256: []byte{2}, EngineVersion: 2,
	}); err != nil {
		t.Fatal(err)
	}
	var items []store.VerificationReconcileItem
	if err := st.IterateVerificationReconcile(1, func(batch []store.VerificationReconcileItem) error {
		items = append(items, batch...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Kind != "tsa" || items[0].RunID == 0 ||
		items[0].Engine != 2 || items[1].Kind != "ots" || items[1].RunID != 0 {
		t.Fatalf("items=%+v", items)
	}
	items = nil
	if err := st.IterateVerificationReconcileAfter(id, "tsa", 1, func(batch []store.VerificationReconcileItem) error {
		items = append(items, batch...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].LocationID != id || items[0].Kind != "ots" {
		t.Fatalf("items after cursor=%+v", items)
	}
}

func TestVerificationLeaseLossDoesNotSaveRun(t *testing.T) {
	st := openTestDB(t)
	loc := sampleLocation()
	id, err := st.Insert(loc)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnqueueVerification(id, "ots", "manual", 100); err != nil {
		t.Fatal(err)
	}
	first, err := st.ClaimVerificationJob(101, "owner-1", 102)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.RenewVerificationJob(first.ID, "owner-1", 150, 101); err != nil || !ok {
		t.Fatalf("renew=%v err=%v", ok, err)
	}
	if ok, err := st.RenewVerificationJob(first.ID, "wrong", 150, 101); err != nil || ok {
		t.Fatalf("wrong renew=%v err=%v", ok, err)
	}
	if n, err := st.ActiveVerificationJobCount(); err != nil || n != 1 {
		t.Fatalf("active=%d err=%v", n, err)
	}
	if n, err := st.VerificationExpiredLeaseCount(151); err != nil || n != 1 {
		t.Fatalf("expired=%d err=%v", n, err)
	}
	if ok, err := st.RenewVerificationJob(first.ID, "owner-1", 200, 151); err != nil || ok {
		t.Fatalf("expired renew=%v err=%v", ok, err)
	}
	second, err := st.ClaimVerificationJob(151, "owner-2", 200)
	if err != nil || second == nil {
		t.Fatalf("reclaim=%+v err=%v", second, err)
	}
	run := &store.VerificationRun{
		LocationID: id, Kind: "ots", Trigger: "manual", Result: "missing", CheckedAt: 103,
	}
	if err := st.FinishVerificationJob(first, run, false, nil, 104); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("lost lease err=%v", err)
	}
	if history, _ := st.VerificationHistory(id, 0, 10); len(history) != 0 {
		t.Fatalf("lost owner saved run: %+v", history)
	}
	if err := st.FinishVerificationJob(second, run, false, nil, 105); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredVerificationLeaseCannotSaveRun(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnqueueVerification(id, "tsa", "manual", 100); err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimVerificationJob(101, "owner", 102)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	run := &store.VerificationRun{LocationID: id, Kind: "tsa", Trigger: "manual", Result: "missing", CheckedAt: 103}
	if err := st.FinishVerificationJob(job, run, false, nil, 103); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("expired lease err=%v", err)
	}
	if history, _ := st.VerificationHistory(id, 0, 10); len(history) != 0 {
		t.Fatalf("expired owner saved run: %+v", history)
	}
}

func TestVerificationRetryRecoveryAndFiltering(t *testing.T) {
	st := openTestDB(t)
	loc := sampleLocation()
	id, err := st.Insert(loc)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnqueueVerification(id, "ots", "import", 100); err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimVerificationJob(101, "owner", 191)
	if err != nil {
		t.Fatal(err)
	}
	run := &store.VerificationRun{
		LocationID: id, Kind: "ots", Trigger: "import", Result: "error",
		ErrorCode: "provider_error", CheckedAt: 101,
	}
	if err := st.FinishVerificationJob(job, run, true, errors.New("https://user:pass@example.test/?token=x"), 102); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListVerificationJobs("retry", 10)
	if err != nil || len(jobs) != 1 || jobs[0].LastError == "" ||
		jobs[0].LastError == "https://user:pass@example.test/?token=x" {
		t.Fatalf("retry jobs=%+v err=%v", jobs, err)
	}
	if _, err := st.ListVerificationJobs("bogus", 10); err == nil {
		t.Fatal("invalid state accepted")
	}
	job, err = st.ClaimVerificationJob(200, "dead", 300)
	if err != nil || job == nil || job.Attempts != 2 {
		t.Fatalf("retry claim=%+v err=%v", job, err)
	}
	if n, err := st.RecoverVerificationJobs(201); err != nil || n != 1 {
		t.Fatalf("recover=%d err=%v", n, err)
	}
	status, err := st.VerificationJobStatus()
	if err != nil || status.Queued != 1 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	now := int64(1000)
	for {
		job, err = st.ClaimVerificationJob(now, "expiry", now+1)
		if err != nil || job == nil {
			t.Fatalf("expiry claim=%+v err=%v", job, err)
		}
		if job.Attempts >= job.MaxAttempts {
			break
		}
		if err := st.FinishVerificationJob(job, nil, true, errors.New("network"), now); err != nil {
			t.Fatal(err)
		}
		now += 1000
	}
	if n, err := st.ExpireVerificationJobLeases(now + 2); err != nil || n != 1 {
		t.Fatalf("expired final=%d err=%v", n, err)
	}
	jobs, _ = st.ListVerificationJobs("failed", 10)
	if len(jobs) != 1 || jobs[0].State != "failed" {
		t.Fatalf("failed jobs=%+v", jobs)
	}
}
