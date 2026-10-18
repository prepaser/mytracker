package config_test

import (
	"errors"
	"testing"

	"github.com/prepaser/mytracker/internal/config"
)

func TestManagerRevisionRejectsStaleUpdate(t *testing.T) {
	cfg := managerTestConfig(t)
	store := seedSettingsStore(t, cfg)
	mgr := config.NewManager(cfg, store)
	_, revision := mgr.SnapshotWithRevision()
	if revision == "" {
		t.Fatal("empty revision")
	}
	if _, err := mgr.Update(func(s *config.Settings) error {
		s.TSAURL = "https://first.example/tsr"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.UpdateIfRevision(revision, func(s *config.Settings) error {
		s.BackupKeep = 1
		return nil
	}); !errors.Is(err, config.ErrRevisionConflict) {
		t.Fatalf("stale update err=%v", err)
	}
	if got := mgr.Snapshot().BackupKeep; got == 1 {
		t.Fatal("stale update changed settings")
	}
	if _, err := mgr.UpdateWithSessionRevocationIfRevision(revision, func(s *config.Settings) error {
		t.Fatal("stale password update callback ran")
		return nil
	}); !errors.Is(err, config.ErrRevisionConflict) {
		t.Fatalf("stale password update err=%v", err)
	}
	if store.saveCalls != 1 || store.revoked {
		t.Fatalf("stale update changed persistence: saves=%d revoked=%t", store.saveCalls, store.revoked)
	}
}

func TestManagerRevisionChangesAfterUpdate(t *testing.T) {
	cfg := managerTestConfig(t)
	store := seedSettingsStore(t, cfg)
	mgr := config.NewManager(cfg, store)
	_, revision := mgr.SnapshotWithRevision()
	if _, err := mgr.UpdateWithSessionRevocationIfRevision(revision, func(s *config.Settings) error {
		s.BackupKeep++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, currentRevision := mgr.SnapshotWithRevision()
	if currentRevision == revision || snapshot.BackupKeep != cfg.BackupKeep+1 {
		t.Fatal("revision did not change")
	}
	if !store.revoked {
		t.Fatal("session revocation was not requested")
	}
}

func TestManagerRevisionAcceptsEmptyListsAfterSave(t *testing.T) {
	cfg := managerTestConfig(t)
	store := seedSettingsStore(t, cfg)
	mgr := config.NewManager(cfg, store)
	updated, err := mgr.Update(func(s *config.Settings) error {
		s.TSAPolicyOIDs = []string{}
		s.EsploraURLs = []string{}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, revision := mgr.SnapshotWithRevision()
	if updated.SettingsRevision() != revision || snapshot.SettingsRevision() != revision {
		t.Fatal("config clones changed the committed revision")
	}
	if _, err := mgr.UpdateIfRevision(revision, func(s *config.Settings) error {
		s.BackupKeep++
		return nil
	}); err != nil {
		t.Fatalf("fresh revision rejected after empty list save: %v", err)
	}
}
