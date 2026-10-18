package store_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/store"
)

func TestVerificationAttemptsAreBoundedAndUnique(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	tooMany := make([]store.VerificationAttempt, store.MaxVerificationAttempts+1)
	for i := range tooMany {
		tooMany[i] = store.VerificationAttempt{ProviderID: "esplora-" + string(rune('1'+i)), Result: "error"}
	}
	if err := st.SaveVerification(&store.VerificationRun{
		LocationID: id, Kind: "ots", Result: "error", CheckedAt: 1, Attempts: tooMany,
	}); !errors.Is(err, store.ErrTooManyVerificationAttempts) {
		t.Fatalf("too many attempts error=%v", err)
	}
	duplicate := []store.VerificationAttempt{
		{ProviderID: "esplora-1", Result: "error"},
		{ProviderID: "esplora-1", Result: "error"},
	}
	if err := st.SaveVerification(&store.VerificationRun{
		LocationID: id, Kind: "ots", Result: "error", CheckedAt: 1, Attempts: duplicate,
	}); !errors.Is(err, store.ErrDuplicateVerificationProvider) {
		t.Fatalf("duplicate attempt error=%v", err)
	}
}

func TestVerificationHistoryAndStats(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	for i, result := range []string{"degraded", "verified"} {
		run := &store.VerificationRun{
			LocationID: id, Kind: "tsa", Trigger: "manual", Result: result,
			ProofSHA256: []byte{byte(i)}, CheckedAt: time.Now().Unix() + int64(i),
			TSAGenTime: int64Ptr(123),
			Attempts: []store.VerificationAttempt{{
				ProviderID: "tsa", Result: "verified", DurationMS: 1,
			}},
		}
		if err := st.SaveVerification(run); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := st.LatestVerification(id, "tsa")
	if err != nil || latest.Result != "verified" || len(latest.Attempts) != 1 {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
	history, err := st.VerificationHistory(id, 0, 50)
	if err != nil || len(history) != 2 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	stats, err := st.VerificationStats()
	if err != nil || stats.Results["verified"] != 1 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}

func int64Ptr(v int64) *int64 { return &v }

func TestVerificationHistoryCleanupKeepsLatest(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	for _, checked := range []int64{1, 2} {
		if err := st.SaveVerification(&store.VerificationRun{
			LocationID: id, Kind: "ots", Trigger: "scheduled", Result: "pending",
			CheckedAt: checked,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.DeleteVerificationHistory(3); err != nil || n != 1 {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
	history, err := st.VerificationHistory(id, 0, 50)
	if err != nil || len(history) != 1 || history[0].CheckedAt != 2 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
}

func TestVerificationPolicyStaleCount(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	tsaPolicy, otsPolicy := []byte("tsa"), []byte("ots")
	if n, err := st.VerificationPolicyStaleCount(2, tsaPolicy, otsPolicy); err != nil || n != 1 {
		t.Fatalf("initial stale=%d err=%v", n, err)
	}
	for kind, policy := range map[string][]byte{"tsa": tsaPolicy, "ots": otsPolicy} {
		if err := st.SaveVerification(&store.VerificationRun{
			LocationID: id, Kind: kind, Trigger: "manual", Result: "verified",
			EngineVersion: 2, PolicySHA256: policy, CheckedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.VerificationPolicyStaleCount(2, tsaPolicy, otsPolicy); err != nil || n != 0 {
		t.Fatalf("matching stale=%d err=%v", n, err)
	}
	if n, err := st.VerificationPolicyStaleCount(3, tsaPolicy, otsPolicy); err != nil || n != 1 {
		t.Fatalf("engine stale=%d err=%v", n, err)
	}
}

func TestVerificationStaleCountIncludesPolicyAndActiveJobs(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	tsaPolicy, otsPolicy := []byte("tsa"), []byte("ots")
	if n, err := st.VerificationStaleCount(now-1, 2, tsaPolicy, otsPolicy); err != nil || n != 1 {
		t.Fatalf("missing stale=%d err=%v", n, err)
	}
	for kind, policy := range map[string][]byte{"tsa": tsaPolicy, "ots": otsPolicy} {
		if err := st.SaveVerification(&store.VerificationRun{
			LocationID: id, Kind: kind, Trigger: "manual", Result: "verified",
			EngineVersion: 2, PolicySHA256: policy, CheckedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.VerificationStaleCount(now-1, 2, tsaPolicy, otsPolicy); err != nil || n != 0 {
		t.Fatalf("fresh stale=%d err=%v", n, err)
	}
	if _, _, err := st.EnqueueVerification(id, "tsa", "manual", now); err != nil {
		t.Fatal(err)
	}
	if n, err := st.VerificationStaleCount(now-1, 2, tsaPolicy, otsPolicy); err != nil || n != 1 {
		t.Fatalf("active stale=%d err=%v", n, err)
	}
}

func TestVerificationImportIterationAndProviders(t *testing.T) {
	st := openTestDB(t)
	first, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	secondLoc := sampleLocation()
	secondLoc.Device = "tablet"
	secondLoc.Tst++
	second, err := st.Insert(secondLoc)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		location int64
		checked  int64
		provider string
	}{
		{first, 10, "esplora-1"},
		{second, 20, "esplora-2"},
	} {
		if err := st.SaveVerification(&store.VerificationRun{
			LocationID: item.location, Kind: "ots", Trigger: "manual", Result: "verified",
			ProofSHA256: []byte{1}, CheckedAt: item.checked,
			Attempts: []store.VerificationAttempt{{
				ProviderID: item.provider, Result: "verified", DurationMS: item.checked,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	providers, err := st.VerificationProviderStatuses()
	if err != nil || len(providers) != 2 || providers[1].ProviderID != "esplora-2" {
		t.Fatalf("providers=%+v err=%v", providers, err)
	}
	var runs, attempts int
	if err := st.IterateVerificationRuns(store.ExportFilter{}, 1, func(page []store.VerificationRun) error {
		runs += len(page)
		return nil
	}); err != nil || runs != 2 {
		t.Fatalf("runs=%d err=%v", runs, err)
	}
	if err := st.IterateVerificationAttempts(store.ExportFilter{}, 1, func(page []store.VerificationAttempt) error {
		attempts += len(page)
		return nil
	}); err != nil || attempts != 2 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}

	importedLoc := sampleLocation()
	importedLoc.Device = "imported"
	importedLoc.Tst += 2
	var importedID int64
	err = st.ImportAtomic(func(tx *store.ImportTx) error {
		var err error
		importedID, err = tx.InsertLocation(importedLoc)
		if err != nil {
			return err
		}
		existingID, digest, err := tx.ExistingLocation(importedLoc.User, importedLoc.Device, importedLoc.Tst)
		if err != nil {
			return err
		}
		if existingID != importedID || !bytes.Equal(digest, importedLoc.Digest) {
			return errors.New("existing location mismatch")
		}
		runID, err := tx.InsertVerification(&store.VerificationRun{
			LocationID: importedID, Kind: "ots", Trigger: "manual",
			Result: "degraded", CheckedAt: 30,
		})
		if err != nil {
			return err
		}
		return tx.InsertVerificationAttempt(runID, &store.VerificationAttempt{
			ProviderID: "esplora-1", Result: "verified",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	imported, err := st.LatestVerification(importedID, "ots")
	if err != nil || imported.Trigger != "import" || imported.EngineVersion != 0 || len(imported.Attempts) != 1 {
		t.Fatalf("imported=%+v err=%v", imported, err)
	}
	if err := st.ImportVerification(&store.VerificationRun{
		LocationID: importedID, Kind: "tsa", Result: "missing", CheckedAt: 31,
	}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestValidateVerification(t *testing.T) {
	if err := store.ValidateVerification(nil); err == nil {
		t.Fatal("nil verification accepted")
	}
	if err := store.ValidateVerification(&store.VerificationRun{LocationID: 1, Kind: "bad"}); err == nil {
		t.Fatal("bad kind accepted")
	}
	run := &store.VerificationRun{LocationID: 1, Kind: "tsa"}
	if err := store.ValidateVerification(run); err != nil || run.CheckedAt == 0 {
		t.Fatalf("run=%+v err=%v", run, err)
	}
}
