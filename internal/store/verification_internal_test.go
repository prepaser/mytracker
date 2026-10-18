package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestVerificationEvidenceStatusDistinguishesUnknown(t *testing.T) {
	tests := []struct {
		valid       bool
		code, check string
		want        string
	}{
		{true, "", "pow", "valid"},
		{false, "header_pow_invalid", "pow", "invalid"},
		{false, "header_pow_invalid", "merkle", "unknown"},
		{false, "provider_unavailable", "pow", "unknown"},
	}
	for _, tt := range tests {
		if got := VerificationEvidenceStatus(tt.valid, tt.code, tt.check); got != tt.want {
			t.Errorf("status(%t,%q,%q)=%q want %q", tt.valid, tt.code, tt.check, got, tt.want)
		}
	}
}

func TestVerificationAttemptsReadRejectsOversizedAndDuplicateRows(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "verification.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	run := &VerificationRun{LocationID: id, Kind: "ots", Trigger: "manual", Result: "error", CheckedAt: 1}
	if err := st.SaveVerification(run); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= MaxVerificationAttempts+1; i++ {
		if _, err := st.db.Exec(`INSERT INTO proof_verification_attempts
			(run_id,provider_id,result) VALUES (?,?,?)`, run.ID, "esplora-"+string(rune('0'+i)), "error"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.verificationAttempts(run.ID); !errors.Is(err, ErrTooManyVerificationAttempts) {
		t.Fatalf("oversized read error=%v", err)
	}
	if _, err := st.db.Exec(`DELETE FROM proof_verification_attempts WHERE run_id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := st.db.Exec(`INSERT INTO proof_verification_attempts
			(run_id,provider_id,result) VALUES (?,?,?)`, run.ID, "esplora-1", "error"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.verificationAttempts(run.ID); !errors.Is(err, ErrDuplicateVerificationProvider) {
		t.Fatalf("duplicate read error=%v", err)
	}
}

func TestVerificationProviderStatusesUsesLatestEligibleAttempt(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "verification.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	insertLocation := func(device string) int64 {
		t.Helper()
		id, err := st.Insert(&Location{
			User: "u", Device: device, Tst: 1, Lat: 1, Lon: 2,
			Payload: []byte(`{}`), Digest: []byte{1},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	save := func(locationID int64, kind, trigger string, engine int, checked int64, attempt VerificationAttempt) {
		t.Helper()
		if err := st.SaveVerification(&VerificationRun{
			LocationID: locationID, Kind: kind, Trigger: trigger,
			EngineVersion: engine, Result: attempt.Result, CheckedAt: checked,
			Attempts: []VerificationAttempt{attempt},
		}); err != nil {
			t.Fatal(err)
		}
	}

	first := insertLocation("first")
	save(first, "ots", "manual", 1, 100, VerificationAttempt{
		ProviderID: "esplora-1", Result: "verified", DurationMS: 11,
		Confirmations: 7, HeaderValid: true, POWValid: true,
	})
	if err := st.ImportVerification(&VerificationRun{
		LocationID: first, Kind: "ots", Result: "error", CheckedAt: 200,
	}, []VerificationAttempt{{ProviderID: "esplora-1", Result: "error", DurationMS: 99}}); err != nil {
		t.Fatal(err)
	}

	second := insertLocation("second")
	save(second, "tsa", "import", 2, 300, VerificationAttempt{
		ProviderID: "esplora-2", Result: "verified", DurationMS: 22,
		Confirmations: 8, POWValid: true,
	})
	if _, err := st.db.Exec(`
		INSERT INTO proof_verification_jobs
			(location_id,kind,trigger,state,created_at,updated_at)
		VALUES (?,?,'import','done',?,?)`, second, "tsa", 300, 300); err != nil {
		t.Fatal(err)
	}

	retired := insertLocation("retired")
	if err := st.ImportVerification(&VerificationRun{
		LocationID: retired, Kind: "ots", Result: "verified", CheckedAt: 400,
	}, []VerificationAttempt{{ProviderID: "esplora-3", Result: "verified", DurationMS: 33}}); err != nil {
		t.Fatal(err)
	}

	statuses, err := st.VerificationProviderStatuses()
	if err != nil {
		t.Fatal(err)
	}
	want := []VerificationProviderStatus{
		{ProviderID: "esplora-1", Result: "verified", DurationMS: 11, CheckedAt: 100,
			Confirmations: 7, HeaderValid: true, POWValid: true, MerkleStatus: "unknown",
			HeaderStatus: "valid", POWStatus: "valid", ErrorCode: ""},
		{ProviderID: "esplora-2", Result: "verified", DurationMS: 22, CheckedAt: 300,
			Confirmations: 8, POWValid: true, HeaderStatus: "unknown", POWStatus: "valid",
			MerkleStatus: "unknown", ErrorCode: ""},
	}
	if !reflect.DeepEqual(statuses, want) {
		t.Fatalf("statuses=%+v want=%+v", statuses, want)
	}
}
