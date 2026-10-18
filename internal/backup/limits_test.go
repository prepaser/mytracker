package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/atomicfile"
)

func TestCheckFileRejectsOversizedDeclaredProof(t *testing.T) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.Create("proofs/1.tsr")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	central := bytes.Index(archive.Bytes(), []byte{0x50, 0x4b, 0x01, 0x02})
	if central < 0 {
		t.Fatal("central directory not found")
	}
	binary.LittleEndian.PutUint32(archive.Bytes()[central+24:central+28], uint32(maxBackupProofSize+1))
	path := filepath.Join(t.TempDir(), "oversized.mtbackup")
	if err := os.WriteFile(path, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckFile(path); err == nil || !strings.Contains(err.Error(), "invalid backup entry") {
		t.Fatalf("oversized declared proof err=%v", err)
	}
}

func TestCopyBoundedRejectsOversizedStream(t *testing.T) {
	var dst bytes.Buffer
	n, err := copyBounded(&dst, bytes.NewReader([]byte("12345")), 4)
	if err == nil {
		t.Fatal("oversized stream accepted")
	}
	if n != 5 {
		t.Fatalf("copied bytes=%d, want 5 bytes observed before rejection", n)
	}
}

func TestCopyBoundedContextStopsCanceledStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := copyBoundedContext(ctx, &bytes.Buffer{}, bytes.NewReader([]byte("data")), 4)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}

func TestValidateBackupSourcesRejectsOversizedProofBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "snapshot.db")
	proofs := filepath.Join(dir, "proofs")
	if err := os.Mkdir(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, []byte("db"), 0600); err != nil {
		t.Fatal(err)
	}
	proof := filepath.Join(proofs, "1.tsr")
	if err := os.WriteFile(proof, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(proof, maxBackupProofSize+1); err != nil {
		t.Fatal(err)
	}
	total := int64(0)
	if err := validateSource(context.Background(), proof, "proofs/1.tsr", &total); err == nil {
		t.Fatal("oversized proof source accepted")
	}
}

func TestBackupLimitHelpers(t *testing.T) {
	if _, err := copyBounded(io.Discard, bytes.NewReader(nil), -1); err == nil {
		t.Fatal("negative copy limit accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := validateSourceSnapshot(ctx, "missing", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled source validation err=%v", err)
	}
	if _, err := VerifyFileContext(ctx, "missing"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled verification err=%v", err)
	}
	if err := validateSourceSnapshot(context.Background(), t.TempDir(), ""); err == nil {
		t.Fatal("directory accepted as database source")
	}
	if err := Scavenge(nil); err == nil {
		t.Fatal("nil config accepted")
	}
}

func TestBackupHelperCancellationAndPruning(t *testing.T) {
	dir := t.TempDir()
	var copied bytes.Buffer
	if n, err := copyContext(context.TODO(), &copied, bytes.NewReader([]byte("ok"))); err != nil || n != 2 {
		t.Fatalf("copied=%d err=%v", n, err)
	}
	if err := atomicfile.SyncDir(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing directory synced")
	}
	if err := prune(filepath.Join(dir, "missing"), 1); err == nil {
		t.Fatal("missing backup directory pruned")
	}
	for _, name := range []string{"mytracker-1.db", "mytracker-2.mtbackup", "ignore.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "mytracker-dir.mtbackup"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := prune(dir, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mytracker-2.mtbackup")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mytracker-1.db")); !os.IsNotExist(err) {
		t.Fatalf("old backup remains: %v", err)
	}
}

func TestRestorePreflightRejectsOversizedSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.mtbackup")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxBackupUncompressedSize + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := preflightRestoreSource(f, t.TempDir()); err == nil {
		t.Fatal("oversized restore source accepted")
	}
	_ = f.Close()
}
