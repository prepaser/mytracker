package app

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
	_ "modernc.org/sqlite"
)

func TestRestoreDoesNotOpenNewerCurrentDatabase(t *testing.T) {
	root := t.TempDir()
	source := config.Default()
	source.DataDir = filepath.Join(root, "source")
	if err := source.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	sourceStore, err := store.Open(source.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	backupPath, err := backup.Create(source, sourceStore)
	_ = sourceStore.Close()
	if err != nil {
		t.Fatal(err)
	}

	target := config.Default()
	target.DataDir = filepath.Join(root, "target")
	if err := target.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", target.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version=999`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target.ProofsDir(), "unreadable-proof"), []byte("raw proof"), 0600); err != nil {
		t.Fatal(err)
	}

	if code := runRestoreCommand(backupPath, target); code != 0 {
		t.Fatalf("restore code=%d", code)
	}
	entries, err := filepath.Glob(filepath.Join(target.DataDir, ".restore-recovery-*"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("raw recovery snapshots=%v err=%v", entries, err)
	}
	for _, name := range []string{"mytracker.db", "proofs/unreadable-proof"} {
		if _, err := os.Stat(filepath.Join(entries[0], name)); err != nil {
			t.Fatalf("raw recovery entry %s: %v", name, err)
		}
	}
	restored, err := store.Open(target.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	_ = restored.Close()
}

func TestRestorePreservesCorruptCurrentDatabase(t *testing.T) {
	root := t.TempDir()
	source := config.Default()
	source.DataDir = filepath.Join(root, "source")
	if err := source.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	sourceStore, err := store.Open(source.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	backupPath, err := backup.Create(source, sourceStore)
	_ = sourceStore.Close()
	if err != nil {
		t.Fatal(err)
	}

	target := config.Default()
	target.DataDir = filepath.Join(root, "target")
	if err := target.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("not a sqlite database")
	if err := os.WriteFile(target.DBPath(), corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if code := runRestoreCommand(backupPath, target); code != 0 {
		t.Fatalf("restore code=%d", code)
	}
	entries, err := filepath.Glob(filepath.Join(target.DataDir, ".restore-recovery-*"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("raw recovery snapshots=%v err=%v", entries, err)
	}
	got, err := os.ReadFile(filepath.Join(entries[0], "mytracker.db"))
	if err != nil || string(got) != string(corrupt) {
		t.Fatalf("raw database=%q err=%v", got, err)
	}
}

func TestRestoreMissingDatabaseResidualState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		proof   bool
		sidecar bool
		wantRaw bool
	}{
		{name: "proof and sidecar", proof: true, sidecar: true, wantRaw: true},
		{name: "empty state", wantRaw: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			source := config.Default()
			source.DataDir = filepath.Join(root, "source")
			if err := source.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			sourceStore, err := store.Open(source.DBPath())
			if err != nil {
				t.Fatal(err)
			}
			backupPath, err := backup.Create(source, sourceStore)
			_ = sourceStore.Close()
			if err != nil {
				t.Fatal(err)
			}

			target := config.Default()
			target.DataDir = filepath.Join(root, "target")
			if err := target.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			if tc.proof {
				if err := os.WriteFile(filepath.Join(target.ProofsDir(), "orphan-proof"), []byte("proof"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.sidecar {
				if err := os.WriteFile(target.DBPath()+"-wal", []byte("wal"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if code := runRestoreCommand(backupPath, target); code != 0 {
				t.Fatalf("restore code=%d", code)
			}
			entries, err := filepath.Glob(filepath.Join(target.DataDir, ".restore-recovery-*"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantRaw {
				if len(entries) != 1 {
					t.Fatalf("raw recovery snapshots=%v", entries)
				}
				for _, name := range []string{"mytracker.db-wal", "proofs/orphan-proof"} {
					if _, err := os.Stat(filepath.Join(entries[0], name)); err != nil {
						t.Fatalf("raw recovery entry %s: %v", name, err)
					}
				}
			} else if len(entries) != 0 {
				t.Fatalf("unexpected raw recovery snapshots=%v", entries)
			}
		})
	}
}

func TestRestorePreservesBackupsWithInvalidCurrentSettings(t *testing.T) {
	root := t.TempDir()

	source := config.Default()
	source.DataDir = filepath.Join(root, "source")
	if err := source.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	sourceStore, err := store.Open(source.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	sourceBackup, err := backup.Create(source, sourceStore)
	_ = sourceStore.Close()
	if err != nil {
		t.Fatal(err)
	}

	target := config.Default()
	target.DataDir = filepath.Join(root, "target")
	if err := target.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	targetStore, err := store.Open(target.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := targetStore.SaveSettings([]byte(`{"unknown_setting":true}`), false); err != nil {
		_ = targetStore.Close()
		t.Fatal(err)
	}
	oldBackups := make([]string, 15)
	for i := range oldBackups {
		oldBackups[i], err = backup.CreateSafety(target, targetStore)
		if err != nil {
			_ = targetStore.Close()
			t.Fatal(err)
		}
	}
	if err := targetStore.Close(); err != nil {
		t.Fatal(err)
	}

	if code := runRestoreCommand(sourceBackup, target); code != 0 {
		t.Fatalf("restore code=%d", code)
	}
	for _, path := range oldBackups {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("existing backup %s was removed: %v", path, err)
		}
	}
	backups, err := filepath.Glob(filepath.Join(target.BackupsDir(), "mytracker-*.mtbackup"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != len(oldBackups)+1 {
		t.Fatalf("backup count=%d, want %d", len(backups), len(oldBackups)+1)
	}
}
