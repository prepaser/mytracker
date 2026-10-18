package store_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/store"
)

func openTestDB(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func sampleLocation() *store.Location {
	return &store.Location{
		User: "alice", Device: "phone", Tst: 1751800000,
		Lat: 37.5665, Lon: 126.978,
		Payload: json.RawMessage(`{"_type":"location"}`),
		Digest:  []byte{1, 2, 3, 4},
	}
}

func TestOpenLiteralPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data?query#fragment% +space", "mytracker.db")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Insert(sampleLocation())
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	st, err = store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	loc, err := st.Get(id)
	if err != nil || loc.User != "alice" {
		t.Fatalf("literal path location=%+v err=%v", loc, err)
	}
}

func TestInsertAndGet(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	loc, err := st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if loc.User != "alice" || loc.TsaStatus != "pending" {
		t.Fatalf("unexpected: %+v", loc)
	}
}

func TestInsertDuplicate(t *testing.T) {
	st := openTestDB(t)
	loc := sampleLocation()
	if _, err := st.Insert(loc); err != nil {
		t.Fatal(err)
	}
	_, err := st.Insert(loc)
	if !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateTSAAndOTS(t *testing.T) {
	st := openTestDB(t)
	id, _ := st.Insert(sampleLocation())
	ts := int64(1751800100)
	if err := st.UpdateTSA(id, "ok", &ts); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTSA(id, "pending", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTSA(id, "ok", &ts); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateOTS(id, "confirmed"); err != nil {
		t.Fatal(err)
	}
	loc, _ := st.Get(id)
	if loc.TsaStatus != "ok" || !loc.TsaTime.Valid || loc.OtsStatus != "confirmed" {
		t.Fatalf("unexpected: %+v", loc)
	}
}

func TestUpdateOTSReturnsNoRowsForDeletedLocation(t *testing.T) {
	st := openTestDB(t)
	if err := st.UpdateOTS(404, "confirmed"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("error=%v", err)
	}
}

func TestListAndStats(t *testing.T) {
	st := openTestDB(t)
	id, _ := st.Insert(sampleLocation())
	ts := int64(1)
	_ = st.UpdateTSA(id, "ok", &ts)
	_ = st.UpdateOTS(id, "submitted")

	locs, err := st.List(10, 0)
	if err != nil || len(locs) != 1 {
		t.Fatalf("list: %d err=%v", len(locs), err)
	}
	pending, err := st.PendingOTS()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %d", len(pending))
	}
	st2, err := st.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st2.Total != 1 || st2.TsaOK != 1 || st2.OtsPending != 1 {
		t.Fatalf("stats: %+v", st2)
	}
}

func TestListExportFilter(t *testing.T) {
	st := openTestDB(t)
	for _, tst := range []int64{100, 200, 300} {
		loc := sampleLocation()
		loc.Tst = tst
		if _, err := st.Insert(loc); err != nil {
			t.Fatal(err)
		}
	}
	locs, err := st.ListExport(store.ExportFilter{From: 150, To: 250})
	if err != nil {
		t.Fatal(err)
	}
	if len(locs) != 1 || locs[0].Tst != 200 {
		t.Fatalf("got %+v", locs)
	}
}

func TestLegacyDatabaseMigratesAndQueuesPendingStamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE locations (
			id INTEGER PRIMARY KEY,user TEXT NOT NULL,device TEXT NOT NULL,tst INTEGER NOT NULL,
			lat REAL NOT NULL,lon REAL NOT NULL,payload BLOB NOT NULL,digest BLOB NOT NULL,
			tsa_time INTEGER,tsa_status TEXT NOT NULL DEFAULT 'pending',
			ots_status TEXT NOT NULL DEFAULT 'pending',created_at INTEGER NOT NULL,
			UNIQUE(user,device,tst));
		CREATE TABLE share_links (
			id INTEGER PRIMARY KEY,token TEXT NOT NULL UNIQUE,label TEXT NOT NULL DEFAULT '',
			user TEXT NOT NULL DEFAULT '',device TEXT NOT NULL DEFAULT '',hours INTEGER NOT NULL DEFAULT 24,
			expires_at INTEGER,created_at INTEGER NOT NULL,last_access_at INTEGER);
		INSERT INTO locations(user,device,tst,lat,lon,payload,digest,created_at)
		VALUES ('a','p',1,1,2,'{}',x'01',1);
		INSERT INTO share_links(token,label,created_at) VALUES ('legacy-secret','old',1);
	`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if links, err := st.ListShares(); err != nil || len(links) != 0 {
		t.Fatalf("legacy shares were not revoked: %+v err=%v", links, err)
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := check.Query(`PRAGMA table_info(share_links)`)
	if err != nil {
		t.Fatal(err)
	}
	hasHash := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "token" {
			t.Fatal("plaintext token column remains")
		}
		hasHash = hasHash || name == "token_hash"
	}
	_ = rows.Close()
	_ = check.Close()
	if !hasHash {
		t.Fatal("token_hash column missing")
	}
	jobs, err := st.JobStatus()
	if err != nil {
		t.Fatal(err)
	}
	if jobs.Queued != 2 {
		t.Fatalf("queued=%d", jobs.Queued)
	}
	id, err := st.CreateShare(&store.ShareLink{Label: "private", DelayMinutes: 60, PrecisionM: 500})
	if err != nil {
		t.Fatal(err)
	}
	links, _ := st.ListShares()
	if len(links) != 1 || links[0].ID != id || links[0].DelayMinutes != 60 || links[0].PrecisionM != 500 {
		t.Fatalf("share migration: %+v", links)
	}
}

func TestRunningStampJobRecoversAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(sampleLocation()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	job, err := st.ClaimStampJob(now, "test", now+90)
	if err != nil || job == nil {
		t.Fatalf("claim: %+v %v", job, err)
	}
	_ = st.Close()
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.RecoverStampJobs(4); err != nil {
		t.Fatal(err)
	}
	status, err := st.JobStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Running != 0 || status.Queued != 2 {
		t.Fatalf("status=%+v", status)
	}
}

func TestRetryAllStampJobsDoesNotResetRunningLease(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	job, err := st.ClaimStampJob(now, "owner", now+90)
	if err != nil || job == nil {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	if job.Kind == "tsa" {
		err = st.UpdateTSA(id, "fail", nil)
	} else {
		err = st.UpdateOTS(id, "fail")
	}
	if err != nil {
		t.Fatal(err)
	}
	if n, err := st.RetryAllStampJobs(4); err != nil || n != 0 {
		t.Fatalf("retry all=%d err=%v", n, err)
	}
	jobs, err := st.ListStampJobs(10)
	if err != nil {
		t.Fatal(err)
	}
	var got *store.StampJob
	for i := range jobs {
		if jobs[i].LocationID == id && jobs[i].Kind == job.Kind {
			got = &jobs[i]
			break
		}
	}
	if got == nil || got.State != "running" || got.LeaseOwner != "owner" || got.LeaseUntil != job.LeaseUntil {
		t.Fatalf("running job changed: %+v", got)
	}
}

func TestRecoverStampJobsMarksTerminalLocationFailed(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobMaxAttempts(id, 1); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	job, err := st.ClaimStampJob(now, "crashed", now+90)
	if err != nil || job == nil {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	if err := st.RecoverStampJobs(1); err != nil {
		t.Fatal(err)
	}
	loc, err := st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind == "tsa" && loc.TsaStatus != "fail" {
		t.Fatalf("tsa status=%q", loc.TsaStatus)
	}
	if job.Kind == "ots" && loc.OtsStatus != "fail" {
		t.Fatalf("ots status=%q", loc.OtsStatus)
	}
	jobs, err := st.ListStampJobs(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range jobs {
		if candidate.ID == job.ID && candidate.State != "failed" {
			t.Fatalf("recovered job=%+v", candidate)
		}
	}
}

func TestRecoverStampJobsUsesCurrentAttemptLimit(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobMaxAttempts(id, 1); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	job, err := st.ClaimStampJob(now, "crashed", now+90)
	if err != nil || job == nil {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	if err := st.RecoverStampJobs(4); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListStampJobs(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range jobs {
		if candidate.ID == job.ID {
			if candidate.State != "queued" || candidate.MaxAttempts != 4 {
				t.Fatalf("recovered job=%+v", candidate)
			}
			return
		}
	}
	t.Fatal("recovered job missing")
}

func TestRecoverStampJobsFailsRetryAboveCurrentLimit(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	job, err := st.ClaimStampJob(now, "worker", now+90)
	if err != nil || job == nil {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	if retry, err := st.FailStampJob(job, errors.New("temporary")); err != nil || !retry {
		t.Fatalf("retry=%t err=%v", retry, err)
	}
	if err := st.RecoverStampJobs(1); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListStampJobs(10)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range jobs {
		if candidate.ID == job.ID {
			if candidate.State != "failed" || candidate.MaxAttempts != 1 {
				t.Fatalf("recovered job=%+v", candidate)
			}
			loc, err := st.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			if job.Kind == "tsa" && loc.TsaStatus != "fail" || job.Kind == "ots" && loc.OtsStatus != "fail" {
				t.Fatalf("location=%+v", loc)
			}
			return
		}
	}
	t.Fatal("recovered job missing")
}

func TestMigrationV8CreatesMaintenanceTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v8.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		DROP TABLE maintenance_runs;
		DROP TABLE maintenance_jobs;
		DROP TABLE proof_import_commits;
		DROP TABLE app_settings;
		PRAGMA user_version=8;
	`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if version, err := st.SchemaVersion(); err != nil || version != store.CurrentSchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if jobs, err := st.ListMaintenanceJobs("", ""); err != nil || len(jobs) != 0 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
}

func TestBackupIntegrityAndPruning(t *testing.T) {
	st := openTestDB(t)
	if _, err := st.Insert(sampleLocation()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "backups")
	for range 3 {
		if _, err := st.BackupAndPrune(dir, 2); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("backups=%d", len(entries))
	}
	for _, entry := range entries {
		if err := store.CheckDB(filepath.Join(dir, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	if path, modified, err := store.LatestBackup(dir); err != nil || path == "" || modified.IsZero() {
		t.Fatalf("latest=%q modified=%v err=%v", path, modified, err)
	}
}

func TestLatestBackupAfterClockCorrection(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for name, modified := range map[string]time.Time{
		"mytracker-20990101-000000.mtbackup": now.Add(24 * time.Hour),
		"mytracker-20260101-000000.mtbackup": now.Add(-time.Hour),
		"mytracker-20250101-000000.db":       now.Add(-time.Minute),
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	path, modified, err := store.LatestBackup(dir)
	if err != nil || filepath.Base(path) != "mytracker-20250101-000000.db" || !modified.Equal(now.Add(-time.Minute)) {
		t.Fatalf("latest=%q modified=%v err=%v", path, modified, err)
	}
}

func TestDurableStampJobLifecycle(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobMaxAttempts(id, 2); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	tsaJob, err := st.ClaimStampJob(now, "test", now+90)
	if err != nil || tsaJob == nil || tsaJob.Kind != "tsa" {
		t.Fatalf("job=%+v err=%v", tsaJob, err)
	}
	ts := time.Now().Unix()
	if err := st.CompleteStampJob(tsaJob, "ok", &ts); err != nil {
		t.Fatal(err)
	}
	otsJob, err := st.ClaimStampJob(now, "test", now+90)
	if err != nil || otsJob == nil || otsJob.Kind != "ots" {
		t.Fatalf("job=%+v err=%v", otsJob, err)
	}
	if err := st.CompleteStampJob(otsJob, "submitted", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateOTS(id, "fail"); err != nil {
		t.Fatal(err)
	}
	if n, err := st.RetryStampJobs(id, 2); err != nil || n != 1 {
		t.Fatalf("reset ots=%d err=%v", n, err)
	}
	now = time.Now().Unix()
	otsJob, err = st.ClaimStampJob(now, "test", now+90)
	if err != nil || otsJob == nil {
		t.Fatalf("ots reclaim=%+v err=%v", otsJob, err)
	}
	retry, err := st.FailStampJob(otsJob, errors.New("temporary"))
	if err != nil || !retry {
		t.Fatalf("retry=%t err=%v", retry, err)
	}
	now = time.Now().Add(time.Hour).Unix()
	otsJob, err = st.ClaimStampJob(now, "test", now+90)
	if err != nil || otsJob == nil {
		t.Fatalf("retry claim=%+v err=%v", otsJob, err)
	}
	retry, err = st.FailStampJob(otsJob, errors.New("final"))
	if err != nil || retry {
		t.Fatalf("retry=%t err=%v", retry, err)
	}
	status, _ := st.JobStatus()
	if status.Failed != 1 {
		t.Fatalf("status=%+v", status)
	}
	if n, err := st.RetryStampJobs(id, 3); err != nil || n != 1 {
		t.Fatalf("retry jobs=%d err=%v", n, err)
	}
	if n, err := st.RetryAllStampJobs(4); err != nil || n != 0 {
		t.Fatalf("retry all=%d err=%v", n, err)
	}
	jobs, err := st.ListStampJobs(10)
	if err != nil || len(jobs) != 1 || jobs[0].State != "queued" {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
}

func TestDeleteRangeAndCursor(t *testing.T) {
	st := openTestDB(t)
	proofs := t.TempDir()
	for _, tst := range []int64{100, 200, 300} {
		loc := sampleLocation()
		loc.Tst = tst
		id, err := st.Insert(loc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proofs, fmt.Sprintf("%d.ots", id)), []byte("proof"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	page, err := st.ListCursor(0, 2)
	if err != nil || len(page) != 2 || page[0].ID <= page[1].ID {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	n, err := st.DeleteRange(proofs, store.ExportFilter{From: 150, To: 250})
	if err != nil || n != 1 {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
	if count, _ := st.Count(); count != 2 {
		t.Fatalf("count=%d", count)
	}
	if err := st.Ready(); err != nil {
		t.Fatal(err)
	}
	if err := st.Ping(); err != nil {
		t.Fatal(err)
	}
	if older, err := st.ListCursor(page[1].ID, 10); err != nil || len(older) != 1 {
		t.Fatalf("older=%+v err=%v", older, err)
	}
}

func TestSharePresentationHelpers(t *testing.T) {
	future := store.ShareExpiresAt(1)
	if !future.Valid || store.FormatShareExpiry(future) == "never" {
		t.Fatal("future expiry")
	}
	if got := store.FormatShareExpiry(sql.NullInt64{}); got != "never" {
		t.Fatalf("expiry=%q", got)
	}
	cases := []struct {
		link store.ShareLink
		want string
	}{
		{store.ShareLink{}, "all devices"},
		{store.ShareLink{User: "a"}, "user a"},
		{store.ShareLink{Device: "p"}, "device p"},
		{store.ShareLink{User: "a", Device: "p"}, "a / p"},
	}
	for _, tc := range cases {
		if got := tc.link.FilterDesc(); got != tc.want {
			t.Fatalf("got=%q want=%q", got, tc.want)
		}
	}
	if !(&store.ShareLink{}).Active() || !(&store.ShareLink{ExpiresAt: future}).Active() {
		t.Fatal("active share")
	}
}

func TestStoreDefaultAndErrorBranches(t *testing.T) {
	st := openTestDB(t)
	if _, _, err := store.LatestBackup(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Fatalf("latest error=%v", err)
	}
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobMaxAttempts(id, 0); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	job, err := st.ClaimStampJob(now, "test", now+90)
	if err != nil || job == nil {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	if retry, err := st.FailStampJob(job, errors.New("failed")); err != nil || retry {
		t.Fatalf("retry=%t err=%v", retry, err)
	}
	if n, err := st.RetryAllStampJobs(2); err != nil || n != 1 {
		t.Fatalf("retry all=%d err=%v", n, err)
	}
	loc, err := st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if loc.TsaStatus != "pending" {
		t.Fatalf("retry status=%q", loc.TsaStatus)
	}
	if err := st.CompleteStampJob(&store.StampJob{Kind: "tsa"}, "ok", nil); err == nil {
		t.Fatal("missing tsa time accepted")
	}
	if _, err := st.ListCursor(0, 0); err != nil {
		t.Fatal(err)
	}
	if jobs, err := st.ListStampJobs(1000); err != nil || len(jobs) != 2 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	if n, err := st.DeleteOlderThan(t.TempDir(), 0, "tst"); err != nil || n != 0 {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
}

func TestLeaseReclaimAndProofGCRecovery(t *testing.T) {
	st := openTestDB(t)
	proofs := t.TempDir()
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	first, err := st.ClaimStampJob(now, "first", now-1)
	if err != nil || first == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := st.ClaimStampJob(now, "second", now+90)
	if err != nil || second == nil || second.ID != first.ID {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	ts := int64(1)
	if err := st.CompleteStampJob(first, "ok", &ts); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("old owner completed: %v", err)
	}
	if ok, err := st.RenewStampJob(second.ID, "second", now+120); err != nil || !ok {
		t.Fatalf("renew=%t err=%v", ok, err)
	}
	if err := st.CompleteStampJob(second, "ok", &ts); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proofs, fmt.Sprintf("%d.tsr", id)), []byte("proof"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteRange(proofs, store.ExportFilter{From: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(proofs, fmt.Sprintf("%d.tsr", id))); !os.IsNotExist(err) {
		t.Fatalf("proof remains: %v", err)
	}
	gc, err := st.ProofGCStatus()
	if err != nil || gc.Pending != 0 {
		t.Fatalf("gc=%+v err=%v", gc, err)
	}
}

func TestExpiredStampLeaseCannotComplete(t *testing.T) {
	st := openTestDB(t)
	if _, err := st.Insert(sampleLocation()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	job, err := st.ClaimStampJob(now, "expired", now-1)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	ts := now
	err = st.CompleteStampJob(job, "ok", &ts)
	if !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("err=%v", err)
	}
}

func TestExpiredStampLeaseCannotRecordFailure(t *testing.T) {
	st := openTestDB(t)
	if _, err := st.Insert(sampleLocation()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	job, err := st.ClaimStampJob(now, "expired", now-1)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if ok, err := st.RenewStampJob(job.ID, "expired", now+90); err != nil || ok {
		t.Fatalf("expired renew=%t err=%v", ok, err)
	}
	if _, err := st.FailStampJob(job, errors.New("network")); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("expired lease err=%v", err)
	}
}

func TestProofGCResumesAfterDeleteFailureAndRestart(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "tracker.db")
	proofs := filepath.Join(root, "proofs")
	if err := os.MkdirAll(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	loc := sampleLocation()
	loc.TsaStatus = "ok"
	id, err := st.ImportLocation(loc)
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(proofs, fmt.Sprintf("%d.tsr", id))
	if err := os.MkdirAll(filepath.Join(blocked, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteRange(proofs, store.ExportFilter{From: 1}); !errors.Is(err, store.ErrDeletionPending) {
		t.Fatalf("delete error=%v", err)
	}
	gc, err := st.ProofGCStatus()
	if err != nil || gc.Pending != 1 || gc.Failed != 1 {
		t.Fatalf("gc=%+v err=%v", gc, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(blocked); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	time.Sleep(1100 * time.Millisecond)
	if n, err := st.ProcessProofGC(proofs, 10); err != nil || n != 1 {
		t.Fatalf("processed=%d err=%v", n, err)
	}
	gc, err = st.ProofGCStatus()
	if err != nil || gc.Pending != 0 {
		t.Fatalf("gc=%+v err=%v", gc, err)
	}
}

func TestProofGCKeepsQueueWhenProofDirectoryIsUnavailable(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "tracker.db")
	proofs := filepath.Join(root, "proofs")
	if err := os.MkdirAll(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	loc := sampleLocation()
	loc.TsaStatus = "ok"
	id, err := st.ImportLocation(loc)
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(proofs, fmt.Sprintf("%d.tsr", id))
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteRange(proofs, store.ExportFilter{From: 1}); !errors.Is(err, store.ErrDeletionPending) {
		t.Fatalf("delete error=%v", err)
	}
	missing := filepath.Join(root, "proofs-missing")
	if err := os.Rename(proofs, missing); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if n, err := st.ProcessProofGC(proofs, 10); n != 0 || err == nil {
		t.Fatalf("unavailable directory processed=%d err=%v", n, err)
	}
	gc, err := st.ProofGCStatus()
	if err != nil || gc.Pending != 1 || gc.Failed != 1 {
		t.Fatalf("gc=%+v err=%v", gc, err)
	}
}

func TestDoctorAuditSchemaAndExpiredLeases(t *testing.T) {
	st := openTestDB(t)
	proofs := t.TempDir()
	loc := sampleLocation()
	loc.TsaStatus = "ok"
	loc.OtsStatus = "submitted"
	id, err := st.ImportLocation(loc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proofs, "orphan.tsr"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{".tsr", ".ots"} {
		if err := os.WriteFile(filepath.Join(proofs, fmt.Sprintf("%d%s", id, ext)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(proofs, "ignored.tsr"), 0700); err != nil {
		t.Fatal(err)
	}
	audit, err := st.AuditProofs(proofs)
	if err != nil || len(audit.Missing) != 0 || len(audit.Orphan) != 1 {
		t.Fatalf("audit=%+v err=%v", audit, err)
	}
	if version, err := st.SchemaVersion(); err != nil || version != store.CurrentSchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	queued := sampleLocation()
	queued.Device = "queued"
	queued.Tst++
	queuedID, err := st.Insert(queued)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobMaxAttempts(queuedID, 1); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	job, err := st.ClaimStampJob(now, "expired", now-1)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if n, err := st.ExpiredLeaseCount(now); err != nil || n != 1 {
		t.Fatalf("expired=%d err=%v", n, err)
	}
	if n, err := st.ExpireStampJobLeases(now); err != nil || n != 1 {
		t.Fatalf("final failures=%d err=%v", n, err)
	}
	jobs, err := st.ListLocationStampJobs(id)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
}

func TestCheckpointReportsBusyReader(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "checkpoint.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	first := sampleLocation()
	if _, err := st.Insert(first); err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(50)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	tx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM locations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	second := sampleLocation()
	second.Device = "tablet"
	second.Tst++
	if _, err := st.Insert(second); err != nil {
		t.Fatal(err)
	}
	deleted, err := st.DeleteRange(t.TempDir(), store.ExportFilter{From: second.Tst})
	if deleted != 1 || !errors.Is(err, store.ErrDeletionPending) || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatal(err)
	}
}

func TestJobErrorsAreRedactedAndProofGCRemovesFiles(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	job, err := st.ClaimStampJob(now, "worker", now+90)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if err := st.SetJobMaxAttempts(id, 1); err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("x", 1100)
	if _, err := st.FailStampJob(job, fmt.Errorf("call https://user:pass@example.com/a?token=abc password=bad %s", secret)); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListLocationStampJobs(id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(jobs[0].LastError, "pass") || strings.Contains(jobs[0].LastError, "abc") ||
		len(jobs[0].LastError) > 1000 {
		t.Fatalf("unsafe error=%q", jobs[0].LastError)
	}
	if store.SafeJobError(nil) != "" || !strings.Contains(store.SafeJobError(errors.New("http://%")), "redacted") {
		t.Fatal("error sanitizer fallback")
	}
	if filtered, err := st.ListStampJobsByState("failed", 1); err != nil || len(filtered) != 1 {
		t.Fatalf("filtered=%+v err=%v", filtered, err)
	}
	if _, err := st.ListStampJobsByState("unknown", 1); err == nil {
		t.Fatal("invalid state accepted")
	}
	proofs := t.TempDir()
	for _, ext := range []string{".tsr", ".ots"} {
		if err := os.WriteFile(filepath.Join(proofs, fmt.Sprintf("%d%s", id, ext)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DeleteIDs([]int64{id}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.ProcessProofGC(proofs, 10); err != nil || n != 2 {
		t.Fatalf("processed=%d err=%v", n, err)
	}
	if status, err := st.ProofGCStatus(); err != nil || status.Pending != 0 {
		t.Fatalf("proof GC status=%+v err=%v", status, err)
	}
	for _, ext := range []string{".tsr", ".ots"} {
		if _, err := os.Stat(filepath.Join(proofs, fmt.Sprintf("%d%s", id, ext))); !os.IsNotExist(err) {
			t.Fatalf("proof remains: %v", err)
		}
	}
}

func TestUnknownClaimedJobKindRollsBack(t *testing.T) {
	st := openTestDB(t)
	if _, err := st.Insert(sampleLocation()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	job, err := st.ClaimStampJob(now, "worker", now+90)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	job.Kind = "unknown"
	if err := st.CompleteStampJob(job, "ok", nil); err == nil {
		t.Fatal("unknown kind completed")
	}
	job.Kind = "tsa"
	if _, err := st.FailStampJob(job, errors.New("stop")); err != nil {
		t.Fatal(err)
	}
}

func TestImportIterationRetentionAndHelpers(t *testing.T) {
	st := openTestDB(t)
	proofs := t.TempDir()
	for _, tst := range []int64{100, 200, 300} {
		loc := sampleLocation()
		loc.Tst = tst
		loc.TsaStatus = "pending"
		loc.OtsStatus = "pending"
		loc.CreatedAt = tst
		if _, err := st.ImportLocation(loc); err != nil {
			t.Fatal(err)
		}
	}
	seen := 0
	if err := st.IterateExport(store.ExportFilter{}, 1, func(locs []store.Location) error {
		seen += len(locs)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 3 {
		t.Fatalf("seen=%d", seen)
	}
	if n, err := st.CountExport(store.ExportFilter{From: 150}); err != nil || n != 2 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	last, err := st.LastLocation()
	if err != nil || last.Tst != 300 {
		t.Fatalf("last=%+v err=%v", last, err)
	}
	last, err = st.LastLocationFor("alice", "phone")
	if err != nil || last.Tst != 300 {
		t.Fatalf("filtered last=%+v err=%v", last, err)
	}
	n, err := st.DeleteOlderThan(proofs, 250, "received")
	if err != nil || n != 2 {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
	locs, err := st.ListFailed()
	if err != nil || len(locs) != 0 {
		t.Fatalf("failed=%+v err=%v", locs, err)
	}
	remaining, _ := st.List(1, 0)
	if err := st.SetImportedStatus(remaining[0].ID, "ok", "confirmed", sql.NullInt64{Int64: 1, Valid: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteIDs([]int64{remaining[0].ID}); err != nil {
		t.Fatal(err)
	}
	if status, err := st.ProofGCStatus(); err != nil || status.Pending != 2 {
		t.Fatalf("proof GC status=%+v err=%v", status, err)
	}
	if store.FormatTime(0) == "" || store.DigestHex([]byte{0xab}) != "ab" {
		t.Fatal("format helpers")
	}
}
