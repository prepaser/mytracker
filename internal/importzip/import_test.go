package importzip_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/export"
	"github.com/prepaser/mytracker/internal/importzip"
	"github.com/prepaser/mytracker/internal/store"
)

type zipEntry struct {
	name string
	data string
}

type cancelChangeSink struct{ cancel context.CancelFunc }

func (s cancelChangeSink) Publish(change store.Change) {
	if change.Topic == store.ChangeLocations {
		s.cancel()
	}
}

type noopStamper struct{}

func (noopStamper) Process(int64, [32]byte) {}

func makeZip(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		w, err := zw.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImportExportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"_type":"location","lat":37.5,"lon":127.0,"tst":1751800000}`)
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	rec, err := canonical.FromLocation("alice", "phone", raw)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := rec.Digest()
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.ImportLocation(&store.Location{
		User: "alice", Device: "phone", Tst: 1751800000, Lat: 37.5, Lon: 127.0,
		Payload: payload, Digest: digest[:], TSANonce: []byte{1, 2, 3, 4},
		TsaStatus: "ok", OtsStatus: "submitted",
	})
	if err != nil {
		t.Fatal(err)
	}
	proofs := filepath.Join(dir, "proofs")
	if err := os.MkdirAll(proofs, 0755); err != nil {
		t.Fatal(err)
	}
	for _, proof := range []struct {
		ext  string
		body []byte
	}{
		{".tsr", []byte("tsr")},
		{".ots", []byte("ots")},
	} {
		if err := os.WriteFile(filepath.Join(proofs, "1"+proof.ext), proof.body, 0644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(proof.body)
		kind, result := "tsa", "degraded"
		var attempts []store.VerificationAttempt
		if proof.ext == ".ots" {
			kind, result = "ots", "verified"
			attempts = []store.VerificationAttempt{{
				ProviderID: "esplora-1", Result: "verified", DurationMS: 2,
			}}
		}
		if err := st.SaveVerification(&store.VerificationRun{
			LocationID: 1, Kind: kind, Trigger: "manual", Result: result,
			ProofSHA256: sum[:], PolicySHA256: sum[:], EngineVersion: 2,
			CheckedAt: 100, Attempts: attempts,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := export.WriteZip(&buf, export.Options{Store: st, ProofsDir: proofs}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	st2, err := store.Open(filepath.Join(dir, "import.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	proofs2 := filepath.Join(dir, "proofs2")
	if err := os.MkdirAll(proofs2, 0755); err != nil {
		t.Fatal(err)
	}
	validated, err := importzip.ValidateArchiveContext(t.Context(), bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = validated.Close() }()
	res, err := importzip.ImportValidatedContext(t.Context(), validated, st2, proofs2, nil, 7)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 1 || res.Proofs != 2 || res.Verifications != 3 {
		t.Fatalf("import: %+v", res)
	}
	stampJobs, err := st2.ListLocationStampJobs(1)
	if err != nil || len(stampJobs) != 2 || stampJobs[0].MaxAttempts != 7 || stampJobs[1].MaxAttempts != 7 {
		t.Fatalf("stamp jobs=%+v err=%v", stampJobs, err)
	}
	latest, err := st2.LatestVerification(1, "ots")
	if err != nil || latest.Trigger != "import" || latest.EngineVersion != 0 || len(latest.Attempts) != 1 {
		t.Fatalf("verification=%+v err=%v", latest, err)
	}
	importedLoc, err := st2.Get(1)
	otsPolicy := sha256.Sum256([]byte("ots"))
	if err != nil || !bytes.Equal(importedLoc.TSANonce, []byte{1, 2, 3, 4}) ||
		!bytes.Equal(latest.PolicySHA256, otsPolicy[:]) {
		t.Fatalf("v4 metadata loc=%+v verification=%+v err=%v", importedLoc, latest, err)
	}
	jobs, err := st2.ListLocationVerificationJobs(1)
	if err != nil || len(jobs) != 2 || jobs[0].State != "queued" || jobs[1].State != "queued" {
		t.Fatalf("verification jobs=%+v err=%v", jobs, err)
	}
	verified, err := importzip.Verify(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil || verified.Imported != 1 || verified.Verifications != 3 {
		t.Fatalf("verify: %+v err=%v", verified, err)
	}
	archivePath := filepath.Join(dir, "export.zip")
	if err := os.WriteFile(archivePath, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if verified, err = importzip.VerifyFile(archivePath); err != nil || verified.Imported != 1 {
		t.Fatalf("verify file: %+v err=%v", verified, err)
	}
	auditTmp := filepath.Join(dir, "audit-tmp")
	if err := os.Mkdir(auditTmp, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", auditTmp)
	cfg := config.Default()
	cfg.EsploraURLs = []string{"http://127.0.0.1:1"}
	report, err := importzip.AuditFile(t.Context(), archivePath, cfg)
	if err != nil || report.Version != 1 || report.ArchiveVersion != 6 ||
		report.Overall != "invalid" || importzip.AuditExitCode(report) != 1 {
		t.Fatalf("audit=%+v err=%v", report, err)
	}
	reportJSON, _ := json.Marshal(report)
	if bytes.Contains(reportJSON, []byte(`"latitude"`)) ||
		bytes.Contains(reportJSON, []byte("127.0.0.1")) {
		t.Fatalf("sensitive audit output: %s", reportJSON)
	}
	if entries, _ := os.ReadDir(auditTmp); len(entries) != 0 {
		t.Fatalf("audit temporary files remain: %v", entries)
	}
	if err := os.Remove(filepath.Join(proofs2, "1.tsr")); err != nil {
		t.Fatal(err)
	}
	res2, err := importzip.ImportBytes(buf.Bytes(), st2, proofs2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Skipped != 1 || res2.Proofs != 1 {
		t.Fatalf("duplicate skip: %+v", res2)
	}
	if body, err := os.ReadFile(filepath.Join(proofs2, "1.tsr")); err != nil || string(body) != "tsr" {
		t.Fatalf("restored proof=%q err=%v", body, err)
	}
}

func TestAuditEmptyArchiveIsInconclusive(t *testing.T) {
	data := makeZip(t, zipEntry{"manifest.json", `{"version":2}`}, zipEntry{"locations.jsonl", ""})
	path := filepath.Join(t.TempDir(), "empty.zip")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	report, err := importzip.AuditFile(t.Context(), path, config.Default())
	if err != nil || report.Overall != "inconclusive" || len(report.Locations) != 0 || importzip.AuditExitCode(report) != 2 {
		t.Fatalf("empty audit=%+v err=%v", report, err)
	}
}

func TestDuplicateImportRestoresMetadataOnlyWithMissingProof(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	proofs := filepath.Join(dir, "proofs")
	if err := os.MkdirAll(proofs, 0700); err != nil {
		t.Fatal(err)
	}

	archiveTime := int64(1_700_000_000)
	oldTime := int64(1_600_000_000)
	makeLocation := func(tst int64, nonce byte) (int64, []byte) {
		t.Helper()
		payload := json.RawMessage(fmt.Sprintf(`{"_type":"location","lat":1,"lon":2,"tst":%d}`, tst))
		var raw map[string]any
		if err := json.Unmarshal(payload, &raw); err != nil {
			t.Fatal(err)
		}
		canonicalRecord, err := canonical.FromLocation("u", "d", raw)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := canonicalRecord.Digest()
		if err != nil {
			t.Fatal(err)
		}
		id, err := st.ImportLocation(&store.Location{
			User: "u", Device: "d", Tst: tst, Lat: 1, Lon: 2, Payload: payload,
			Digest: digest[:], TSANonce: []byte{nonce},
			TsaTime:   sql.NullInt64{Int64: oldTime, Valid: true},
			TsaStatus: "pending", OtsStatus: "confirmed",
		})
		if err != nil {
			t.Fatal(err)
		}
		return id, digest[:]
	}

	firstID, firstDigest := makeLocation(100, 1)
	secondID, secondDigest := makeLocation(200, 9)
	now := time.Now().Unix()
	for _, owner := range []string{"worker-first", "worker-second"} {
		job, err := st.ClaimStampJob(now, owner, now+60)
		if err != nil || job == nil || job.Kind != "tsa" {
			t.Fatalf("claim TSA job: job=%+v err=%v", job, err)
		}
	}
	if err := os.WriteFile(filepath.Join(proofs, fmt.Sprintf("%d.tsr", secondID)), []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}

	makeRecord := func(id int64, tst int64, digest []byte, nonce byte) string {
		t.Helper()
		rec, err := json.Marshal(export.Record{
			ID: id, User: "u", Device: "d", EventUnix: tst,
			Payload:      json.RawMessage(fmt.Sprintf(`{"_type":"location","lat":1,"lon":2,"tst":%d}`, tst)),
			DigestSHA256: fmt.Sprintf("%x", digest), TSANonceHex: fmt.Sprintf("%x", []byte{nonce}),
			TsaStatus: "ok", OtsStatus: "pending",
			TsaTimeUTC: time.Unix(archiveTime, 0).UTC().Format(time.RFC3339),
			Proofs:     export.ProofPaths{TSR: fmt.Sprintf("proofs/%d.tsr", id)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(rec)
	}
	archive := makeZip(t,
		zipEntry{"manifest.json", `{"version":2}`},
		zipEntry{"locations.jsonl", makeRecord(101, 100, firstDigest, 2) + "\n" + makeRecord(202, 200, secondDigest, 3) + "\n"},
		zipEntry{"proofs/101.tsr", "restored"},
		zipEntry{"proofs/202.tsr", "archive"},
	)
	result, err := importzip.ImportBytes(archive, st, proofs, nil)
	if err != nil || result.Skipped != 2 || result.Proofs != 1 {
		t.Fatalf("import result=%+v err=%v", result, err)
	}

	first, err := st.Get(firstID)
	if err != nil || first.TsaStatus != "ok" || !first.TsaTime.Valid || first.TsaTime.Int64 != archiveTime ||
		!bytes.Equal(first.TSANonce, []byte{2}) || first.OtsStatus != "confirmed" {
		t.Fatalf("restored metadata=%+v err=%v", first, err)
	}
	if body, err := os.ReadFile(filepath.Join(proofs, fmt.Sprintf("%d.tsr", firstID))); err != nil || string(body) != "restored" {
		t.Fatalf("restored proof=%q err=%v", body, err)
	}
	firstJobs, err := st.ListLocationStampJobs(firstID)
	if err != nil || len(firstJobs) != 2 {
		t.Fatalf("restored stamp jobs=%+v err=%v", firstJobs, err)
	}
	var firstTSA store.StampJob
	for _, job := range firstJobs {
		if job.Kind == "tsa" {
			firstTSA = job
		}
	}
	if firstTSA.State != "done" || firstTSA.LeaseOwner != "" || firstTSA.LeaseUntil != 0 {
		t.Fatalf("restored TSA job=%+v", firstTSA)
	}
	if valid, err := st.StampJobLeaseValid(firstTSA.ID, "worker-first", time.Now().Unix()); err != nil || valid {
		t.Fatalf("restored TSA lease valid=%t err=%v", valid, err)
	}

	second, err := st.Get(secondID)
	if err != nil || second.TsaStatus != "pending" || !second.TsaTime.Valid || second.TsaTime.Int64 != oldTime ||
		!bytes.Equal(second.TSANonce, []byte{9}) || second.OtsStatus != "confirmed" {
		t.Fatalf("existing-proof metadata=%+v err=%v", second, err)
	}
	if body, err := os.ReadFile(filepath.Join(proofs, fmt.Sprintf("%d.tsr", secondID))); err != nil || string(body) != "existing" {
		t.Fatalf("existing proof=%q err=%v", body, err)
	}
	secondJobs, err := st.ListLocationStampJobs(secondID)
	if err != nil || len(secondJobs) != 2 {
		t.Fatalf("existing-proof stamp jobs=%+v err=%v", secondJobs, err)
	}
	var secondTSA store.StampJob
	for _, job := range secondJobs {
		if job.Kind == "tsa" {
			secondTSA = job
		}
	}
	if secondTSA.State != "running" || secondTSA.LeaseOwner != "worker-second" {
		t.Fatalf("existing-proof TSA job=%+v", secondTSA)
	}
	if valid, err := st.StampJobLeaseValid(secondTSA.ID, "worker-second", time.Now().Unix()); err != nil || !valid {
		t.Fatalf("existing-proof TSA lease valid=%t err=%v", valid, err)
	}
}

func TestImportReportsPostCommitCancellation(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","lat":1,"lon":2,"tst":1}`)
	var raw map[string]any
	_ = json.Unmarshal(payload, &raw)
	can, _ := canonical.FromLocation("u", "d", raw)
	digest, _ := can.Digest()
	location, _ := json.Marshal(export.Record{
		ID: 1, User: "u", Device: "d", EventUnix: 1, Payload: payload,
		DigestSHA256: fmt.Sprintf("%x", digest), TsaStatus: "pending", OtsStatus: "pending",
	})
	data := makeZip(t,
		zipEntry{"manifest.json", `{"version":2}`},
		zipEntry{"locations.jsonl", string(location) + "\n"},
	)
	ctx, cancel := context.WithCancel(t.Context())
	archive, err := importzip.ValidateArchiveContext(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	st.SetChangeSink(cancelChangeSink{cancel: cancel})
	res, err := importzip.ImportValidatedContext(ctx, archive, st, filepath.Join(dir, "proofs"), noopStamper{})
	if !importzip.IsCommitted(err) || !res.Committed || res.Imported != 1 {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if count, countErr := st.Count(); countErr != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, countErr)
	}
}

func TestVerifyRejectsMalformedArchives(t *testing.T) {
	validManifest := `{"version":2}`
	cases := []struct {
		name    string
		entries []zipEntry
	}{
		{"no manifest", []zipEntry{{"locations.jsonl", ""}}},
		{"bad manifest", []zipEntry{{"manifest.json", "{"}}},
		{"future version", []zipEntry{{"manifest.json", `{"version":99}`}}},
		{"zero version", []zipEntry{{"manifest.json", `{}`}}},
		{"negative version", []zipEntry{{"manifest.json", `{"version":-1}`}}},
		{"no locations", []zipEntry{{"manifest.json", validManifest}}},
		{"duplicate locations", []zipEntry{
			{"manifest.json", validManifest}, {"locations.jsonl", ""}, {"locations.jsonl", ""},
		}},
		{"duplicate manifest", []zipEntry{
			{"manifest.json", validManifest}, {"manifest.json", validManifest}, {"locations.jsonl", ""},
		}},
		{"nested proof", []zipEntry{
			{"manifest.json", validManifest}, {"locations.jsonl", ""}, {"proofs/nested/1.ots", "x"},
		}},
		{"bad proof name", []zipEntry{
			{"manifest.json", validManifest}, {"locations.jsonl", ""}, {"proofs/nope", "x"},
		}},
		{"zero proof id", []zipEntry{
			{"manifest.json", validManifest}, {"locations.jsonl", ""}, {"proofs/0.ots", "x"},
		}},
		{"duplicate proof", []zipEntry{
			{"manifest.json", validManifest}, {"locations.jsonl", ""},
			{"proofs/1.ots", "x"}, {"proofs/1.ots", "x"},
		}},
		{"manifest count mismatch", []zipEntry{
			{"manifest.json", `{"version":4,"locations":1}`}, {"locations.jsonl", ""},
			{"verification_runs.jsonl", ""}, {"verification_attempts.jsonl", ""},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := makeZip(t, tc.entries...)
			if _, err := importzip.Verify(bytes.NewReader(data), int64(len(data))); err == nil {
				t.Fatal("archive accepted")
			}
		})
	}
}

func TestVerifyAcceptsHistoricalVerificationProofDigest(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","lat":1,"lon":2,"tst":1}`)
	var raw map[string]any
	_ = json.Unmarshal(payload, &raw)
	can, _ := canonical.FromLocation("u", "d", raw)
	digest, _ := can.Digest()
	location, _ := json.Marshal(export.Record{
		ID: 1, User: "u", Device: "d", EventUnix: 1, Payload: payload,
		DigestSHA256: fmt.Sprintf("%x", digest),
		Proofs:       export.ProofPaths{TSR: "proofs/1.tsr"},
	})
	oldRun, _ := json.Marshal(export.VerificationRunRecord{
		VerificationRun: store.VerificationRun{
			ID: 1, LocationID: 1, Kind: "tsa", Trigger: "manual",
			Result: "degraded", CheckedAt: 1,
		},
		ProofSHA256Hex: strings.Repeat("00", sha256.Size),
	})
	currentRun, _ := json.Marshal(export.VerificationRunRecord{
		VerificationRun: store.VerificationRun{
			ID: 2, LocationID: 1, Kind: "tsa", Trigger: "scheduled",
			Result: "verified", CheckedAt: 2,
		},
		ProofSHA256Hex: fmt.Sprintf("%x", sha256.Sum256([]byte("proof"))),
	})
	data := makeZip(t,
		zipEntry{"manifest.json", `{"version":3}`},
		zipEntry{"locations.jsonl", string(location) + "\n"},
		zipEntry{"verification_runs.jsonl", string(oldRun) + "\n" + string(currentRun) + "\n"},
		zipEntry{"verification_attempts.jsonl", ""},
		zipEntry{"proofs/1.tsr", "proof"},
	)
	if _, err := importzip.Verify(bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("historical verification proof rejected: %v", err)
	}
}

func TestImportAcceptsOutOfOrderVerificationRuns(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","lat":1,"lon":2,"tst":1}`)
	var raw map[string]any
	_ = json.Unmarshal(payload, &raw)
	can, _ := canonical.FromLocation("u", "d", raw)
	digest, _ := can.Digest()
	location, _ := json.Marshal(export.Record{
		ID: 1, User: "u", Device: "d", EventUnix: 1, Payload: payload,
		DigestSHA256: fmt.Sprintf("%x", digest),
	})
	run := func(id int64) string {
		trigger := "manual"
		if id == 2 {
			trigger = "scheduled"
		}
		b, _ := json.Marshal(export.VerificationRunRecord{VerificationRun: store.VerificationRun{
			ID: id, LocationID: 1, Kind: "tsa", Trigger: trigger, Result: "missing", CheckedAt: id,
		}})
		return string(b) + "\n"
	}
	data := makeZip(t,
		zipEntry{"manifest.json", `{"version":3}`},
		zipEntry{"locations.jsonl", string(location) + "\n"},
		zipEntry{"verification_runs.jsonl", run(2) + run(1)},
		zipEntry{"verification_attempts.jsonl", ""},
	)
	if _, err := importzip.Verify(bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	proofs := filepath.Join(dir, "proofs")
	if err := os.Mkdir(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := importzip.ImportBytes(data, st, proofs, nil); err != nil {
		t.Fatal(err)
	}
	latest, err := st.LatestVerification(1, "tsa")
	if err != nil || latest.Trigger != "import" || latest.EngineVersion != 0 || latest.CheckedAt != 2 {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
}

func TestVerifyRejectsVerificationProofDigestMismatch(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","lat":1,"lon":2,"tst":1}`)
	var raw map[string]any
	_ = json.Unmarshal(payload, &raw)
	can, _ := canonical.FromLocation("u", "d", raw)
	digest, _ := can.Digest()
	location, _ := json.Marshal(export.Record{ID: 1, User: "u", Device: "d", EventUnix: 1, Payload: payload, DigestSHA256: fmt.Sprintf("%x", digest), Proofs: export.ProofPaths{TSR: "proofs/1.tsr"}})
	run, _ := json.Marshal(export.VerificationRunRecord{VerificationRun: store.VerificationRun{ID: 1, LocationID: 1, Kind: "tsa", Trigger: "manual", Result: "degraded", CheckedAt: 1}, ProofSHA256Hex: strings.Repeat("00", sha256.Size)})
	data := makeZip(t, zipEntry{"manifest.json", `{"version":5,"locations":1,"verification_runs":1,"proof_sha256":{"proofs/1.tsr":"` + strings.Repeat("00", sha256.Size) + `"}}`}, zipEntry{"locations.jsonl", string(location) + "\n"}, zipEntry{"verification_runs.jsonl", string(run) + "\n"}, zipEntry{"verification_attempts.jsonl", ""}, zipEntry{"proofs/1.tsr", "proof"})
	if _, err := importzip.Verify(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("mismatched verification proof accepted")
	}
}

func TestVerifyAcceptsStaleVerificationHistoryWithCurrentProofDigest(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","lat":1,"lon":2,"tst":1}`)
	var raw map[string]any
	_ = json.Unmarshal(payload, &raw)
	can, _ := canonical.FromLocation("u", "d", raw)
	digest, _ := can.Digest()
	location, _ := json.Marshal(export.Record{
		ID: 1, User: "u", Device: "d", EventUnix: 1, Payload: payload,
		DigestSHA256: fmt.Sprintf("%x", digest), Proofs: export.ProofPaths{OTS: "proofs/1.ots"},
	})
	run, _ := json.Marshal(export.VerificationRunRecord{
		VerificationRun: store.VerificationRun{
			ID: 1, LocationID: 1, Kind: "ots", Trigger: "upgrade", Result: "pending", CheckedAt: 1,
		},
		ProofSHA256Hex: fmt.Sprintf("%x", sha256.Sum256([]byte("old-proof"))),
	})
	proof := []byte("current-proof")
	manifest := fmt.Sprintf(`{"version":5,"locations":1,"verification_runs":1,"proof_sha256":{"proofs/1.ots":"%x"}}`, sha256.Sum256(proof))
	data := makeZip(t,
		zipEntry{"manifest.json", manifest},
		zipEntry{"locations.jsonl", string(location) + "\n"},
		zipEntry{"verification_runs.jsonl", string(run) + "\n"},
		zipEntry{"verification_attempts.jsonl", ""},
		zipEntry{"proofs/1.ots", string(proof)},
	)
	if _, err := importzip.Verify(bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("stale verification history rejected: %v", err)
	}
}

func TestVerifyV5RequiresEveryProofDigest(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","lat":1,"lon":2,"tst":1}`)
	var raw map[string]any
	_ = json.Unmarshal(payload, &raw)
	can, _ := canonical.FromLocation("u", "d", raw)
	digest, _ := can.Digest()
	location, _ := json.Marshal(export.Record{
		ID: 1, User: "u", Device: "d", EventUnix: 1, Payload: payload,
		DigestSHA256: fmt.Sprintf("%x", digest), Proofs: export.ProofPaths{TSR: "proofs/1.tsr"},
	})
	data := makeZip(t,
		zipEntry{"manifest.json", `{"version":5,"locations":1}`},
		zipEntry{"locations.jsonl", string(location) + "\n"},
		zipEntry{"verification_runs.jsonl", ""},
		zipEntry{"verification_attempts.jsonl", ""},
		zipEntry{"proofs/1.tsr", "proof"},
	)
	if _, err := importzip.Verify(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("v5 proof without manifest digest accepted")
	}
}

func TestVerifyRejectsUnsafeVerificationMetadata(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","lat":1,"lon":2,"tst":1}`)
	var raw map[string]any
	_ = json.Unmarshal(payload, &raw)
	can, _ := canonical.FromLocation("u", "d", raw)
	digest, _ := can.Digest()
	location, _ := json.Marshal(export.Record{
		ID: 1, User: "u", Device: "d", EventUnix: 1, Payload: payload,
		DigestSHA256: fmt.Sprintf("%x", digest),
	})
	for _, tc := range []struct {
		name    string
		run     store.VerificationRun
		attempt store.VerificationAttempt
	}{
		{"future run", store.VerificationRun{ID: 1, LocationID: 1, Kind: "ots", Result: "verified", CheckedAt: time.Now().Add(time.Hour).Unix()}, store.VerificationAttempt{}},
		{"provider URL", store.VerificationRun{ID: 1, LocationID: 1, Kind: "ots", Result: "verified", CheckedAt: 1}, store.VerificationAttempt{ID: 1, RunID: 1, ProviderID: "esplora-https://secret.example", Result: "error"}},
		{"error URL", store.VerificationRun{ID: 1, LocationID: 1, Kind: "ots", Result: "error", ErrorCode: "https://secret.example", CheckedAt: 1}, store.VerificationAttempt{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, _ := json.Marshal(export.VerificationRunRecord{VerificationRun: tc.run})
			attempts := ""
			if tc.attempt.ID > 0 {
				attempt, _ := json.Marshal(tc.attempt)
				attempts = string(attempt) + "\n"
			}
			data := makeZip(t,
				zipEntry{"manifest.json", `{"version":3}`},
				zipEntry{"locations.jsonl", string(location) + "\n"},
				zipEntry{"verification_runs.jsonl", string(run) + "\n"},
				zipEntry{"verification_attempts.jsonl", attempts},
			)
			if _, err := importzip.Verify(bytes.NewReader(data), int64(len(data))); err == nil {
				t.Fatal("unsafe metadata accepted")
			}
		})
	}
}

func TestImportProofFailureRollsBackTransaction(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","lat":1,"lon":2,"tst":1}`)
	var raw map[string]any
	_ = json.Unmarshal(payload, &raw)
	can, _ := canonical.FromLocation("u", "d", raw)
	digest, _ := can.Digest()
	rec := export.Record{
		ID: 1, User: "u", Device: "d", EventUnix: 1, Payload: payload,
		DigestSHA256: fmt.Sprintf("%x", digest),
		TsaStatus:    "ok",
		Proofs:       export.ProofPaths{TSR: "proofs/1.tsr"},
	}
	line, _ := json.Marshal(rec)
	data := makeZip(t,
		zipEntry{"manifest.json", `{"version":2}`},
		zipEntry{"locations.jsonl", string(line) + "\n"},
		zipEntry{"proofs/1.tsr", "proof"},
	)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	proofs := filepath.Join(dir, "proofs")
	if err := os.WriteFile(proofs, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := importzip.ImportBytes(data, st, proofs, nil); err == nil {
		t.Fatal("import succeeded")
	}
	if n, err := st.Count(); err != nil || n != 0 {
		t.Fatalf("count=%d err=%v", n, err)
	}
}

func TestVerifyRejectsInvalidRecords(t *testing.T) {
	data := makeZip(t,
		zipEntry{"manifest.json", `{"version":2}`},
		zipEntry{"locations.jsonl", `{"digest_sha256":"bad","payload":{}}` + "\n"},
	)
	if _, err := importzip.Verify(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("invalid record accepted")
	}
}

func TestVerifyRejectsRecordWithMissingProof(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","lat":1,"lon":2,"tst":1}`)
	var raw map[string]any
	_ = json.Unmarshal(payload, &raw)
	can, _ := canonical.FromLocation("u", "d", raw)
	digest, _ := can.Digest()
	rec := export.Record{
		ID: 1, User: "u", Device: "d", EventUnix: 1, Payload: payload,
		DigestSHA256: fmt.Sprintf("%x", digest),
		Proofs:       export.ProofPaths{OTS: "proofs/1.ots"},
	}
	line, _ := json.Marshal(rec)
	data := makeZip(t,
		zipEntry{"manifest.json", `{"version":2}`},
		zipEntry{"locations.jsonl", string(line) + "\n"},
	)
	if _, err := importzip.Verify(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("missing proof accepted")
	}
}

func TestVerifyRejectsAmbiguousLocationReferences(t *testing.T) {
	record := func(id, tst int64) string {
		payload := json.RawMessage(fmt.Sprintf(`{"_type":"location","lat":1,"lon":2,"tst":%d}`, tst))
		var raw map[string]any
		_ = json.Unmarshal(payload, &raw)
		can, _ := canonical.FromLocation("u", "d", raw)
		digest, _ := can.Digest()
		line, _ := json.Marshal(export.Record{
			ID: id, User: "u", Device: "d", EventUnix: tst, Payload: payload,
			DigestSHA256: fmt.Sprintf("%x", digest),
		})
		return string(line) + "\n"
	}

	duplicate := makeZip(t,
		zipEntry{"manifest.json", `{"version":2}`},
		zipEntry{"locations.jsonl", record(1, 1) + record(1, 2)},
	)
	if _, err := importzip.Verify(bytes.NewReader(duplicate), int64(len(duplicate))); err == nil {
		t.Fatal("duplicate location id accepted")
	}

	run, _ := json.Marshal(export.VerificationRunRecord{VerificationRun: store.VerificationRun{
		ID: 1, LocationID: 2, Kind: "tsa", Trigger: "manual", Result: "degraded", CheckedAt: 1,
	}})
	unknown := makeZip(t,
		zipEntry{"manifest.json", `{"version":3}`},
		zipEntry{"locations.jsonl", record(1, 1)},
		zipEntry{"verification_runs.jsonl", string(run) + "\n"},
		zipEntry{"verification_attempts.jsonl", ""},
	)
	if _, err := importzip.Verify(bytes.NewReader(unknown), int64(len(unknown))); err == nil {
		t.Fatal("unknown verification location accepted")
	}
}

func TestAuditExitCodesAndText(t *testing.T) {
	for result, want := range map[string]int{"verified": 0, "degraded": 2, "invalid": 1} {
		report := importzip.AuditReport{
			ArchiveVersion: 4, Overall: result,
			Locations: []importzip.AuditLocation{{
				ID: 1, Digest: strings.Repeat("0", 64),
				TSA: importzip.AuditProof{Result: result},
				OTS: importzip.AuditProof{Result: result},
			}},
		}
		if got := importzip.AuditExitCode(report); got != want {
			t.Fatalf("%s exit=%d", result, got)
		}
		var text bytes.Buffer
		if err := importzip.WriteAuditText(&text, report); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text.String(), "archive v4") || strings.Contains(text.String(), "latitude") {
			t.Fatalf("text=%q", text.String())
		}
	}
}

func TestContextCancellationStopsValidation(t *testing.T) {
	data := makeZip(t,
		zipEntry{"manifest.json", `{"version":2}`},
		zipEntry{"locations.jsonl", ""},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := importzip.VerifyContext(ctx, bytes.NewReader(data), int64(len(data))); !errors.Is(err, context.Canceled) {
		t.Fatalf("verify error=%v", err)
	}
	if _, err := importzip.ImportBytesContext(ctx, data, nil, t.TempDir(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("import error=%v", err)
	}
}

func TestVerifyRejectsUnreferencedProofEntries(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range []zipEntry{
		{"manifest.json", `{"version":2}`},
		{"locations.jsonl", ""},
	} {
		w, err := zw.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 2; i++ {
		w, err := zw.Create(fmt.Sprintf("proofs/%d.tsr", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := importzip.Verify(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err == nil || !strings.Contains(err.Error(), "unreferenced proofs") {
		t.Fatalf("unreferenced proofs error=%v", err)
	}
}
