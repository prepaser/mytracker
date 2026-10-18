package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type backupIndex struct {
	db   *sql.DB
	tx   *sql.Tx
	file *os.File
}

type indexedFile struct {
	Name     string
	Offset   int64
	Declared int64
	Info     FileInfo
}

func newBackupIndex(ctx context.Context, dir string) (*backupIndex, error) {
	f, err := os.CreateTemp(dir, "mytracker-backup-index-*.db")
	if err != nil {
		return nil, err
	}
	i := &backupIndex{file: f}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = i.Close()
		return nil, err
	}
	abs, err := filepath.Abs(f.Name())
	if err != nil {
		_ = i.Close()
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
	i.db, err = sql.Open("sqlite", dsn)
	if err != nil {
		_ = i.Close()
		return nil, err
	}
	i.db.SetMaxOpenConns(1)
	_, err = i.db.ExecContext(ctx, `
		PRAGMA journal_mode=OFF;
		PRAGMA synchronous=OFF;
		PRAGMA cache_size=-2048;
		PRAGMA temp_store=FILE;
		CREATE TABLE files (
			name TEXT PRIMARY KEY,
			offset INTEGER NOT NULL,
			declared INTEGER NOT NULL,
			sha256 TEXT,
			size INTEGER,
			referenced INTEGER NOT NULL DEFAULT 0
		);`)
	if err != nil {
		_ = i.Close()
		return nil, err
	}
	return i, nil
}

func (i *backupIndex) Begin(ctx context.Context) error {
	if i.tx != nil {
		return errors.New("backup index transaction already open")
	}
	tx, err := i.db.BeginTx(ctx, nil)
	if err == nil {
		i.tx = tx
	}
	return err
}

func (i *backupIndex) Commit() error {
	if i.tx == nil {
		return nil
	}
	err := i.tx.Commit()
	i.tx = nil
	return err
}

func (i *backupIndex) Close() error {
	if i.tx != nil {
		_ = i.tx.Rollback()
		i.tx = nil
	}
	var errs []error
	if i.db != nil {
		errs = append(errs, i.db.Close())
	}
	if i.file != nil {
		errs = append(errs, i.file.Close())
		if err := os.Remove(i.file.Name()); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
		i.file = nil
	}
	return errors.Join(errs...)
}

func (i *backupIndex) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if i.tx != nil {
		return i.tx.ExecContext(ctx, query, args...)
	}
	return i.db.ExecContext(ctx, query, args...)
}

func (i *backupIndex) Add(ctx context.Context, name string, offset, declared int64) error {
	r, err := i.exec(ctx, `INSERT OR IGNORE INTO files(name,offset,declared) VALUES(?,?,?)`, name, offset, declared)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		err = fmt.Errorf("duplicate entry %q", name)
	}
	return err
}

func (i *backupIndex) SetInfo(ctx context.Context, name string, info FileInfo) error {
	r, err := i.exec(ctx, `UPDATE files SET sha256=?,size=? WHERE name=? AND sha256 IS NULL`, info.SHA256, info.Size, name)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		err = fmt.Errorf("duplicate or unknown manifest entry %q", name)
	}
	return err
}

func (i *backupIndex) Reference(ctx context.Context, name string) error {
	r, err := i.exec(ctx, `UPDATE files SET referenced=1 WHERE name=?`, name)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		err = fmt.Errorf("referenced proof missing: %s", name)
	}
	return err
}

func (i *backupIndex) Get(ctx context.Context, name string) (indexedFile, error) {
	var f indexedFile
	err := i.db.QueryRowContext(ctx, `SELECT name,offset,declared,COALESCE(sha256,''),COALESCE(size,0) FROM files WHERE name=?`, name).
		Scan(&f.Name, &f.Offset, &f.Declared, &f.Info.SHA256, &f.Info.Size)
	return f, err
}

func (i *backupIndex) MissingInfo(ctx context.Context) (int, error) {
	var n int
	err := i.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE name!='manifest.json' AND sha256 IS NULL`).Scan(&n)
	return n, err
}

func (i *backupIndex) Unreferenced(ctx context.Context) (string, error) {
	var name string
	err := i.db.QueryRowContext(ctx, `SELECT name FROM files WHERE name LIKE 'proofs/%' AND referenced=0 LIMIT 1`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

func (i *backupIndex) Each(ctx context.Context, fn func(indexedFile) error) error {
	rows, err := i.db.QueryContext(ctx, `SELECT name,offset,declared,COALESCE(sha256,''),COALESCE(size,0) FROM files WHERE name!='manifest.json' ORDER BY name`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var f indexedFile
		if err := rows.Scan(&f.Name, &f.Offset, &f.Declared, &f.Info.SHA256, &f.Info.Size); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(f); err != nil {
			return err
		}
	}
	return rows.Err()
}
