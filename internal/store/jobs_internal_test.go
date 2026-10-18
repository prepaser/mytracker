package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

type stampPolicySink func(Change)

func (f stampPolicySink) Publish(change Change) { f(change) }

func TestStampPolicyIsCommittedBeforePublication(t *testing.T) {
	for _, imported := range []bool{false, true} {
		for _, attempts := range []int{1, 7} {
			t.Run(fmt.Sprintf("import=%t/attempts=%d", imported, attempts), func(t *testing.T) {
				st, err := Open(filepath.Join(t.TempDir(), "policy.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = st.Close() }()
				published := false
				st.SetChangeSink(stampPolicySink(func(change Change) {
					if change.Topic != ChangeLocations {
						return
					}
					published = true
					jobs, err := st.ListStampJobs(10)
					if err != nil || len(jobs) != 2 {
						t.Fatalf("published jobs=%+v err=%v", jobs, err)
					}
					for _, job := range jobs {
						if job.MaxAttempts != attempts || job.State != "queued" {
							t.Fatalf("published job=%+v", job)
						}
					}
				}))
				loc := &Location{User: "u", Device: "d", Tst: 1, Lat: 1, Lon: 2, Payload: []byte(`{}`), Digest: []byte{1}, TsaStatus: "pending", OtsStatus: "pending"}
				if imported {
					err = st.ImportAtomic(func(tx *ImportTx) error {
						_, err := tx.InsertLocation(loc, attempts)
						return err
					})
				} else {
					_, err = st.Insert(loc, attempts)
				}
				if err != nil || !published {
					t.Fatalf("published=%t err=%v", published, err)
				}
			})
		}
	}
}

func TestFailStampJobUsesCurrentAttemptLimit(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "attempts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Lat: 1, Lon: 2, Payload: []byte(`{}`), Digest: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE stamp_jobs SET state='done' WHERE location_id=? AND kind='ots'`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobMaxAttempts(id, 5); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Unix()
	for i, now := range []int64{base, base + 10} {
		job, err := st.ClaimStampJob(now, "worker", base+1000)
		if err != nil || job == nil {
			t.Fatalf("claim %d: job=%+v err=%v", i+1, job, err)
		}
		if retry, err := st.FailStampJob(job, errors.New("temporary")); err != nil || !retry {
			t.Fatalf("fail %d: retry=%t err=%v", i+1, retry, err)
		}
	}
	job, err := st.ClaimStampJob(base+20, "worker", base+1000)
	if err != nil || job == nil || job.Attempts != 3 {
		t.Fatalf("third claim: job=%+v err=%v", job, err)
	}
	if err := st.SetJobMaxAttempts(id, 1); err != nil {
		t.Fatal(err)
	}
	if retry, err := st.FailStampJob(job, context.Canceled); err != nil || retry {
		t.Fatalf("lowered limit: retry=%t err=%v", retry, err)
	}
	jobs, err := st.ListLocationStampJobs(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].State != "failed" && jobs[1].State != "failed" {
		t.Fatalf("jobs=%+v", jobs)
	}
	loc, err := st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if loc.TsaStatus != "fail" {
		t.Fatalf("tsa status=%q", loc.TsaStatus)
	}
}

func TestFailStampJobUsesRaisedAttemptLimit(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "raised.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Lat: 1, Lon: 2, Payload: []byte(`{}`), Digest: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE stamp_jobs SET state='done' WHERE location_id=? AND kind='ots'`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobMaxAttempts(id, 1); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Unix()
	job, err := st.ClaimStampJob(base, "worker", base+1000)
	if err != nil || job == nil {
		t.Fatalf("claim: job=%+v err=%v", job, err)
	}
	if err := st.SetJobMaxAttempts(id, 2); err != nil {
		t.Fatal(err)
	}
	if retry, err := st.FailStampJob(job, errors.New("canceled")); err != nil || !retry {
		t.Fatalf("raised limit: retry=%t err=%v", retry, err)
	}
}

func TestClaimDigestFailureDoesNotStrandJob(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.Insert(&Location{
		User: "u", Device: "d", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: []byte{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobMaxAttempts(id, 1); err != nil {
		t.Fatal(err)
	}
	st.db.SetMaxOpenConns(1)
	if _, err := st.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DELETE FROM locations WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if job, err := st.ClaimStampJob(now, "worker", now+90); err == nil || job != nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	var state string
	if err := st.db.QueryRow(`SELECT state FROM stamp_jobs ORDER BY id LIMIT 1`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("state=%q", state)
	}
}

func TestRetryStampJobsDoesNotResetRunningLease(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "running.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.Insert(&Location{
		User: "u", Device: "d", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: []byte{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	job, err := st.ClaimStampJob(now, "owner", now+90)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if err := st.UpdateTSA(id, "fail", nil); err != nil {
		t.Fatal(err)
	}
	if n, err := st.RetryStampJobs(id, 3); err != nil || n != 1 {
		t.Fatalf("retried=%d err=%v", n, err)
	}
	loc, err := st.Get(id)
	if err != nil || loc.TsaStatus != "fail" {
		t.Fatalf("location=%+v err=%v", loc, err)
	}
	jobs, err := st.ListLocationStampJobs(id)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	for _, got := range jobs {
		if got.State == "running" && (got.LeaseOwner != "owner" || got.LeaseUntil != now+90) {
			t.Fatalf("running job mutated: %+v", got)
		}
	}
}

func TestTimelineClaimRoundRobinKeepsWallClockTimestamps(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "timeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	devices := []string{"hot", "cold", "warm"}
	for i, device := range devices {
		if _, err := st.Insert(&Location{User: "u", Device: device, Tst: int64(i + 1), Payload: []byte(`{}`), Digest: []byte{byte(i + 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Unix()
	for cycle := range 12 {
		job, err := st.ClaimTimelineJob("worker", now, now+90)
		if err != nil || job == nil {
			t.Fatalf("cycle=%d job=%+v err=%v", cycle, job, err)
		}
		if want := devices[cycle%len(devices)]; job.Device != want {
			t.Fatalf("cycle=%d device=%q want=%q", cycle, job.Device, want)
		}
		if err := st.EnqueueTimeline(job.User, job.Device, job.DirtyFrom); err != nil {
			t.Fatal(err)
		}
		if err := st.FinishTimelineJob(job, job.DirtyFrom, nil, nil); err != nil {
			t.Fatal(err)
		}
		var latest int64
		if err := st.db.QueryRow(`SELECT COALESCE(MAX(updated_at),0) FROM timeline_rebuild_jobs`).Scan(&latest); err != nil {
			t.Fatal(err)
		}
		if latest > time.Now().Unix() {
			t.Fatalf("future updated_at=%d now=%d", latest, time.Now().Unix())
		}
	}
}

func TestJobStatusExcludesFailedFromOldest(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "oldest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.Insert(&Location{
		User: "u", Device: "d", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: []byte{1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE stamp_jobs SET state='failed',updated_at=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE stamp_jobs SET state='queued',updated_at=200 WHERE kind='tsa'`); err != nil {
		t.Fatal(err)
	}
	status, err := st.JobStatusContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Queued != 1 || status.Failed != 1 || status.OldestQueued != 200 {
		t.Fatalf("status=%+v", status)
	}
}
