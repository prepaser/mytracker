package export

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/prepaser/mytracker/internal/store"
	"golang.org/x/sys/unix"
)

func TestDigestIndexTransactionRoundTrip(t *testing.T) {
	idx := testIndex(t)
	must(t, idx.Begin(context.Background()))
	must(t, idx.Put(context.Background(), "a", "1111"))
	idx.Rollback()
	if ok, err := idx.Has(context.Background(), "a"); err != nil || ok {
		t.Fatalf("rolled back row present=%v err=%v", ok, err)
	}
	must(t, idx.Begin(context.Background()))
	must(t, idx.Put(context.Background(), "committed", "3333"))
	must(t, idx.Commit())
	if ok, err := idx.Has(context.Background(), "committed"); err != nil || !ok {
		t.Fatalf("committed row present=%v err=%v", ok, err)
	}
}

func TestDigestIndexContextAndDatabaseErrors(t *testing.T) {
	idx := testIndex(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	wantErr(t, idx.Begin(canceled), context.Canceled)
	wantErr(t, idx.Put(canceled, "x", "y"), context.Canceled)
	if ok, err := idx.Has(canceled, "x"); ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("has ok=%v err=%v", ok, err)
	}
	if rows, err := idx.Rows(canceled); rows != nil {
		t.Fatalf("rows=%v", rows)
	} else {
		wantErr(t, err, context.Canceled)
	}
	if err := idx.db.Close(); err != nil {
		t.Fatal(err)
	}
	wantFailure(t, idx.Begin(context.Background()))
	wantFailure(t, idx.Put(context.Background(), "x", "y"))
	if ok, err := idx.Has(context.Background(), "x"); ok || err == nil {
		t.Fatalf("closed has ok=%v err=%v", ok, err)
	}
	if rows, err := idx.Rows(context.Background()); rows != nil || err == nil {
		t.Fatalf("closed rows=%v err=%v", rows, err)
	}
}

func TestDigestIndexAndWorkTempFailures(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "not-a-directory")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if idx, err := newDigestIndex(context.Background(), file); idx != nil || err == nil {
		t.Fatalf("file directory idx=%v err=%v", idx, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if idx, err := newDigestIndex(canceled, tmp); idx != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled index idx=%v err=%v", idx, err)
	}
	if matches, err := filepath.Glob(filepath.Join(tmp, ".mytracker-export-index-*.db")); err != nil || len(matches) != 0 {
		t.Fatalf("index files remain: %v err=%v", matches, err)
	}
	t.Setenv("TMPDIR", filepath.Join(tmp, "missing"))
	if work, err := newExportWork(); work != nil || err == nil {
		t.Fatalf("work=%v err=%v", work, err)
	}
}

func TestExportWorkLockAndCleanup(t *testing.T) {
	work, err := newExportWork()
	if err != nil {
		t.Fatal(err)
	}
	dir := work.dir
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Fatal("active work lock was available")
	}
	_ = lock.Close()
	if err := os.WriteFile(filepath.Join(dir, ".archive-test"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := work.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workdir still exists: %v", err)
	}
}

func TestWriteZipCancellationCleansWorkdir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	st, err := store.Open(filepath.Join(tmp, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	dst := &cancelWriter{cancel: cancel}
	if err := WriteZipContext(ctx, dst, Options{Store: st}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if dst.buf.Len() == 0 {
		t.Fatal("cancellation writer saw no archive bytes")
	}
	if matches, err := filepath.Glob(filepath.Join(tmp, ".mytracker-export-work-*")); err != nil || len(matches) != 0 {
		t.Fatalf("workdirs remain: %v err=%v", matches, err)
	}
}

type cancelWriter struct {
	cancel context.CancelFunc
	wrote  bool
	buf    bytes.Buffer
}

func (w *cancelWriter) Write(p []byte) (int, error) {
	if w.wrote {
		return 0, context.Canceled
	}
	w.wrote = true
	n, err := w.buf.Write(p)
	w.cancel()
	return n, err
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v want=%v", err, want)
	}
}

func wantFailure(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("operation unexpectedly succeeded")
	}
}

func testIndex(t *testing.T) *digestIndex {
	t.Helper()
	idx, err := newDigestIndex(context.Background(), t.TempDir())
	must(t, err)
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}
