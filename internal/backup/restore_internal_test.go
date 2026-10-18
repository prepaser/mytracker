package backup

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"golang.org/x/sys/unix"
)

func restoreFixture(cfg *config.Config, phase string) restoreJournal {
	return restoreJournal{
		Phase: phase, StageDir: filepath.Join(cfg.DataDir, ".restore-stage-test"),
		OldDB:     filepath.Join(cfg.DataDir, ".restore-old-test.db"),
		OldProofs: filepath.Join(cfg.DataDir, ".restore-old-proofs-test"),
	}
}

func TestRecoverPreparedRestoreRollsBack(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	oldDB := filepath.Join(cfg.DataDir, ".restore-old-test.db")
	if err := os.WriteFile(oldDB, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.DBPath(), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	j := restoreFixture(cfg, "prepared")
	j.OldDB, j.HadDB = oldDB, true
	if err := writeJournal(cfg, j); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(cfg.DBPath())
	if err != nil || string(got) != "old" {
		t.Fatalf("db=%q err=%v", got, err)
	}
}

func TestRecoverStagingRestoreRemovesOnlyStage(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.DBPath(), []byte("live"), 0600); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(cfg.DataDir, ".restore-stage-stale")
	if err := os.MkdirAll(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "secret"), []byte("sensitive"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJournal(cfg, restoreJournal{Phase: "staging", StageDir: stage}); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("stage remains: %v", err)
	}
	if got, err := os.ReadFile(cfg.DBPath()); err != nil || string(got) != "live" {
		t.Fatalf("live db=%q err=%v", got, err)
	}
}

func TestStagingCleanupFailurePreservesJournal(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	stage, err := os.MkdirTemp(cfg.DataDir, ".restore-stage-*")
	if err != nil {
		t.Fatal(err)
	}
	j := restoreJournal{Phase: "staging", StageDir: stage}
	if err := writeJournal(cfg, j); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "proof"), []byte("proof"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stage, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stage, 0700) })
	if err := discardStagingRestore(cfg, j); err == nil {
		t.Fatal("stage removal unexpectedly succeeded")
	}
	if _, err := os.Stat(restoreJournalPath(cfg)); err != nil {
		t.Fatalf("journal lost: %v", err)
	}
	if err := os.Chmod(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestScavengeRemovesStaleArtifactsAndPreservesActiveJournalPaths(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	staleFile := filepath.Join(cfg.DataDir, ".export-stale.zip")
	if err := os.WriteFile(staleFile, []byte("sensitive"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleFile, old, old); err != nil {
		t.Fatal(err)
	}
	staleStage := filepath.Join(cfg.DataDir, ".restore-stage-stale")
	if err := os.MkdirAll(staleStage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleStage, old, old); err != nil {
		t.Fatal(err)
	}
	staleSource := filepath.Join(cfg.DataDir, ".restore-source-stale.mtbackup")
	if err := os.WriteFile(staleSource, []byte("sensitive"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleSource, old, old); err != nil {
		t.Fatal(err)
	}
	staleWork := filepath.Join(cfg.BackupsDir(), ".bundle-work-stale")
	if err := os.MkdirAll(staleWork, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleWork, old, old); err != nil {
		t.Fatal(err)
	}
	staleBundleTemp := filepath.Join(cfg.BackupsDir(), ".mytracker-old.mtbackup.tmp-stale")
	if err := os.WriteFile(staleBundleTemp, []byte("sensitive"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleBundleTemp, old, old); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(cfg.DataDir, ".export-active.zip")
	if err := os.WriteFile(active, []byte("active"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJournal(cfg, restoreJournal{Phase: "staging", StageDir: staleStage}); err != nil {
		t.Fatal(err)
	}
	if err := Scavenge(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Fatalf("stale export remains: %v", err)
	}
	for _, path := range []string{staleSource, staleWork, staleBundleTemp} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale artifact remains: %s (%v)", path, err)
		}
	}
	if _, err := os.Stat(staleStage); err != nil {
		t.Fatalf("journal stage removed: %v", err)
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("active export removed: %v", err)
	}
}

func TestScavengeTempRemovesStaleVerificationFiles(t *testing.T) {
	f, err := os.CreateTemp("", "mytracker-verify-scavenge-")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := ScavengeTemp(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale temp remains: %v", err)
	}
}

func TestScavengePreservesActiveExportWork(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	dir, err := os.MkdirTemp("", ".mytracker-export-work-*")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	if err := ScavengeTemp(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("active export removed: %v", err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := ScavengeTemp(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("stale export remains: %v", err)
	}
	orphan, err := os.MkdirTemp("", ".mytracker-export-work-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	if err := ScavengeTemp(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("unlocked orphan export remains: %v", err)
	}
}

func TestRecoverCommittedRestoreFinishes(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	oldDB := filepath.Join(cfg.DataDir, ".restore-old-test.db")
	stage := filepath.Join(cfg.DataDir, ".restore-stage-test")
	if err := os.WriteFile(oldDB, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	j := restoreFixture(cfg, "committed")
	j.OldDB, j.StageDir = oldDB, stage
	if err := writeJournal(cfg, j); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(cfg); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{oldDB, stage, restoreJournalPath(cfg)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s remains: %v", path, err)
		}
	}
}

func TestRecoverRejectsInvalidJournal(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(restoreJournalPath(cfg), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(cfg); err == nil {
		t.Fatal("invalid restore journal accepted")
	}
}

func TestRecoverPreparedRestoreWithoutOldData(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.DBPath(), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ProofsDir(), "new.tsr"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(cfg.DataDir, ".restore-stage-test")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	j := restoreFixture(cfg, "prepared")
	j.StageDir = stage
	if err := writeJournal(cfg, j); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.DBPath()); !os.IsNotExist(err) {
		t.Fatalf("new DB remains: %v", err)
	}
	if entries, err := os.ReadDir(cfg.ProofsDir()); err != nil || len(entries) != 0 {
		t.Fatalf("proofs=%v err=%v", entries, err)
	}
}

func TestRecoverPreparedRestorePreservesUnmovedExistingData(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.DBPath(), []byte("live"), 0600); err != nil {
		t.Fatal(err)
	}
	proof := filepath.Join(cfg.ProofsDir(), "1.tsr")
	if err := os.WriteFile(proof, []byte("proof"), 0600); err != nil {
		t.Fatal(err)
	}
	j := restoreFixture(cfg, "prepared")
	j.HadDB, j.HadProofs = true, true
	if err := writeJournal(cfg, j); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(cfg); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(cfg.DBPath()); err != nil || string(got) != "live" {
		t.Fatalf("db=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(proof); err != nil || string(got) != "proof" {
		t.Fatalf("proof=%q err=%v", got, err)
	}
}

func TestRecoverPreservesJournalWhenRollbackFails(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.DBPath()); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(cfg.DBPath(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DBPath(), "busy"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	oldDB := filepath.Join(cfg.DataDir, ".restore-old-test.db")
	if err := os.WriteFile(oldDB, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	j := restoreFixture(cfg, "prepared")
	j.OldDB, j.HadDB = oldDB, true
	if err := writeJournal(cfg, j); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(cfg); err == nil {
		t.Fatal("rollback failure was hidden")
	}
	if _, err := os.Stat(restoreJournalPath(cfg)); err != nil {
		t.Fatalf("restore journal removed after failure: %v", err)
	}
}

func TestRecoverPreservesJournalWhenFinishFails(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	oldDB := filepath.Join(cfg.DataDir, ".restore-old-test.db")
	if err := os.Mkdir(oldDB, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDB, "busy"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	j := restoreFixture(cfg, "committed")
	j.OldDB = oldDB
	if err := writeJournal(cfg, j); err != nil {
		t.Fatal(err)
	}
	if err := RecoverRestore(cfg); err == nil {
		t.Fatal("finish failure was hidden")
	}
	if _, err := os.Stat(restoreJournalPath(cfg)); err != nil {
		t.Fatalf("restore journal removed after failure: %v", err)
	}
}

func TestPruneBackupFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"mytracker-1.db", "mytracker-2.mtbackup", "mytracker-3.mtbackup", "notes.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prune(dir, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mytracker-1.db")); !os.IsNotExist(err) {
		t.Fatalf("old backup remains: %v", err)
	}
	if err := prune(dir, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mytracker-2.mtbackup")); !os.IsNotExist(err) {
		t.Fatalf("minimum keep not applied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if err := prune(filepath.Join(dir, "missing"), 1); err == nil {
		t.Fatal("missing backup directory accepted")
	}
}

func TestPrunePreservesExplicitBackup(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "mytracker-0001.mtbackup")
	newer := filepath.Join(dir, "mytracker-9999.mtbackup")
	for _, path := range []string{old, newer} {
		if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prune(dir, 1, old); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("explicitly preserved backup was removed: %v", err)
	}
	if _, err := os.Stat(newer); !os.IsNotExist(err) {
		t.Fatalf("unprotected backup remains: %v", err)
	}
}

func TestPrivacySafetyBackupPendingFailsClosed(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(dataDir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if pending, err := privacySafetyBackupPending(dataDir); err == nil || pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
}

func TestInstallNoClobber(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, ".tmp")
	base := filepath.Join(dir, "mytracker-20260831-120000.mtbackup")
	if err := os.WriteFile(tmp, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := installNoClobber(tmp, base)
	if err != nil {
		t.Fatal(err)
	}
	if got == base {
		t.Fatal("existing backup was selected")
	}
	if data, err := os.ReadFile(base); err != nil || string(data) != "old" {
		t.Fatalf("existing backup=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(got); err != nil || string(data) != "new" {
		t.Fatalf("installed backup=%q err=%v", data, err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("temporary file remains: %v", err)
	}
}

func TestCheckFileRejectsMalformedStructure(t *testing.T) {
	manifest, _ := json.Marshal(Manifest{FormatVersion: FormatVersion, SchemaVersion: 1, Files: map[string]FileInfo{}})
	dbHash := sha256.Sum256([]byte("db"))
	digestMismatch, _ := json.Marshal(Manifest{
		FormatVersion: FormatVersion, SchemaVersion: 1,
		Files: map[string]FileInfo{"mytracker.db": {SHA256: "00", Size: 2}},
	})
	unexpectedHash := sha256.Sum256([]byte("extra"))
	unexpected, _ := json.Marshal(Manifest{
		FormatVersion: FormatVersion, SchemaVersion: 1,
		Files: map[string]FileInfo{
			"mytracker.db": {SHA256: hex.EncodeToString(dbHash[:]), Size: 2},
			"extra":        {SHA256: hex.EncodeToString(unexpectedHash[:]), Size: 5},
		},
	})
	cases := []struct {
		name    string
		entries []struct{ name, body string }
	}{
		{"unsafe path", []struct{ name, body string }{{"../db", "x"}}},
		{"duplicate entry", []struct{ name, body string }{{"manifest.json", "{}"}, {"manifest.json", "{}"}}},
		{"missing manifest", []struct{ name, body string }{{"mytracker.db", "x"}}},
		{"unsupported format", []struct{ name, body string }{{"manifest.json", `{"format_version":99,"schema_version":1}`}}},
		{"missing database", []struct{ name, body string }{{"manifest.json", string(manifest)}}},
		{"digest mismatch", []struct{ name, body string }{{"manifest.json", string(digestMismatch)}, {"mytracker.db", "db"}}},
		{"unexpected entry", []struct{ name, body string }{
			{"manifest.json", string(unexpected)}, {"mytracker.db", "db"}, {"extra", "extra"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			for _, entry := range tc.entries {
				w, err := zw.Create(entry.name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write([]byte(entry.body)); err != nil {
					t.Fatal(err)
				}
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(t.TempDir(), tc.name+".mtbackup")
			if err := os.WriteFile(archive, buf.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			if err := CheckFile(archive); err == nil {
				t.Fatal("malformed backup accepted")
			}
		})
	}
}

func TestExtractRejectsInvalidAndDuplicateEntries(t *testing.T) {
	stage := t.TempDir()
	if err := extract(filepath.Join(t.TempDir(), "missing.mtbackup"), stage); err == nil {
		t.Fatal("missing archive extracted")
	}
	archive := filepath.Join(t.TempDir(), "duplicate.mtbackup")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for range 2 {
		w, err := zw.Create("mytracker.db")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("db")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := extract(archive, t.TempDir()); err == nil {
		t.Fatal("duplicate database extracted")
	}
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := extract(archive, blocked); err == nil {
		t.Fatal("archive extracted into file")
	}
}
