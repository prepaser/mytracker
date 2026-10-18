package importzip

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/export"
)

type cancelingReader struct {
	cancel context.CancelFunc
	done   bool
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

type failAfterWriter struct {
	writes int
	err    error
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, w.err
	}
	return len(p), nil
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func (r *cancelingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	p[0] = 1
	r.cancel()
	return 1, nil
}

func TestCopyContextStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelingReader{cancel: cancel}
	if err := copyContext(ctx, io.Discard, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func TestContextHelpers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (contextReader{ctx: ctx, r: bytes.NewReader(nil)}).Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("reader error=%v", err)
	}
	if _, err := stageProofContext(ctx, t.TempDir(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("stage error=%v", err)
	}
	if _, err := readFilePathContext(ctx, filepath.Join(t.TempDir(), "missing")); !errors.Is(err, context.Canceled) {
		t.Fatalf("file error=%v", err)
	}
}

func TestContextIOErrors(t *testing.T) {
	want := errors.New("I/O failed")
	if err := copyContext(context.Background(), io.Discard, errorReader{want}); !errors.Is(err, want) {
		t.Fatalf("read error=%v", err)
	}
	if err := copyContext(context.Background(), errorWriter{want}, bytes.NewReader([]byte("x"))); !errors.Is(err, want) {
		t.Fatalf("write error=%v", err)
	}
	if err := copyContext(context.Background(), shortWriter{}, bytes.NewReader([]byte("x"))); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error=%v", err)
	}
	if n, err := (contextReader{ctx: context.Background(), r: bytes.NewReader([]byte("ok"))}).Read(make([]byte, 2)); err != nil || n != 2 {
		t.Fatalf("read n=%d err=%v", n, err)
	}
}

func TestCanceledArchiveHelpers(t *testing.T) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(`{"version":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checks := []func() error{
		func() error { _, err := validateContext(ctx, zr); return err },
		func() error {
			return eachRecordContext(ctx, archiveData{jsonl: zr.File[0]}, func(export.Record) error { return nil })
		},
		func() error { return eachJSONLContext(ctx, zr.File[0], func(*json.Decoder) error { return nil }) },
		func() error { _, err := readFileContext(ctx, zr.File[0], 1024); return err },
		func() error { _, err := proofSHA256Context(ctx, zr.File[0]); return err },
		func() error { _, err := checkManifestContext(ctx, zr); return err },
	}
	for i, check := range checks {
		if err := check(); !errors.Is(err, context.Canceled) {
			t.Fatalf("check %d error=%v", i, err)
		}
	}
}

func TestValidationKeepsOnlyProofReferenceMetadata(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","tst":100,"lat":1,"lon":2}`)
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	can, err := canonical.FromLocation("u", "d", raw)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := can.Digest()
	if err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(export.Record{
		ID: 1, User: "u", Device: "d", EventUnix: 100,
		DigestSHA256: fmt.Sprintf("%x", digest), Payload: payload,
		TsaStatus: "pending", OtsStatus: "pending", Proofs: export.ProofPaths{TSR: "proofs/1.tsr"},
	})
	if err != nil {
		t.Fatal(err)
	}
	archive := makeArchive(t, map[string][]byte{
		"manifest.json":   []byte(`{"version":2}`),
		"locations.jsonl": append(record, '\n'),
		"proofs/1.tsr":    []byte("proof"),
	})
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	data, err := validateContext(context.Background(), zr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = data.index.Close() }()
	found, err := data.index.hasLocation(context.Background(), 1)
	if err != nil || !found {
		t.Fatal("location missing from archive index")
	}
}

func TestV6EntryManifestRejectsTampering(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","tst":100,"lat":1,"lon":2}`)
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	can, _ := canonical.FromLocation("u", "d", raw)
	digest, _ := can.Digest()
	stamp := time.Unix(100, 0).UTC().Format(time.RFC3339)
	record, _ := json.Marshal(export.Record{
		ID: 1, User: "u", Device: "d", EventUnix: 100, EventTimeUTC: stamp,
		Latitude: 1, Longitude: 2, ReceivedUTC: stamp, TsaStatus: "pending", OtsStatus: "pending",
		DigestSHA256: fmt.Sprintf("%x", digest), Payload: payload,
	})
	entries := map[string][]byte{
		"README.txt":                  []byte("readme"),
		"locations.jsonl":             append(record, '\n'),
		"locations.csv":               []byte("csv"),
		"verification_runs.jsonl":     {},
		"verification_attempts.jsonl": {},
	}
	manifest := export.Manifest{
		Version: 6, Locations: 1, EntrySHA256: make(map[string]string, len(entries)),
	}
	for name, content := range entries {
		sum := sha256.Sum256(content)
		manifest.EntrySHA256[name] = fmt.Sprintf("%x", sum)
	}
	manifestJSON, _ := json.Marshal(manifest)
	entries["manifest.json"] = manifestJSON
	valid := makeArchive(t, entries)
	if _, err := Verify(bytes.NewReader(valid), int64(len(valid))); err != nil {
		t.Fatalf("valid v6 archive rejected: %v", err)
	}
	entries["README.txt"] = []byte("tampered")
	tampered := makeArchive(t, entries)
	if _, err := Verify(bytes.NewReader(tampered), int64(len(tampered))); err == nil {
		t.Fatal("tampered non-proof entry accepted")
	}
}

func TestValidationRejectsOrphanProof(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","tst":100,"lat":1,"lon":2}`)
	var raw map[string]any
	_ = json.Unmarshal(payload, &raw)
	can, _ := canonical.FromLocation("u", "d", raw)
	digest, _ := can.Digest()
	record, _ := json.Marshal(export.Record{
		ID: 1, User: "u", Device: "d", EventUnix: 100,
		DigestSHA256: fmt.Sprintf("%x", digest), Payload: payload,
	})
	archive := makeArchive(t, map[string][]byte{
		"manifest.json":   []byte(`{"version":2}`),
		"locations.jsonl": append(record, '\n'),
		"proofs/1.tsr":    []byte("orphan"),
	})
	if _, err := Verify(bytes.NewReader(archive), int64(len(archive))); err == nil {
		t.Fatal("orphan proof accepted")
	}
}

func TestCommittedErrorIsClassified(t *testing.T) {
	cause := errors.New("cleanup failed")
	err := &CommittedError{Err: cause}
	if !IsCommitted(err) || !errors.Is(err, cause) {
		t.Fatalf("committed=%t err=%v", IsCommitted(err), err)
	}
	if !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("error=%q", err)
	}
}

func TestWriteAuditTextPropagatesWriterError(t *testing.T) {
	err := errors.New("write failed")
	if got := WriteAuditText(errorWriter{err: err}, AuditReport{}); !errors.Is(got, err) {
		t.Fatalf("error=%v", got)
	}
	w := &failAfterWriter{err: err}
	if got := WriteAuditText(w, AuditReport{Locations: []AuditLocation{{ID: 1}}}); !errors.Is(got, err) {
		t.Fatalf("location error=%v", got)
	}
}

func makeArchive(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestVerificationResultPrecedence(t *testing.T) {
	for _, test := range []struct {
		results []string
		want    string
	}{
		{[]string{"verified", "verified"}, "verified"},
		{[]string{"verified", "pending"}, "degraded"},
		{[]string{"degraded", "invalid"}, "invalid"},
		{[]string{"missing"}, "invalid"},
	} {
		if got := auditOverall(test.results...); got != test.want {
			t.Fatalf("auditOverall(%v)=%q, want %q", test.results, got, test.want)
		}
	}
	for _, test := range []struct{ current, next, want string }{
		{"verified", "verified", "verified"},
		{"verified", "degraded", "degraded"},
		{"degraded", "verified", "degraded"},
		{"verified", "invalid", "invalid"},
		{"invalid", "verified", "invalid"},
	} {
		if got := mergeAuditResult(test.current, test.next); got != test.want {
			t.Fatalf("mergeAuditResult(%q, %q)=%q, want %q", test.current, test.next, got, test.want)
		}
	}
}

func TestNeedsStamp(t *testing.T) {
	for _, test := range []struct {
		tsa, ots string
		want     bool
	}{
		{"ok", "confirmed", false},
		{"pending", "confirmed", true},
		{"ok", "fail", true},
	} {
		if got := needsStamp(test.tsa, test.ots); got != test.want {
			t.Fatalf("needsStamp(%q, %q)=%t, want %t", test.tsa, test.ots, got, test.want)
		}
	}
}
