package backup_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
)

func TestDeletionWatermarkKeepsNewArrivals(t *testing.T) {
	for _, kind := range []string{"range", "retention", "empty"} {
		t.Run(kind, func(t *testing.T) {
			cfg := config.Default()
			cfg.DataDir = t.TempDir()
			if err := cfg.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(cfg.DBPath())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			insert := func(tst int64) int64 {
				t.Helper()
				id, err := st.Insert(&store.Location{User: "u", Device: "d", Tst: tst, Payload: []byte(`{}`), Digest: make([]byte, 32)})
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			if kind != "empty" {
				insert(1)
			}
			ctx := context.Background()
			maxID, err := st.LocationWatermarkContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backup.Create(cfg, st); err != nil {
				t.Fatal(err)
			}
			late := insert(2)
			var n int64
			if kind == "retention" {
				n, err = st.DeleteOlderThanThroughContext(ctx, cfg.ProofsDir(), 100, "tst", maxID)
			} else {
				n, err = st.DeleteRangeThroughContext(ctx, cfg.ProofsDir(), store.ExportFilter{To: 100}, maxID)
			}
			want := int64(1)
			if kind == "empty" {
				want = 0
			}
			if err != nil || n != want {
				t.Fatalf("deleted=%d err=%v", n, err)
			}
			if _, err := st.Get(late); err != nil {
				t.Fatalf("arrival outside backup was deleted: %v", err)
			}
		})
	}
}

func TestBundlePruningWaitsForDeletionCleanup(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.BackupKeep = 1
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.ImportAtomic(func(tx *store.ImportTx) error {
		for i := range 601 {
			if _, err := tx.InsertLocation(&store.Location{User: "u", Device: "d", Tst: int64(i + 1), Payload: []byte(`{}`), Digest: make([]byte, 32)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pre, err := backup.Create(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := st.DeleteRange(cfg.ProofsDir(), store.ExportFilter{From: 1}); n != 601 || !errors.Is(err, store.ErrDeletionPending) {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
	post, err := backup.Create(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pre); err != nil {
		t.Fatalf("pending deletion recovery backup was pruned: %v", err)
	}
	if n, err := st.ProcessProofGC(cfg.ProofsDir(), 1000); n != 202 || err != nil {
		t.Fatalf("cleanup=%d err=%v", n, err)
	}
	latest, err := backup.Create(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{pre, post} {
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatalf("completed deletion backup was not pruned: %v", err)
		}
	}
	if _, err := backup.VerifyFile(latest); err != nil {
		t.Fatal(err)
	}
}

func TestBundlePruningWaitsForPendingPrivacyPurge(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.BackupKeep = 1
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.Insert(&store.Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, ".privacy-safety.mtbackup"), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := backup.Create(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, ".privacy-purge.pending"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cfg.DataDir, ".privacy-safety.mtbackup")); err != nil {
		t.Fatal(err)
	}
	second, err := backup.Create(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{first, second} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("pending purge recovery backup was pruned: %v", err)
		}
	}
}

func TestBundlePruningPreservesBackupWhileWALIsBusy(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.BackupKeep = 1
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.Insert(&store.Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	pre, err := backup.Create(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.BeginExportSnapshot(store.ExportFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	if _, err := st.Insert(&store.Location{User: "u", Device: "d", Tst: 2, Payload: []byte(`{}`), Digest: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Create(cfg, st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pre); err != nil {
		t.Fatalf("backup was pruned before the checkpoint completed: %v", err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Create(cfg, st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pre); !os.IsNotExist(err) {
		t.Fatalf("pruning did not resume: %v", err)
	}
}

func TestBundleCreateVerifyAndRestore(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(root, "source")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Insert(&store.Location{
		User: "u", Device: "d", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: make([]byte, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := int64(2)
	if err := st.UpdateTSA(id, "ok", &ts); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateOTS(id, "submitted"); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{".tsr", ".ots"} {
		if err := os.WriteFile(filepath.Join(cfg.ProofsDir(), "1"+ext), []byte("proof-"+ext), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateSession("backup-session-token", 1, 1<<62); err != nil {
		t.Fatal(err)
	}
	path, err := backup.Create(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(path) != ".mtbackup" {
		t.Fatalf("path=%s", path)
	}
	manifest, err := backup.VerifyFile(path)
	if err != nil || manifest.FormatVersion != backup.FormatVersion || len(manifest.Files) != 3 {
		t.Fatalf("manifest=%+v err=%v", manifest, err)
	}
	staged, err := backup.StageRestoreSource(path, cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if staged == path {
		t.Fatal("restore source was not staged")
	}
	if _, err := backup.VerifyFile(staged); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(staged)
	_ = st.Close()
	missingTarget := config.Default()
	missingTarget.DataDir = filepath.Join(root, "missing-target")
	if err := backup.Restore(path, missingTarget); err == nil {
		t.Fatal("restore into missing data directory succeeded")
	}

	target := config.Default()
	target.DataDir = filepath.Join(root, "target")
	if err := target.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(path, target); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Open(target.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	if n, err := restored.Count(); err != nil || n != 1 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	if n, err := restored.SessionCount(); err != nil || n != 0 {
		t.Fatalf("restored sessions=%d err=%v", n, err)
	}
	if b, err := os.ReadFile(filepath.Join(target.ProofsDir(), "1.tsr")); err != nil || string(b) != "proof-.tsr" {
		t.Fatalf("proof=%q err=%v", b, err)
	}
}

func TestBundleCreateWithLiteralSQLitePath(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data#percent%question? space")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	path, err := backup.Create(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backup.VerifyFile(path); err != nil {
		t.Fatal(err)
	}
}

func TestBundleRejectsCorruptionAndMissingProof(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.Insert(&store.Location{
		User: "u", Device: "d", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: make([]byte, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := int64(2)
	if err := st.UpdateTSA(id, "ok", &ts); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Create(cfg, st); err == nil {
		t.Fatal("backup accepted missing proof")
	}
	if err := os.WriteFile(filepath.Join(cfg.ProofsDir(), "1.tsr"), []byte("proof"), 0600); err != nil {
		t.Fatal(err)
	}
	path, err := backup.Create(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	future := rewriteManifestSchema(t, path, store.CurrentSchemaVersion+1)
	if _, err := backup.VerifyFile(future); err == nil {
		t.Fatal("future backup schema accepted")
	}
	mismatch := rewriteManifestSchema(t, path, store.CurrentSchemaVersion-1)
	if _, err := backup.VerifyFile(mismatch); err == nil {
		t.Fatal("backup manifest schema mismatch accepted")
	}
	tampered := addOrphanProof(t, path)
	if _, err := backup.VerifyFile(tampered); err == nil {
		t.Fatal("backup with unreferenced proof accepted")
	}
	target := config.Default()
	target.DataDir = filepath.Join(t.TempDir(), "restore")
	if err := target.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(tampered, target); err == nil {
		t.Fatal("restore with unreferenced proof accepted")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 32 {
		t.Fatal("backup too small")
	}
	b[len(b)/2] ^= 0xff
	corrupt := filepath.Join(t.TempDir(), "corrupt.mtbackup")
	if err := os.WriteFile(corrupt, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.VerifyFile(corrupt); err == nil {
		t.Fatal("corrupt backup accepted")
	}
	if err := backup.Restore(corrupt, cfg); err == nil {
		t.Fatal("corrupt backup restored")
	}
	if staged, err := backup.StageRestoreSource(corrupt, cfg.DataDir); err == nil || staged != "" {
		t.Fatalf("corrupt source staged=%q err=%v", staged, err)
	}
	if _, err := backup.StageRestoreSource(filepath.Join(t.TempDir(), "missing"), cfg.DataDir); err == nil {
		t.Fatal("missing source staged")
	}
	if _, err := backup.StageRestoreSource(cfg.DataDir, cfg.DataDir); err == nil {
		t.Fatal("directory staged as backup")
	}
	if staged, err := backup.StageRestoreSource(path,
		filepath.Join(t.TempDir(), "missing-dir")); err == nil || staged != "" {
		t.Fatalf("missing target staged=%q err=%v", staged, err)
	}
}

func rewriteManifestSchema(t *testing.T, path string, schema int) string {
	t.Helper()
	archive, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()

	var manifest backup.Manifest
	entries := make(map[string][]byte, len(archive.File))
	for _, file := range archive.File {
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if file.Name == "manifest.json" {
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			continue
		}
		entries[file.Name] = data
	}
	manifest.SchemaVersion = schema
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "future-schema.mtbackup")
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
	}
	w, err := zw.Create("manifest.json")
	if err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if _, err := w.Write(manifestData); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return target
}

func addOrphanProof(t *testing.T, path string) string {
	t.Helper()
	archive, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()

	var manifest backup.Manifest
	entries := make(map[string][]byte, len(archive.File))
	for _, file := range archive.File {
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if file.Name == "manifest.json" {
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			continue
		}
		entries[file.Name] = data
	}
	orphan := []byte("orphan proof")
	sum := sha256.Sum256(orphan)
	entries["proofs/999.tsr"] = orphan
	manifest.Files["proofs/999.tsr"] = backup.FileInfo{
		SHA256: hex.EncodeToString(sum[:]),
		Size:   int64(len(orphan)),
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	w, err := zw.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(manifestData); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	tampered := filepath.Join(t.TempDir(), "orphan.mtbackup")
	if err := os.WriteFile(tampered, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return tampered
}
