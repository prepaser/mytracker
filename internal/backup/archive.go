package backup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/zipguard"
	"golang.org/x/sys/unix"
)

type archiveFiles interface {
	Walk(context.Context, func(int64, *zip.File) error) error
	FileAt(int64) (*zip.File, error)
}

type openArchiveResult struct {
	file     *os.File
	files    archiveFiles
	index    *backupIndex
	declared uint64
}

func (a *openArchiveResult) Close() {
	if a.index != nil {
		_ = a.index.Close()
	}
	if a.file != nil {
		_ = a.file.Close()
	}
}

func indexArchive(ctx context.Context, files archiveFiles) (*openArchiveResult, error) {
	index, err := newBackupIndex(ctx, "")
	if err != nil {
		return nil, err
	}
	a := &openArchiveResult{files: files, index: index}
	if err := index.Begin(ctx); err != nil {
		a.Close()
		return nil, err
	}
	err = files.Walk(ctx, func(offset int64, f *zip.File) error {
		if f == nil || !validBackupName(f.Name) || f.UncompressedSize64 > uint64(entryLimit(f.Name)) {
			return fmt.Errorf("invalid backup entry")
		}
		if f.UncompressedSize64 > uint64(maxBackupUncompressedSize)-a.declared {
			return fmt.Errorf("backup exceeds uncompressed size limit")
		}
		a.declared += f.UncompressedSize64
		return index.Add(ctx, f.Name, offset, int64(f.UncompressedSize64))
	})
	if err == nil {
		err = index.Commit()
	}
	if err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

func openArchive(path string) (*openArchiveResult, error) {
	return openArchiveContext(context.Background(), path)
}

func openArchiveContext(ctx context.Context, path string) (*openArchiveResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBackupUncompressedSize {
		_ = f.Close()
		return nil, fmt.Errorf("backup exceeds size limit")
	}
	files, err := zipguard.Open(ctx, f, info.Size())
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	a, err := indexArchive(ctx, files)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	a.file = f
	return a, nil
}

func validBackupName(name string) bool {
	if name == "manifest.json" || name == "mytracker.db" {
		return true
	}
	if !strings.HasPrefix(name, "proofs/") || len(name) > 64 {
		return false
	}
	base := strings.TrimPrefix(name, "proofs/")
	ext := filepath.Ext(base)
	if ext != ".tsr" && ext != ".ots" {
		return false
	}
	id, err := strconv.ParseInt(strings.TrimSuffix(base, ext), 10, 64)
	return err == nil && id > 0 && base == strconv.FormatInt(id, 10)+ext
}

func verifyArchive(ctx context.Context, a *openArchiveResult) (Manifest, error) {
	var manifest Manifest
	mf, err := a.index.Get(ctx, "manifest.json")
	if err != nil {
		return manifest, fmt.Errorf("manifest missing: %w", err)
	}
	file, err := a.files.FileAt(mf.Offset)
	if err != nil {
		return manifest, err
	}
	rc, err := file.Open()
	if err != nil {
		return manifest, err
	}
	manifest, manifestSize, readErr := readManifest(ctx, rc, a.index)
	closeErr := rc.Close()
	if readErr != nil {
		return manifest, readErr
	}
	if closeErr != nil {
		return manifest, closeErr
	}
	if manifest.SchemaVersion > store.CurrentSchemaVersion {
		return manifest, fmt.Errorf("unsupported backup format or schema")
	}
	if _, err := a.index.Get(ctx, "mytracker.db"); err != nil {
		return manifest, fmt.Errorf("backup database missing: %w", err)
	}
	if missing, err := a.index.MissingInfo(ctx); err != nil || missing != 0 {
		if err != nil {
			return manifest, err
		}
		return manifest, fmt.Errorf("backup file set mismatch")
	}
	actual := uint64(manifestSize)
	if actual > a.declared || actual > uint64(maxBackupUncompressedSize) {
		return manifest, fmt.Errorf("backup exceeds uncompressed size limit")
	}
	tmp, err := os.CreateTemp("", "mytracker-verify-*.db")
	if err != nil {
		return manifest, err
	}
	defer func() {
		_ = tmp.Close()
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(tmp.Name() + suffix)
		}
	}()
	if err := unix.Flock(int(tmp.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return manifest, err
	}
	err = a.index.Each(ctx, func(item indexedFile) error {
		f, err := a.files.FileAt(item.Offset)
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		hash := sha256.New()
		var dst io.Writer = hash
		if item.Name == "mytracker.db" {
			dst = io.MultiWriter(tmp, hash)
		}
		n, copyErr := copyBoundedContext(ctx, dst, rc, entryLimit(item.Name))
		closeErr := rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if hex.EncodeToString(hash.Sum(nil)) != item.Info.SHA256 || n != item.Info.Size {
			return fmt.Errorf("digest mismatch for %s", item.Name)
		}
		if uint64(n) > uint64(maxBackupUncompressedSize)-actual {
			return fmt.Errorf("backup exceeds uncompressed size limit")
		}
		actual += uint64(n)
		return nil
	})
	if err != nil {
		return manifest, err
	}
	if err := tmp.Sync(); err != nil {
		return manifest, err
	}
	schema, err := schemaVersion(tmp.Name())
	if err != nil {
		return manifest, err
	}
	if schema != manifest.SchemaVersion || schema > store.CurrentSchemaVersion {
		return manifest, fmt.Errorf("backup schema mismatch")
	}
	if err := a.index.Begin(ctx); err != nil {
		return manifest, err
	}
	if err := eachReferencedProof(ctx, tmp.Name(), true, func(name string) error {
		return a.index.Reference(ctx, "proofs/"+name)
	}); err != nil {
		return manifest, err
	}
	if err := a.index.Commit(); err != nil {
		return manifest, err
	}
	if name, err := a.index.Unreferenced(ctx); err != nil {
		return manifest, err
	} else if name != "" {
		return manifest, fmt.Errorf("unreferenced proof: %s", name)
	}
	return manifest, nil
}

func collectManifest(ctx context.Context, m Manifest, index *backupIndex) (Manifest, error) {
	m.Files = make(map[string]FileInfo)
	err := index.Each(ctx, func(f indexedFile) error {
		m.Files[f.Name] = f.Info
		return nil
	})
	return m, err
}

func CheckFile(path string) error {
	return CheckFileContext(context.Background(), path)
}

func CheckFileContext(ctx context.Context, path string) error {
	a, err := openArchiveContext(ctx, path)
	if err != nil {
		return err
	}
	defer a.Close()
	_, err = verifyArchive(ctx, a)
	return err
}

func VerifyToWriter(ctx context.Context, path string, w io.Writer) error {
	a, err := openArchiveContext(ctx, path)
	if err != nil {
		return err
	}
	defer a.Close()
	m, err := verifyArchive(ctx, a)
	if err != nil {
		return err
	}
	return writeManifest(ctx, w, m, a.index)
}

func eachReferencedProof(ctx context.Context, path string, integrity bool, fn func(string) error) error {
	dsn, err := store.SQLiteReadOnlyDSN(path)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if integrity {
		var result string
		if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
			return err
		}
		if result != "ok" {
			return fmt.Errorf("integrity_check: %s", result)
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT id,tsa_status,ots_status FROM locations ORDER BY id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var tsa, ots string
		if err := rows.Scan(&id, &tsa, &ots); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if tsa == "ok" {
			if err := fn(strconv.FormatInt(id, 10) + ".tsr"); err != nil {
				return err
			}
		}
		if ots == "submitted" || ots == "confirmed" {
			if err := fn(strconv.FormatInt(id, 10) + ".ots"); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}
