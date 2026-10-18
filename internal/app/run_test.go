package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/privacy"
	"github.com/prepaser/mytracker/internal/store"
	"golang.org/x/crypto/acme/autocert"
)

type failedOutput struct{}

func (failedOutput) Write([]byte) (int, error) { return 0, os.ErrClosed }

func seedSettings(t *testing.T, cfg *config.Config) {
	t.Helper()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if cfg.AdminHash == "" {
		cfg.AdminHash = "test-admin-hash"
	}
	if cfg.PubPass == "" {
		cfg.PubPass = "test-pub-pass"
	}
	data, err := json.Marshal(cfg.Settings)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSettings(data, false); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCommandOutputErrors(t *testing.T) {
	previous := commandOutput
	commandOutput = failedOutput{}
	t.Cleanup(func() { commandOutput = previous })
	if err := writeCommandJSON(map[string]bool{"ok": true}); err == nil {
		t.Fatal("JSON output error ignored")
	}
	if err := writeCommandLine("ok"); err == nil {
		t.Fatal("line output error ignored")
	}
}

func TestInvalidCommandArgumentsDoNotInitializeInstance(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	dataDir := filepath.Join(dir, "data")
	for _, args := range [][]string{
		{"restore", "-config", cfgPath},
		{"import-gpx", filepath.Join(dir, "in.gpx"), "-config", cfgPath},
		{"privacy-scrub", "extra", "-config", cfgPath},
		{"serve", "extra", "-config", cfgPath},
		{"doctor", "extra", "-config", cfgPath},
		{"backup", "extra", "-config", cfgPath},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			if code := Run(args); code != 2 {
				t.Fatalf("usage status=%d want=2", code)
			}
			if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
				t.Fatalf("invalid invocation created config: %v", err)
			}
			if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
				t.Fatalf("invalid invocation created data directory: %v", err)
			}
		})
	}
}

func TestVerificationCommandsScavengeCrashFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfgPath := filepath.Join(dir, "config.toml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"verify-backup", filepath.Join(dir, "missing.mtbackup")},
		{"verify-export", filepath.Join(dir, "missing.zip")},
		{"verify-export", filepath.Join(dir, "missing.zip"), "--proofs", "-config", cfgPath},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			path := filepath.Join(tmp, "mytracker-verify-crash.db")
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-48 * time.Hour)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			if code := Run(args); code != 1 {
				t.Fatalf("missing archive code=%d", code)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("crash file remains: %v", err)
			}
		})
	}
}

func TestOperationalCommands(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.BackupKeep = 1
	path := filepath.Join(dir, "config.toml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	seedSettings(t, cfg)
	gpxIn := filepath.Join(dir, "in.gpx")
	gpxOut := filepath.Join(dir, "out.gpx")
	if err := os.WriteFile(gpxIn, []byte(`<gpx><trk><trkseg><trkpt lat="37" lon="127"><time>2026-01-01T00:00:00Z</time></trkpt></trkseg></trk></gpx>`), 0600); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"import-gpx", gpxIn, "--user", "u", "--device", "d", "-config", path}); code != 0 {
		t.Fatalf("GPX import code=%d", code)
	}
	if code := Run([]string{"import-gpx", gpxIn, "--user", "u", "--device", "d", "-config", path}); code != 0 {
		t.Fatalf("duplicate GPX import=%d", code)
	}
	if code := Run([]string{"export-gpx", gpxOut, "--user", "u", "--device", "d", "-config", path}); code != 0 {
		t.Fatalf("GPX export code=%d", code)
	}
	if data, err := os.ReadFile(gpxOut); err != nil || !bytes.Contains(data, []byte("<trkpt")) {
		t.Fatalf("GPX output=%s err=%v", data, err)
	}
	if code := Run([]string{"import-gpx", gpxIn, "-config", path}); code != 2 {
		t.Fatalf("GPX import usage=%d", code)
	}
	if code := Run([]string{"export-gpx", "-config", path}); code != 2 {
		t.Fatalf("GPX export usage=%d", code)
	}
	badGPX := filepath.Join(dir, "bad.gpx")
	_ = os.WriteFile(badGPX, []byte(`<gpx><trkpt lat="1" lon="2"/></gpx>`), 0600)
	if code := Run([]string{"import-gpx", badGPX, "--user", "u", "--device", "d", "-config", path}); code != 1 {
		t.Fatalf("bad GPX import=%d", code)
	}
	duplicateGPX := filepath.Join(dir, "duplicate.gpx")
	_ = os.WriteFile(duplicateGPX, []byte(`<gpx><trkpt lat="1" lon="2"><time>2026-01-01T01:00:00Z</time></trkpt><trkpt lat="2" lon="3"><time>2026-01-01T01:00:00Z</time></trkpt></gpx>`), 0600)
	if code := Run([]string{"import-gpx", duplicateGPX, "--user", "u", "--device", "d", "-config", path}); code != 1 {
		t.Fatalf("duplicate GPX import=%d", code)
	}
	largeGPX := filepath.Join(dir, "large.gpx")
	if err := os.WriteFile(largeGPX, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(largeGPX, cfg.ImportMaxBytes+1); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"import-gpx", largeGPX, "--user", "u", "--device", "d", "-config", path}); code != 1 {
		t.Fatalf("large GPX import=%d", code)
	}
	if code := Run([]string{"import-gpx", filepath.Join(dir, "missing.gpx"), "--user", "u", "--device", "d", "-config", path}); code != 1 {
		t.Fatalf("missing GPX import=%d", code)
	}
	if code := Run([]string{"export-gpx", dir, "-config", path}); code != 1 {
		t.Fatalf("bad GPX export=%d", code)
	}
	for _, command := range []string{"doctor", "backup"} {
		if code := Run([]string{command, "-config", path}); code != 0 {
			t.Fatalf("%s code=%d", command, code)
		}
	}
	backupPath, _, err := store.LatestBackup(cfg.BackupsDir())
	if err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"verify-backup", backupPath}); code != 0 {
		t.Fatalf("verify backup code=%d", code)
	}
	initialPassword := config.InitialPasswordPath()
	if err := os.WriteFile(initialPassword, []byte("bootstrap\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"restore", backupPath, "-config", path}); code != 0 {
		t.Fatalf("restore code=%d", code)
	}
	if _, err := os.Stat(initialPassword); !os.IsNotExist(err) {
		t.Fatalf("bootstrap password remains after restore: %v", err)
	}
	if code := Run([]string{"privacy-scrub", "--purge-backups", "-config", path}); code != 0 {
		t.Fatalf("privacy-scrub code=%d", code)
	}
	if code := Run([]string{"verify-export"}); code != 2 {
		t.Fatalf("verify usage code=%d", code)
	}
	if code := Run([]string{"verify-backup"}); code != 2 {
		t.Fatalf("verify backup usage code=%d", code)
	}
	if code := Run([]string{"restore", "-config", path}); code != 2 {
		t.Fatalf("restore usage code=%d", code)
	}
	if code := Run([]string{"privacy-scrub", "extra", "-config", path}); code != 2 {
		t.Fatalf("privacy scrub usage code=%d", code)
	}
	if code := Run([]string{"doctor", "-unknown"}); code != 2 {
		t.Fatalf("flag usage code=%d", code)
	}
	var audit bytes.Buffer
	zw := zip.NewWriter(&audit)
	manifest, _ := zw.Create("manifest.json")
	_, _ = manifest.Write([]byte(`{"version":2}`))
	locations, _ := zw.Create("locations.jsonl")
	_, _ = locations.Write([]byte(`{"digest_sha256":"bad","payload":{}}` + "\n"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	auditPath := filepath.Join(dir, "audit.zip")
	if err := os.WriteFile(auditPath, audit.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"verify-export", auditPath, "--proofs", "--format", "text", "-config", path}); code != 1 {
		t.Fatalf("proof audit code=%d", code)
	}
	if code := Run([]string{"verify-export", auditPath}); code != 1 {
		t.Fatalf("archive verify code=%d", code)
	}
	if code := Run([]string{"verify-backup", auditPath}); code != 1 {
		t.Fatalf("invalid backup verify code=%d", code)
	}
	if code := Run([]string{"verify-export", auditPath, "--proofs", "--format", "yaml"}); code != 2 {
		t.Fatalf("proof audit format code=%d", code)
	}
	var validAudit bytes.Buffer
	validWriter := zip.NewWriter(&validAudit)
	manifest, _ = validWriter.Create("manifest.json")
	_, _ = manifest.Write([]byte(`{"version":2}`))
	if _, err := validWriter.Create("locations.jsonl"); err != nil {
		t.Fatal(err)
	}
	if err := validWriter.Close(); err != nil {
		t.Fatal(err)
	}
	validAuditPath := filepath.Join(dir, "valid-audit.zip")
	if err := os.WriteFile(validAuditPath, validAudit.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"json", "text"} {
		if code := Run([]string{"verify-export", validAuditPath, "--proofs", "--format", format, "-config", path}); code != 2 {
			t.Fatalf("empty %s proof audit code=%d", format, code)
		}
	}
	if code := Run([]string{"verify-export", validAuditPath}); code != 0 {
		t.Fatalf("valid archive verify code=%d", code)
	}
	func() {
		previous := commandOutput
		commandOutput = failedOutput{}
		defer func() { commandOutput = previous }()
		for _, args := range [][]string{
			{"verify-export", validAuditPath},
			{"verify-export", validAuditPath, "--proofs", "--format", "text", "-config", path},
			{"verify-backup", backupPath},
			{"doctor", "-config", path},
			{"import-gpx", gpxIn, "--user", "u", "--device", "d", "-config", path},
			{"export-gpx", filepath.Join(dir, "failed-output.gpx"), "-config", path},
			{"privacy-scrub", "-config", path},
			{"backup", "-config", path},
		} {
			if code := Run(args); code != 1 {
				t.Fatalf("output failure code=%d args=%v", code, args)
			}
		}
	}()
	if cfg, err := loadAuditConfig(filepath.Join(dir, "missing.toml")); err != nil || cfg == nil {
		t.Fatalf("default audit config=%+v err=%v", cfg, err)
	}
	if cfg, err := loadAuditConfig(""); err != nil || cfg == nil {
		t.Fatalf("empty audit config=%+v err=%v", cfg, err)
	}
	if code := Run([]string{"unknown", "-config", path}); code != 2 {
		t.Fatalf("unknown code=%d", code)
	}
}

func TestGPXExportRejectsInvalidRangesBeforeWriting(t *testing.T) {
	for _, bounds := range [][]string{
		{"--from", "-1"},
		{"--to", "-1"},
		{"--from", "200", "--to", "100"},
	} {
		t.Run(fmt.Sprint(bounds), func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "out.gpx")
			cfgPath := filepath.Join(dir, "missing.toml")
			if err := os.WriteFile(out, []byte("existing export"), 0600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"export-gpx", out, "-config", cfgPath}, bounds...)
			if code := Run(args); code != 2 {
				t.Fatalf("invalid range code=%d", code)
			}
			if data, err := os.ReadFile(out); err != nil || string(data) != "existing export" {
				t.Fatalf("output changed: data=%q err=%v", data, err)
			}
			if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
				t.Fatalf("invalid range initialized configuration: %v", err)
			}
		})
	}
}

func TestLoadAuditConfigUsesReadOnlySettings(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.TSAURL = "https://audit.example.test/tsr"
	cfgPath := filepath.Join(dir, "config.toml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	seedSettings(t, cfg)
	before, err := os.ReadFile(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loadAuditConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TSAURL != cfg.TSAURL || loaded.AdminHash != cfg.AdminHash {
		t.Fatalf("audit settings not loaded: %+v", loaded.Settings)
	}
	after, err := os.ReadFile(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only audit changed the database")
	}

	missing := config.Default()
	missing.DataDir = filepath.Join(dir, "missing-data")
	missingPath := filepath.Join(dir, "missing-config.toml")
	if err := missing.Save(missingPath); err != nil {
		t.Fatal(err)
	}
	defaults, err := loadAuditConfig(missingPath)
	if err != nil || defaults.TSAURL != config.Default().TSAURL {
		t.Fatalf("missing DB settings=%+v err=%v", defaults, err)
	}
	if _, err := os.Stat(missing.DBPath()); !os.IsNotExist(err) {
		t.Fatalf("audit created missing database: %v", err)
	}
}

func TestCLICommandsDoNotInitializeSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfgPath := filepath.Join(dir, "config.toml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"backup", "-config", cfgPath}); code != 0 {
		t.Fatalf("backup code=%d", code)
	}
	if _, err := os.Stat(config.InitialPasswordPath()); !os.IsNotExist(err) {
		t.Fatalf("CLI created bootstrap password: %v", err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	data, err := st.LoadSettings()
	if err != nil || len(data) != 0 {
		t.Fatalf("CLI initialized settings: %s err=%v", data, err)
	}
}

func TestRestoreValidatesRestoredSettingsAndSkipsCurrentSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	source := config.Default()
	source.DataDir = filepath.Join(dir, "source-data")
	seedSettings(t, source)
	sourceStore, err := store.Open(source.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	validBackup, err := backup.Create(source, sourceStore)
	_ = sourceStore.Close()
	if err != nil {
		t.Fatal(err)
	}

	target := config.Default()
	target.DataDir = filepath.Join(dir, "target-data")
	targetPath := filepath.Join(dir, "target.toml")
	if err := target.Save(targetPath); err != nil {
		t.Fatal(err)
	}
	if err := target.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	targetStore, err := store.Open(target.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := targetStore.SaveSettings([]byte(`{"old_invalid_setting":true}`), false); err != nil {
		_ = targetStore.Close()
		t.Fatal(err)
	}
	if err := targetStore.Close(); err != nil {
		t.Fatal(err)
	}
	bootstrap := config.InitialPasswordPath()
	if err := os.WriteFile(bootstrap, []byte("bootstrap\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"restore", validBackup, "-config", targetPath}); code != 0 {
		t.Fatalf("restore with corrupt current settings code=%d", code)
	}
	if _, err := os.Stat(bootstrap); !os.IsNotExist(err) {
		t.Fatalf("bootstrap password remains after restore: %v", err)
	}

	badSource := config.Default()
	badSource.DataDir = filepath.Join(dir, "bad-source-data")
	if err := badSource.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	badStore, err := store.Open(badSource.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := badStore.SaveSettings([]byte(`{"unknown_setting":true}`), false); err != nil {
		_ = badStore.Close()
		t.Fatal(err)
	}
	badBackup, err := backup.Create(badSource, badStore)
	_ = badStore.Close()
	if err != nil {
		t.Fatal(err)
	}

	second := config.Default()
	second.DataDir = filepath.Join(dir, "second-data")
	secondPath := filepath.Join(dir, "second.toml")
	if err := second.Save(secondPath); err != nil {
		t.Fatal(err)
	}
	if err := second.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.InitialPasswordPath(), []byte("keep\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"restore", badBackup, "-config", secondPath}); code != 1 {
		t.Fatalf("restore with invalid saved settings code=%d", code)
	}
	if _, err := os.Stat(config.InitialPasswordPath()); err != nil {
		t.Fatalf("failed restore removed bootstrap password: %v", err)
	}
	secondStore, err := store.Open(second.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = secondStore.Close() }()
	settings, err := secondStore.LoadSettings()
	if err != nil || len(settings) != 0 {
		t.Fatalf("invalid restore changed target settings: %s err=%v", settings, err)
	}
}

func TestRunOperationalFailures(t *testing.T) {
	t.Chdir(t.TempDir())
	newConfig := func(t *testing.T) (*config.Config, string) {
		t.Helper()
		dir := t.TempDir()
		cfg := config.Default()
		cfg.DataDir = filepath.Join(dir, "data")
		path := filepath.Join(dir, "config.toml")
		if err := cfg.Save(path); err != nil {
			t.Fatal(err)
		}
		return cfg, path
	}

	t.Run("restore recovery", func(t *testing.T) {
		cfg, path := newConfig(t)
		if err := cfg.EnsureDirs(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.DataDir, ".restore-journal.json"), []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
		if code := Run([]string{"doctor", "-config", path}); code != 1 {
			t.Fatalf("code=%d", code)
		}
	})

	t.Run("database open", func(t *testing.T) {
		cfg, path := newConfig(t)
		if err := cfg.EnsureDirs(); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(cfg.DBPath(), 0700); err != nil {
			t.Fatal(err)
		}
		if code := Run([]string{"doctor", "-config", path}); code != 1 {
			t.Fatalf("code=%d", code)
		}
	})

	t.Run("backup proof missing", func(t *testing.T) {
		cfg, path := newConfig(t)
		if err := cfg.EnsureDirs(); err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(cfg.DBPath())
		if err != nil {
			t.Fatal(err)
		}
		id, err := st.Insert(&store.Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: []byte{1}})
		if err != nil {
			t.Fatal(err)
		}
		stampTime := int64(1)
		if err := st.UpdateTSA(id, "ok", &stampTime); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		if code := Run([]string{"backup", "-config", path}); code != 1 {
			t.Fatalf("code=%d", code)
		}
	})

	t.Run("restore source missing", func(t *testing.T) {
		cfg, path := newConfig(t)
		if err := cfg.EnsureDirs(); err != nil {
			t.Fatal(err)
		}
		initialPassword := config.InitialPasswordPath()
		if err := os.WriteFile(initialPassword, []byte("bootstrap\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if code := Run([]string{"restore", filepath.Join(t.TempDir(), "missing.mtbackup"), "-config", path}); code != 1 {
			t.Fatalf("code=%d", code)
		}
		if _, err := os.Stat(initialPassword); err != nil {
			t.Fatalf("failed restore removed bootstrap password: %v", err)
		}
	})

	t.Run("doctor corrupt backup", func(t *testing.T) {
		cfg, path := newConfig(t)
		if err := cfg.EnsureDirs(); err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(cfg.DBPath())
		if err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		if err := os.WriteFile(filepath.Join(cfg.BackupsDir(), "mytracker-z.mtbackup"), []byte("bad"), 0600); err != nil {
			t.Fatal(err)
		}
		if code := Run([]string{"doctor", "-config", path}); code != 1 {
			t.Fatalf("code=%d", code)
		}
	})

	t.Run("audit config", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "bad.toml")
		if err := os.WriteFile(bad, []byte(":"), 0600); err != nil {
			t.Fatal(err)
		}
		if code := Run([]string{"verify-export", "missing.zip", "--proofs", "-config", bad}); code != 1 {
			t.Fatalf("code=%d", code)
		}
	})

}

func TestCleanupSessionsAndBackupRemoval(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now().Unix()
	_, _ = st.CreateSession("expired-session", now-100000, now-1)
	_, _ = st.CleanupSessions(now, int64(cfg.SessionIdleDuration().Seconds()))
	if count, _ := st.SessionCount(); count != 0 {
		t.Fatalf("sessions=%d", count)
	}
	if count, oldest := backupRetentionInfo(cfg.BackupsDir()); count != 0 || oldest != "" {
		t.Fatalf("empty backups=%d oldest=%q", count, oldest)
	}
	keep := filepath.Join(cfg.BackupsDir(), "mytracker-keep.mtbackup")
	old := filepath.Join(cfg.BackupsDir(), "mytracker-old.db")
	for _, path := range []string{keep, old, filepath.Join(cfg.BackupsDir(), "note.txt")} {
		if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if count, oldest := backupRetentionInfo(cfg.BackupsDir()); count != 2 || oldest == "" {
		t.Fatalf("backups=%d oldest=%q", count, oldest)
	}
	if err := privacy.RemoveBackupsExcept(cfg.BackupsDir(), keep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old backup remains: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal(err)
	}
}

func TestDataAndServerLocks(t *testing.T) {
	dir := t.TempDir()
	first, err := lockDataDir(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := lockDataDir(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Close()
	if lock, err := lockDataDir(dir, true); err == nil {
		_ = lock.Close()
		t.Fatal("exclusive restore lock acquired while shared lock is held")
	}
	serverPath := filepath.Join(dir, ".mytracker-server.lock")
	server, err := lockFile(serverPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	if duplicate, err := lockFile(serverPath, true); err == nil {
		_ = duplicate.Close()
		t.Fatal("duplicate server lock acquired")
	}
	if _, err := os.Stat(serverPath); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveSQLiteSidecars(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "mytracker.db")
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(db+suffix, []byte("stale"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeSQLiteSidecars(db); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(db + suffix); !os.IsNotExist(err) {
			t.Fatalf("sidecar %s remains: %v", suffix, err)
		}
	}
	if err := removeSQLiteSidecars(db); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(db+"-wal/child", 0700); err != nil {
		t.Fatal(err)
	}
	if err := removeSQLiteSidecars(db); err == nil {
		t.Fatal("sidecar removal failure was ignored")
	}
}

func TestWarnListenAddr(t *testing.T) {
	cfg := config.Default()
	cfg.ListenAddr = ":8080"
	warnListenAddr(cfg)
	cfg.TLSCertFile = "server.crt"
	cfg.TLSKeyFile = "server.key"
	warnListenAddr(cfg)
	cfg.ListenAddr = "127.0.0.1:8080"
	warnListenAddr(cfg)
}

func TestPrivacyScrubPurgeSafetyBackupLifecycle(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	seedSettings(t, cfg)
	safety := filepath.Join(cfg.DataDir, privacySafetyBackupName)
	if err := os.WriteFile(safety, []byte("safety"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"privacy-scrub", "--purge-backups", "-config", path}); code != 0 {
		t.Fatalf("privacy-scrub code=%d", code)
	}
	if _, err := os.Stat(safety); !os.IsNotExist(err) {
		t.Fatalf("safety backup remains after successful purge: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(safety, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"privacy-scrub", "--purge-backups", "-config", path}); code != 1 {
		t.Fatalf("privacy-scrub failure code=%d", code)
	}
	if _, err := os.Stat(safety); err != nil {
		t.Fatalf("failed purge removed safety backup: %v", err)
	}
}

func TestPrivacyScrubPreservesSafetyWithPendingGC(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	seedSettings(t, cfg)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Insert(&store.Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: []byte{1}})
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.ProofsDir(), fmt.Sprintf("%d.tsr", id), "blocked"), 0700); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	_, err = st.DeleteRange(cfg.ProofsDir(), store.ExportFilter{From: 1})
	if !errors.Is(err, store.ErrDeletionPending) {
		_ = st.Close()
		t.Fatalf("delete err=%v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	safety := privacySafetyBackupPath(cfg.DataDir)
	if err := os.WriteFile(safety, []byte("safety"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"privacy-scrub", "--purge-backups", "-config", path}); code != 1 {
		t.Fatalf("privacy-scrub code=%d", code)
	}
	if data, err := os.ReadFile(safety); err != nil || string(data) != "safety" {
		t.Fatalf("safety changed: %q (%v)", data, err)
	}
}

func TestDoctorReportsLegacyBackupAndRejectsCorruption(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	backupPath, err := st.Backup(cfg.BackupsDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.InitialPasswordPath(), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privacySafetyBackupPath(cfg.DataDir), []byte("safety"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := doctor(cfg, st, false)
	if err != nil {
		t.Fatal(err)
	}
	backupState := report["backup"].(map[string]any)
	privacy := report["privacy"].(map[string]any)
	if backupState["state"] != "ok" || backupState["path"] != backupPath ||
		privacy["initial_password_file"] != true || privacy["settings_initialized"] != false ||
		privacy["purge_pending"] != true {
		t.Fatalf("report=%+v", report)
	}
	if report["timeline_jobs"] == nil || report["geofence_deliveries"] == nil {
		t.Fatalf("worker diagnostics missing: %+v", report)
	}

	corrupt := filepath.Join(cfg.BackupsDir(), "mytracker-z.mtbackup")
	if err := os.WriteFile(corrupt, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := doctor(cfg, st, false); err == nil {
		t.Fatal("corrupt latest backup accepted")
	}
}

func TestServeContextShutdown(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.TSAURL = "http://127.0.0.1:1"
	cfg.OTSCalendars = []string{"http://127.0.0.1:1"}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	mgr := config.NewManager(cfg, st)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code, stopped := serveManaged(ctx, cfg, mgr, st); code != 0 || !stopped {
		t.Fatalf("serve code=%d stopped=%t", code, stopped)
	}
	if err := st.Ping(); err != nil {
		t.Fatalf("store closed during serve shutdown: %v", err)
	}
}

func TestShutdownExitCode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exitCode int
		stopped  bool
		want     int
	}{
		{name: "clean", exitCode: 0, stopped: true, want: 0},
		{name: "shutdown failure", exitCode: 0, stopped: false, want: 1},
		{name: "serve failure", exitCode: 1, stopped: true, want: 1},
		{name: "serve and shutdown failure", exitCode: 1, stopped: false, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shutdownExitCode(tc.exitCode, tc.stopped); got != tc.want {
				t.Fatalf("exit code=%d stopped=%t got=%d, want=%d", tc.exitCode, tc.stopped, got, tc.want)
			}
		})
	}
}

func TestConfigureHTTPSModes(t *testing.T) {
	cfg := config.Default()
	setup, err := configureHTTPS(cfg)
	if err != nil || setup.mode != "http" || setup.tlsConfig != nil {
		t.Fatalf("HTTP setup=%+v err=%v", setup, err)
	}
	cfg.TLSCertFile = filepath.Join(t.TempDir(), "missing.crt")
	cfg.TLSKeyFile = filepath.Join(t.TempDir(), "missing.key")
	if _, err := configureHTTPS(cfg); err == nil {
		t.Fatal("missing certificate accepted")
	}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cert := tlsServer.TLS.Certificates[0]
	tlsServer.Close()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.TLSCertFile, cfg.TLSKeyFile = certPath, keyPath
	setup, err = configureHTTPS(cfg)
	if err != nil || setup.mode != "manual" || len(setup.tlsConfig.Certificates) != 1 {
		t.Fatalf("manual setup=%+v err=%v", setup, err)
	}
	cfg = config.Default()
	cfg.DataDir = t.TempDir()
	cfg.AutoHTTPSDomains = []string{"tracker.example.com"}
	setup, err = configureHTTPS(cfg)
	if err != nil || setup.mode != "automatic" || setup.tlsConfig == nil || setup.httpServer == nil {
		t.Fatalf("automatic setup=%+v err=%v", setup, err)
	}
	if setup.tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("minimum TLS version=%d", setup.tlsConfig.MinVersion)
	}
	cfg.ListenAddr = ""
	setup, err = configureHTTPS(cfg)
	if err != nil || setup.httpServer != nil {
		t.Fatalf("TLS-ALPN setup=%+v err=%v", setup, err)
	}
}

func TestAutoHTTPSRedirectRestrictsHost(t *testing.T) {
	cfg := config.Default()
	cfg.AutoHTTPSDomains = []string{"tracker.example.com"}
	srv := newHTTPRedirectServer(cfg, &autocert.Manager{HostPolicy: autoHTTPSHostPolicy(cfg.AutoHTTPSDomains)})

	req := httptest.NewRequest(http.MethodGet, "http://tracker.example.com/map?q=1", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "https://tracker.example.com/map?q=1" {
		t.Fatalf("redirect code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	cfg.HTTPSListenAddr = ":8443"
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Location"); got != "https://tracker.example.com:8443/map?q=1" {
		t.Fatalf("separate HTTPS port redirect=%q", got)
	}
	cfg.HTTPSListenAddr = ":443"
	for _, tc := range []struct{ method, host string }{
		{http.MethodGet, "evil.example"},
		{http.MethodPost, "tracker.example.com"},
	} {
		req = httptest.NewRequest(tc.method, "http://"+tc.host+"/", nil)
		rec = httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s code=%d", tc.method, tc.host, rec.Code)
		}
	}
	for _, host := range []string{"tracker.example.com", "tracker.example.com:80", "TRACKER.EXAMPLE.COM"} {
		req = httptest.NewRequest(http.MethodGet, "http://"+host+"/.well-known/acme-challenge/missing", nil)
		rec = httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("ACME host %q code=%d body=%q", host, rec.Code, rec.Body.String())
		}
	}
	for _, host := range []string{"tracker.example.com.", "evil.example:80"} {
		err := autoHTTPSHostPolicy(cfg.AutoHTTPSDomains)(t.Context(), host)
		if (host == "tracker.example.com.") != (err == nil) {
			t.Fatalf("host %q err=%v", host, err)
		}
	}
}

func TestServeReturnsFailureWhenListenerIsBusy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.ListenAddr = listener.Addr().String()
	cfg.TSAURL = "http://127.0.0.1:1"
	cfg.OTSCalendars = []string{"http://127.0.0.1:1"}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	mgr := config.NewManager(cfg, st)
	if code := serve(t.Context(), cfg, mgr, st); code != 1 {
		t.Fatalf("serve code=%d", code)
	}
}

func TestRunFailsFastOnInvalidConfigAndLocks(t *testing.T) {
	dir := t.TempDir()
	badTOML := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(badTOML, []byte(":"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"doctor", "-config", badTOML}); code != 1 {
		t.Fatalf("bad TOML code=%d", code)
	}

	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data-file")
	if err := os.WriteFile(cfg.DataDir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "invalid-dir.toml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"doctor", "-config", cfgPath}); code != 1 {
		t.Fatalf("invalid data directory code=%d", code)
	}

	cfg.DataDir = filepath.Join(dir, "data")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	dataLock, err := lockDataDir(cfg.DataDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"backup", "-config", cfgPath}); code != 1 {
		t.Fatalf("busy data directory code=%d", code)
	}
	_ = dataLock.Close()

	serverLock, err := lockFile(filepath.Join(cfg.DataDir, ".mytracker-server.lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"serve", "-config", cfgPath}); code != 1 {
		t.Fatalf("busy server code=%d", code)
	}
	_ = serverLock.Close()

	t.Setenv("MYTRACKER_LISTEN_ADDR", "invalid")
	if code := Run([]string{"doctor", "-config", cfgPath}); code != 1 {
		t.Fatalf("invalid listen override code=%d", code)
	}
}
