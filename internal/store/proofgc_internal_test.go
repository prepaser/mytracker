package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestProofGCSyncFailureLeavesQueueRetryable(t *testing.T) {
	root := t.TempDir()
	proofs := filepath.Join(root, "proofs")
	if err := os.MkdirAll(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(root, "tracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.ImportLocation(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proofs, fmt.Sprintf("%d.tsr", id))
	if err := os.WriteFile(path, []byte("proof"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := st.db.Exec(`INSERT INTO proof_gc(path,created_at,updated_at,next_attempt_at) VALUES(?,?,?,?)`,
		filepath.Base(path), now, now, now); err != nil {
		t.Fatal(err)
	}
	original := proofGCSyncDir
	proofGCSyncDir = func(string) error { return errors.New("sync failed") }
	n, err := st.ProcessProofGC(proofs, 10)
	proofGCSyncDir = original
	if n != 0 || err == nil {
		t.Fatalf("processed=%d err=%v", n, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("proof stat error=%v", err)
	}
	var pending int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM proof_gc WHERE path=?`, filepath.Base(path)).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending=%d", pending)
	}
	if n, err := st.ProcessProofGC(proofs, 10); err != nil || n != 1 {
		t.Fatalf("retry processed=%d err=%v", n, err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM proof_gc WHERE path=?`, filepath.Base(path)).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("pending after retry=%d", pending)
	}
}

func TestProofGCSyncsDirectoryOncePerBatch(t *testing.T) {
	root := t.TempDir()
	proofs := filepath.Join(root, "proofs")
	if err := os.MkdirAll(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(root, "tracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now().Unix()
	for _, name := range []string{"1.tsr", "2.ots"} {
		if err := os.WriteFile(filepath.Join(proofs, name), []byte("proof"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.Exec(`INSERT INTO proof_gc(path,created_at,updated_at,next_attempt_at) VALUES(?,?,?,?)`,
			name, now, now, now); err != nil {
			t.Fatal(err)
		}
	}
	original := proofGCSyncDir
	defer func() { proofGCSyncDir = original }()
	var calls atomic.Int32
	proofGCSyncDir = func(string) error {
		calls.Add(1)
		return nil
	}
	if n, err := st.ProcessProofGC(proofs, 10); err != nil || n != 2 {
		t.Fatalf("processed=%d err=%v", n, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("directory syncs=%d", calls.Load())
	}
}

func TestProofGCCancellationAfterSyncKeepsQueueRetryable(t *testing.T) {
	root := t.TempDir()
	proofs := filepath.Join(root, "proofs")
	if err := os.Mkdir(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(root, "tracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := os.WriteFile(filepath.Join(proofs, "1.tsr"), []byte("proof"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO proof_gc(path,created_at,updated_at) VALUES('1.tsr',1,1)`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	original := proofGCSyncDir
	defer func() { proofGCSyncDir = original }()
	proofGCSyncDir = func(dir string) error {
		err := original(dir)
		cancel()
		return err
	}
	if n, err := st.ProcessProofGCContext(ctx, proofs, 10); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup=%d err=%v", n, err)
	}
	if status, err := st.ProofGCStatus(); err != nil || status.Pending != 1 {
		t.Fatalf("pending=%+v err=%v", status, err)
	}
	proofGCSyncDir = original
	if n, err := st.ProcessProofGC(proofs, 10); n != 1 || err != nil {
		t.Fatalf("resumed cleanup=%d err=%v", n, err)
	}
}

func TestProofGCBatchRemovesReplacementArtifacts(t *testing.T) {
	root := t.TempDir()
	proofs := filepath.Join(root, "proofs")
	if err := os.MkdirAll(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(root, "tracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now().Unix()
	var ids []int64
	for i := range 2 {
		id, err := st.Insert(&Location{User: "u", Device: "d", Tst: int64(i + 1), Payload: []byte(`{}`), Digest: []byte{byte(i + 1)}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		base := proofName(id, ".tsr")
		for _, suffix := range []string{"", ".old-batch", ".new-batch"} {
			name := base
			if suffix != "" {
				name = "." + base + suffix
			}
			if err := os.WriteFile(filepath.Join(proofs, name), []byte("proof"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.db.Exec(`INSERT INTO proof_gc(path,created_at,updated_at,next_attempt_at) VALUES(?,?,?,?)`, base, now, now, now); err != nil {
			t.Fatal(err)
		}
	}
	for i := range proofReplacementReadDirBatchSize * 2 {
		if err := os.WriteFile(filepath.Join(proofs, fmt.Sprintf("noise-%d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.ProcessProofGCContext(context.Background(), proofs, 10); err != nil || n != 2 {
		t.Fatalf("processed=%d err=%v", n, err)
	}
	for _, id := range ids {
		for _, suffix := range []string{"", ".old-batch", ".new-batch"} {
			name := proofName(id, ".tsr")
			if suffix != "" {
				name = "." + name + suffix
			}
			if _, err := os.Stat(filepath.Join(proofs, name)); !os.IsNotExist(err) {
				t.Fatalf("remaining artifact %q err=%v", name, err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(proofs, "noise-0")); err != nil {
		t.Fatalf("noise file removed: %v", err)
	}
}

func TestProofGCDeferredFailureIsNotReady(t *testing.T) {
	root := t.TempDir()
	proofs := filepath.Join(root, "proofs")
	if err := os.MkdirAll(filepath.Join(proofs, "1.tsr"), 0700); err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(root, "tracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now().Unix()
	if _, err := st.db.Exec(`INSERT INTO proof_gc(path,created_at,updated_at,next_attempt_at) VALUES(?,?,?,?)`,
		"1.tsr", now, now, now); err != nil {
		t.Fatal(err)
	}
	if n, err := st.ProcessProofGC(proofs, 10); n != 0 || err != nil {
		t.Fatalf("processed=%d err=%v", n, err)
	}
	if ready, err := st.ProofGCReadyCountContext(context.Background(), now); err != nil || ready != 0 {
		t.Fatalf("ready=%d err=%v", ready, err)
	}
	status, err := st.ProofGCStatus()
	if err != nil || status.Pending != 1 || status.Failed != 1 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}
