package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/store"
)

func TestProofSnapshotBlocksMutation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	unlockSnapshot := st.LockProofSnapshot()
	done := make(chan struct{})
	go func() {
		unlockMutation := st.LockProofMutation()
		unlockMutation()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("proof mutation bypassed snapshot lock")
	case <-time.After(20 * time.Millisecond):
	}
	unlockSnapshot()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("proof mutation did not resume")
	}
}

func TestProofLockHonorsCanceledContext(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	unlock := st.LockProofMutation()
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.LockProofSnapshotContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}

func TestProofLockCoordinatesStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	first, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	unlock := first.LockProofSnapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if release, err := second.LockProofMutationContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		unlock()
		t.Fatalf("mutation bypassed another store's snapshot: %v", err)
	}
	unlock()
	release, err := second.LockProofMutationContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestBackupLockCoordinatesStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	first, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	unlock, err := first.LockBackupContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if release, err := second.LockBackupContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("backup bypassed active maintenance: %v", err)
	}
	unlock()
	release, err := second.LockBackupContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestBackupSnapshotLockCoordinatesStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	first, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	read, err := second.LockBackupSnapshotContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer read()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if unlock, err := first.LockBackupContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("backup bypassed snapshot: %v", err)
	}
	read()
	write, err := first.LockBackupContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer write()
	ctx, cancelRead := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelRead()
	if unlock, err := second.LockBackupSnapshotContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("snapshot bypassed backup: %v", err)
	}
}

func TestLegacyBackupSnapshotBlocksStoreInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(filepath.Dir(path), ".mytracker-backups.lock")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	reader, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	unlock, err := reader.LockBackupSnapshotContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("read-only inspection created backup lock: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		writer, err := store.Open(path)
		if err == nil {
			err = writer.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("writable store bypassed legacy snapshot: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("store initialization did not resume")
	}
}
