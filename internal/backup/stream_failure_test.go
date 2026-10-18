package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
)

func TestReadManifestFailurePaths(t *testing.T) {
	manifest := `{"format_version":1,"schema_version":1,"files":{}}`
	t.Run("canceled", func(t *testing.T) {
		index, err := newBackupIndex(t.Context(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = index.Close() }()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, _, err := readManifest(ctx, strings.NewReader(manifest), index); !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v, want context.Canceled", err)
		}
	})

	t.Run("reader error", func(t *testing.T) {
		want := errors.New("manifest read failed")
		if _, _, err := readManifest(t.Context(), manifestErrorReader{err: want}, nil); err == nil {
			t.Fatal("reader error was accepted")
		}
	})
}

func TestVerifyToWriterFailureCleansIndexAndPreservesBundle(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	cfg := config.Default()
	cfg.DataDir = filepath.Join(root, "data")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	path, err := Create(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	before := backupIndexPaths(t)
	want := errors.New("output failed")
	if err := VerifyToWriter(t.Context(), path, failWriter{err: want}); !errors.Is(err, want) {
		t.Fatalf("err=%v, want %v", err, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("bundle after failed output: %v", err)
	}
	after := backupIndexPaths(t)
	if len(before) != len(after) {
		t.Fatalf("index files before=%v after=%v", before, after)
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			t.Fatalf("pre-existing index disappeared: %s", path)
		}
	}
}

func TestVerifyFileRejectsDatabaseProofReferenceMissingFromArchive(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proofs"), 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Insert(&store.Location{
		User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: make([]byte, 32),
	})
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	when := int64(1)
	if err := st.UpdateTSA(id, "ok", &when); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Checkpoint(); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := os.ReadFile(filepath.Join(root, "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(db)
	manifest, err := json.Marshal(Manifest{
		FormatVersion: FormatVersion,
		SchemaVersion: store.CurrentSchemaVersion,
		Files: map[string]FileInfo{
			"mytracker.db": {SHA256: hex.EncodeToString(sum[:]), Size: int64(len(db))},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for name, data := range map[string][]byte{"mytracker.db": db, "manifest.json": manifest} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "missing-proof.mtbackup")
	if err := os.WriteFile(path, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyFile(path); err == nil || !strings.Contains(err.Error(), "referenced proof missing: proofs/1.tsr") {
		t.Fatalf("err=%v", err)
	}
}

type manifestErrorReader struct{ err error }

func (r manifestErrorReader) Read([]byte) (int, error) { return 0, r.err }

type failWriter struct{ err error }

func (w failWriter) Write([]byte) (int, error) { return 0, w.err }

func backupIndexPaths(t *testing.T) map[string]struct{} {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(os.TempDir(), "mytracker-backup-index-*"))
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		result[path] = struct{}{}
	}
	return result
}
