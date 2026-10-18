package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProofReplacementCleanupErrorsPreserveArtifacts(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, ".1.tsr.old-pending")
	if err := os.WriteFile(artifact, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tt := range []struct {
		ctx     context.Context
		path    string
		wantErr bool
	}{
		{canceled, dir, true},
		{t.Context(), artifact, true},
		{t.Context(), filepath.Join(dir, "absent"), false},
	} {
		err := removeProofReplacementArtifactsContext(tt.ctx, tt.path, map[string]struct{}{"1.tsr": {}})
		if (err != nil) != tt.wantErr || tt.ctx.Err() != nil && !errors.Is(err, tt.ctx.Err()) {
			t.Fatalf("cleanup err=%v want error=%t", err, tt.wantErr)
		}
	}
	if data, err := os.ReadFile(artifact); err != nil || string(data) != "old" {
		t.Fatalf("artifact changed: %q err=%v", data, err)
	}
}

func TestRecoverProofReplacementBeforeCompletionRestoresOldProof(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mytracker.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimStampJob(time.Now().Unix(), "crashed", time.Now().Add(time.Minute).Unix())
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	proofs := filepath.Join(dir, "proofs")
	writeProofArtifact(t, proofs, id, ".tsr", "old", "new", true)

	st, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	assertProofCleanup(t, proofs, id, ".tsr", "old")
}

func TestRecoverProofReplacementBeforeRenameKeepsLiveProof(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mytracker.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimStampJob(time.Now().Unix(), "crashed", time.Now().Add(time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	proofs := filepath.Join(dir, "proofs")
	writeProofArtifact(t, proofs, id, ".ots", "old", "old", true)

	st, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	assertProofCleanup(t, proofs, id, ".ots", "old")
}

func TestRecoverProofReplacementAfterCompletionKeepsLiveProof(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mytracker.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimStampJob(time.Now().Unix(), "done", time.Now().Add(time.Minute).Unix())
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	ts := time.Now().Unix()
	if err := st.CompleteStampJob(job, "ok", &ts); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	proofs := filepath.Join(dir, "proofs")
	writeProofArtifact(t, proofs, id, ".tsr", "old", "new", false)

	st, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	assertProofCleanup(t, proofs, id, ".tsr", "new")
}

func TestRecoverProofReplacementWithoutOldProofKeepsLiveProof(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mytracker.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	proofs := filepath.Join(dir, "proofs")
	if err := os.MkdirAll(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proofs, proofName(id, ".ots")), []byte("live"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proofs, "."+proofName(id, ".ots")+".new-crash"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}

	st, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	assertProofCleanup(t, proofs, id, ".ots", "live")
}

func TestRecoverProofReplacementIgnoresEmptyOldArtifact(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mytracker.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	proofs := filepath.Join(dir, "proofs")
	if err := os.MkdirAll(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proofs, proofName(id, ".tsr")), []byte("live"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proofs, "."+proofName(id, ".tsr")+".old-crash"), nil, 0600); err != nil {
		t.Fatal(err)
	}

	st, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	assertProofCleanup(t, proofs, id, ".tsr", "live")
}

func TestAuditProofsReportsReplacementArtifacts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mytracker.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	proofs := filepath.Join(dir, "proofs")
	if err := os.MkdirAll(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proofs, ".12.tsr.old-crash"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	audit, err := st.AuditProofs(proofs)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.Orphan) != 1 || audit.Orphan[0] != ".12.tsr.old-crash" {
		t.Fatalf("orphan=%v", audit.Orphan)
	}
}

func TestProofGCRemovesReplacementArtifactsAfterLocationDeletion(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	proofs := filepath.Join(dir, "proofs")
	writeProofArtifact(t, proofs, id, ".tsr", "old", "live", true)
	if _, err := st.DeleteRange(proofs, ExportFilter{From: 1}); err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{filepath.Join(proofs, proofName(id, ".tsr")), filepath.Join(proofs, "."+proofName(id, ".tsr")+".*")} {
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) != 0 {
			t.Fatalf("remaining proof files=%v err=%v", matches, err)
		}
	}
}

func writeProofArtifact(t *testing.T, dir string, id int64, ext, old, live string, running bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, proofName(id, ext)), []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "."+proofName(id, ext)+".old-crash"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	if running {
		if err := os.WriteFile(filepath.Join(dir, "."+proofName(id, ext)+".new-crash"), []byte("new"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertProofCleanup(t *testing.T, dir string, id int64, ext, want string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, proofName(id, ext)))
	if err != nil || string(b) != want {
		t.Fatalf("live=%q err=%v want=%q", b, err, want)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "."+proofName(id, ext)+".*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("artifacts=%v err=%v", matches, err)
	}
}

func proofName(id int64, ext string) string {
	return fmt.Sprintf("%d%s", id, ext)
}
