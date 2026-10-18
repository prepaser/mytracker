package web

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
)

func TestBackupStateCacheRecoversFromTransientTempFailure(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	path, err := backup.Create(cfg, st)
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	canceled := &Server{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if state := canceled.verifyBackupStateContext(ctx, path, time.Now()); state != "invalid" {
		t.Fatalf("canceled verification state=%q", state)
	}
	if canceled.backupCheck.path != "" {
		t.Fatal("canceled verification was cached")
	}
	if state := canceled.verifyBackupState(path); state != "ok" {
		t.Fatalf("verification after cancellation state=%q", state)
	}

	recoveryTemp := t.TempDir()
	blockedTemp := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedTemp, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", blockedTemp)

	s := &Server{}
	checkedAt := time.Unix(1700000000, 0)
	if state := s.verifyBackupStateAt(path, checkedAt); state != "invalid" {
		t.Fatalf("unavailable temp storage state=%q", state)
	}

	t.Setenv("TMPDIR", recoveryTemp)
	if state := s.verifyBackupStateAt(path, checkedAt.Add(backupInvalidCacheTTL-time.Nanosecond)); state != "invalid" {
		t.Fatalf("negative cache state=%q", state)
	}
	if s.backupCheck.checkedAt != checkedAt {
		t.Fatalf("negative cache was refreshed at %v", s.backupCheck.checkedAt)
	}
	if state := s.verifyBackupStateAt(path, checkedAt.Add(backupInvalidCacheTTL)); state != "ok" {
		t.Fatalf("recovered backup state=%q", state)
	}

	t.Setenv("TMPDIR", blockedTemp)
	if state := s.verifyBackupStateAt(path, checkedAt.Add(2*backupInvalidCacheTTL)); state != "ok" {
		t.Fatalf("success cache state=%q", state)
	}
}

func TestBackupStateCacheExpiresInvalidResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.mtbackup")
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}

	s := &Server{}
	checkedAt := time.Unix(1700000000, 0)
	if state := s.verifyBackupStateAt(path, checkedAt); state != "invalid" {
		t.Fatalf("initial state=%q", state)
	}
	if state := s.verifyBackupStateAt(path, checkedAt.Add(backupInvalidCacheTTL-time.Nanosecond)); state != "invalid" {
		t.Fatalf("cached state=%q", state)
	}
	if s.backupCheck.checkedAt != checkedAt {
		t.Fatalf("invalid result was refreshed at %v", s.backupCheck.checkedAt)
	}
	if state := s.verifyBackupStateAt(path, checkedAt.Add(backupInvalidCacheTTL)); state != "invalid" {
		t.Fatalf("expired state=%q", state)
	}
	if s.backupCheck.checkedAt != checkedAt.Add(backupInvalidCacheTTL) {
		t.Fatalf("expired invalid result was not rechecked: %v", s.backupCheck.checkedAt)
	}
}

func TestBackupStateWaitHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.mtbackup")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	busy := make(chan struct{})
	s := &Server{backupBusy: busy}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan string, 1)
	go func() { done <- s.verifyBackupStateContext(ctx, path, time.Now()) }()
	select {
	case state := <-done:
		if state != "invalid" || s.backupBusy != busy || s.backupCheck.path != "" {
			t.Fatalf("state=%q, busy=%v, cache=%+v", state, s.backupBusy, s.backupCheck)
		}
	case <-time.After(time.Second):
		close(busy)
		t.Fatal("canceled waiter remained blocked")
	}
}
