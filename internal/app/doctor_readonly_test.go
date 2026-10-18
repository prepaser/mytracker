package app

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
)

func TestDoctorDoesNotInitialize(t *testing.T) {
	t.Chdir(t.TempDir())
	if code := Run([]string{"doctor", "-config", "missing.toml"}); code != 1 {
		t.Fatalf("missing config code=%d", code)
	}
	if _, err := os.Stat("missing.toml"); !os.IsNotExist(err) {
		t.Fatalf("doctor created config: %v", err)
	}
	cfg := config.Default()
	if err := cfg.Save("config.toml"); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"doctor"}); code != 0 {
		t.Fatalf("uninitialized instance code=%d", code)
	}
	if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
		t.Fatalf("doctor created data directory: %v", err)
	}
}

func TestDoctorPreservesMigrationAndRestoreState(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	db, err := sql.Open("sqlite", cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version=8`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	report, err := inspectInstance(cfg)
	if err != nil || report["state"] != "migration_required" || report["schema_version"] != 8 {
		t.Fatalf("report=%v err=%v", report, err)
	}
	after, err := os.ReadFile(cfg.DBPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("doctor migrated the database: %v", err)
	}
	stage := filepath.Join(cfg.DataDir, ".restore-stage-test")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(cfg.DataDir, ".restore-journal.json")
	body, _ := json.Marshal(map[string]string{"phase": "staging", "stage_dir": stage})
	if err := os.WriteFile(journal, body, 0600); err != nil {
		t.Fatal(err)
	}
	report, err = inspectInstance(cfg)
	if err != nil || report["state"] != "recovery_required" {
		t.Fatalf("report=%v err=%v", report, err)
	}
	for _, path := range []string{stage, journal} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("doctor replayed the restore journal: %v", err)
		}
	}
}

func TestDoctorCurrentDatabaseIsReadOnly(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Create(cfg, st); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	artifact := filepath.Join(os.TempDir(), "mytracker-verify-stale.db")
	if err := os.WriteFile(artifact, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(artifact, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectInstance(cfg); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(cfg.DBPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("doctor modified the database: %v", err)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("doctor removed an existing artifact: %v", err)
	}
	st, err = store.OpenReadOnly(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.DeleteAllSessions(); err == nil {
		t.Fatal("read-only store accepted a write")
	}
}

func TestRestoreRecoveryRequiresExclusiveAccess(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	stage := filepath.Join(cfg.DataDir, ".restore-stage-test")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(cfg.DataDir, ".restore-journal.json")
	body, _ := json.Marshal(map[string]string{"phase": "staging", "stage_dir": stage})
	if err := os.WriteFile(journal, body, 0600); err != nil {
		t.Fatal(err)
	}
	first, err := lockDataDir(cfg.DataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := lockDataDir(cfg.DataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if err := recoverRestore(cfg, first, false); err == nil {
		t.Fatal("restore recovery ran concurrently with another process")
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("failed recovery changed staging")
	}
	_ = second.Close()
	if err := recoverRestore(cfg, first, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("recovery left journal: %v", err)
	}
	if lock, err := lockDataDir(cfg.DataDir, true); err == nil {
		_ = lock.Close()
		t.Fatal("recovery did not retain its shared data lock")
	}
}
