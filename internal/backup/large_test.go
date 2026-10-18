package backup_test

import (
	"archive/zip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
)

func TestBackupBeyondLegacyProofLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("100,001 proof archive")
	}
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
	db, err := sql.Open("sqlite", cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100001)
		INSERT INTO locations(user,device,tst,lat,lon,payload,digest,created_at,tsa_status)
		SELECT 'u','d',x,1,2,'{}',x'01',x,'ok' FROM n`)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 100001; n++ {
		group := (n - 1) / 50000
		source := filepath.Join(cfg.ProofsDir(), fmt.Sprintf(".fixture-%d", group))
		if (n-1)%50000 == 0 {
			if err := os.WriteFile(source, []byte(fmt.Sprintf("proof-%d", group)), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Link(source, filepath.Join(cfg.ProofsDir(), fmt.Sprintf("%d.tsr", n))); err != nil {
			t.Fatal(err)
		}
	}
	path, err := backup.Create(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.VerifyToWriter(context.Background(), path, io.Discard); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	if len(zr.File) != 100003 {
		t.Fatalf("ZIP entries=%d", len(zr.File))
	}
	for _, f := range zr.File {
		if f.Name == "manifest.json" && f.UncompressedSize64 <= 4<<20 {
			t.Fatalf("manifest unexpectedly fits old cap: %d", f.UncompressedSize64)
		}
		if f.Name == "proofs/100001.tsr" {
			r, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(r)
			_ = r.Close()
			if err != nil || string(b) != "proof-2" {
				t.Fatalf("last proof=%q err=%v", b, err)
			}
		}
	}
}
