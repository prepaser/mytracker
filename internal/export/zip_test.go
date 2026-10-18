package export_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/export"
	"github.com/prepaser/mytracker/internal/importzip"
	"github.com/prepaser/mytracker/internal/store"
)

func TestWriteZip(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"_type":"location","lat":37.5665,"lon":126.978,"tst":1751800000}`)
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	record, err := canonical.FromLocation("a", "p", raw)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := record.Digest()
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.ImportLocation(&store.Location{
		User: "a", Device: "p", Tst: 1751800000, Lat: 37.5665, Lon: 126.978,
		Payload: payload, Digest: digest[:], TSANonce: []byte{5, 6, 7, 8},
		TsaStatus: "ok", OtsStatus: "pending",
	})
	if err != nil {
		t.Fatal(err)
	}
	proofs := filepath.Join(dir, "proofs")
	if err := os.MkdirAll(proofs, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proofs, "1.tsr"), []byte("tsr"), 0644); err != nil {
		t.Fatal(err)
	}
	proofHash := sha256.Sum256([]byte("tsr"))
	oldProofHash := sha256.Sum256([]byte("old-tsr"))
	if err := st.SaveVerification(&store.VerificationRun{
		LocationID: 1, Kind: "tsa", Trigger: "manual", Result: "degraded",
		ProofSHA256: oldProofHash[:], CheckedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveVerification(&store.VerificationRun{
		LocationID: 1, Kind: "tsa", Trigger: "manual", Result: "degraded",
		ProofSHA256: proofHash[:], CheckedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	counted := -1
	if err := export.WriteZip(&buf, export.Options{
		Store: st, ProofsDir: proofs, OnCount: func(n int) { counted = n },
	}); err != nil {
		t.Fatal(err)
	}
	if counted != 1 {
		t.Fatalf("count callback=%d", counted)
	}
	if _, err := importzip.Verify(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		t.Fatalf("generated export failed verification: %v", err)
	}
	_ = st.Close()
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{
		"manifest.json", "README.txt", "locations.jsonl", "locations.csv",
		"verification_runs.jsonl", "verification_attempts.jsonl", "proofs/1.tsr",
	} {
		if !names[want] {
			t.Fatalf("missing %s in %v", want, names)
		}
	}
	if names["config.toml"] || names["mytracker.db"] {
		t.Fatal("should not include config or db")
	}

	rc, _ := zr.Open("locations.jsonl")
	var rec export.Record
	if err := json.NewDecoder(rc).Decode(&rec); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if rec.Latitude != 37.5665 || rec.Proofs.TSR != "proofs/1.tsr" ||
		rec.TSANonceHex != "05060708" {
		t.Fatalf("record: %+v", rec)
	}
	rc, _ = zr.Open("manifest.json")
	var manifest export.Manifest
	if err := json.NewDecoder(rc).Decode(&manifest); err != nil || manifest.Version != 6 ||
		manifest.VerificationRuns != 2 || manifest.VerificationAttempts != 0 {
		t.Fatalf("manifest=%+v err=%v", manifest, err)
	}
	if manifest.EntrySHA256["proofs/1.tsr"] != fmt.Sprintf("%x", proofHash) || len(manifest.EntrySHA256) != 6 {
		t.Fatalf("entry digests=%v", manifest.EntrySHA256)
	}
	_ = rc.Close()

	rc, _ = zr.Open("locations.csv")
	body := make([]byte, 512)
	n, _ := rc.Read(body)
	_ = rc.Close()
	if !strings.Contains(string(body[:n]), "digest_sha256") {
		t.Fatalf("csv: %s", body[:n])
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestWriteZipEmptyAndWriterFailure(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "tmp?#")
	if err := os.Mkdir(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	st, err := store.Open(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var buf bytes.Buffer
	counted := -1
	if err := export.WriteZip(&buf, export.Options{Store: st, OnCount: func(n int) { counted = n }}); err != nil {
		t.Fatal(err)
	}
	if counted != 0 {
		t.Fatalf("count callback=%d", counted)
	}
	if err := export.WriteZip(failingWriter{}, export.Options{Store: st}); err == nil {
		t.Fatal("writer failure ignored")
	}
	if files, err := os.ReadDir(tmp); err != nil || len(files) != 0 {
		t.Fatalf("temporary export artifacts remain: %v, %v", files, err)
	}
}

func TestWriteZipScopesProofs(t *testing.T) {
	if name := export.Filename(time.Unix(0, 0)); !strings.HasPrefix(name, "mytracker-export-") ||
		!strings.HasSuffix(name, ".zip") {
		t.Fatalf("filename=%q", name)
	}
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.ImportLocation(&store.Location{
		User: "a", Device: "p", Tst: 1, Lat: 1, Lon: 1,
		Payload: []byte(`{}`), Digest: []byte{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	proofs := filepath.Join(dir, "proofs")
	if err := os.MkdirAll(proofs, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"1.tsr", "2.tsr"} {
		if err := os.WriteFile(filepath.Join(proofs, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := export.WriteZip(&buf, export.Options{Store: st, ProofsDir: proofs}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["proofs/1.tsr"] || names["proofs/2.tsr"] {
		t.Fatalf("proof scope: %v", names)
	}
}

func TestWriteZipContextStopsBeforeWriting(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	if err := export.WriteZipContext(ctx, &buf, export.Options{Store: st}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("canceled export wrote %d bytes", buf.Len())
	}
}
