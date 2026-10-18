package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (s *Store) Backup(dir string) (string, error) {
	return s.BackupContext(context.Background(), dir)
}

func (s *Store) BackupContext(ctx context.Context, dir string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-s.backupSem:
	}
	defer func() { s.backupSem <- struct{}{} }()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("mytracker-%s.db", time.Now().UTC().Format("20060102-150405.000000000"))
	path := filepath.Join(dir, name)
	tmp := filepath.Join(dir, "."+name+".tmp")
	_ = os.Remove(tmp)
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Chmod(tmp, 0600); err != nil {
		return "", err
	}
	if err := CheckDBContext(ctx, tmp); err != nil {
		return "", fmt.Errorf("verify backup: %w", err)
	}
	f, err := os.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	d, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) BackupAndPrune(dir string, keep int) (string, error) {
	path, err := s.Backup(dir)
	if err != nil {
		return "", err
	}
	if keep < 1 {
		keep = 1
	}
	entries, err := backupFiles(dir)
	if err != nil {
		return path, err
	}
	for len(entries) > keep {
		if err := os.Remove(filepath.Join(dir, entries[0].Name())); err != nil {
			return path, err
		}
		entries = entries[1:]
	}
	return path, nil
}

func LatestBackup(dir string) (string, time.Time, error) {
	entries, err := backupFiles(dir)
	if err != nil {
		return "", time.Time{}, err
	}
	if len(entries) == 0 {
		return "", time.Time{}, os.ErrNotExist
	}
	now := time.Now()
	var latest os.FileInfo
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return "", time.Time{}, err
		}
		if latest != nil {
			future, latestFuture := info.ModTime().After(now), latest.ModTime().After(now)
			if future != latestFuture {
				if future {
					continue
				}
			} else if info.ModTime().Before(latest.ModTime()) ||
				info.ModTime().Equal(latest.ModTime()) && info.Name() < latest.Name() {
				continue
			}
		}
		latest = info
	}
	return filepath.Join(dir, latest.Name()), latest.ModTime(), nil
}

func backupFiles(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := entries[:0]
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "mytracker-") &&
			(strings.HasSuffix(e.Name(), ".db") || strings.HasSuffix(e.Name(), ".mtbackup")) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func CheckDB(path string) error {
	return CheckDBContext(context.Background(), path)
}

func CheckDBContext(ctx context.Context, path string) error {
	dsn, err := SQLiteReadOnlyDSN(path)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check: %s", result)
	}
	return nil
}

func SQLiteReadOnlyDSN(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(abs),
		RawQuery: "mode=ro",
	}).String(), nil
}
