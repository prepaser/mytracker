package export

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

type digestIndex struct {
	db   *sql.DB
	tx   *sql.Tx
	path string
	lock *os.File
}

func newDigestIndex(ctx context.Context, dir string) (*digestIndex, error) {
	f, err := os.CreateTemp(dir, ".mytracker-export-index-*.db")
	if err != nil {
		return nil, err
	}
	index := &digestIndex{path: f.Name(), lock: f}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = index.Close()
		return nil, err
	}
	abs, err := filepath.Abs(index.path)
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
	index.db, err = sql.Open("sqlite", dsn)
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	index.db.SetMaxOpenConns(1)
	index.db.SetMaxIdleConns(1)
	_, err = index.db.ExecContext(ctx, `
		PRAGMA journal_mode=OFF;
		PRAGMA synchronous=OFF;
		PRAGMA cache_size=-2048;
		PRAGMA temp_store=FILE;
		CREATE TABLE entries (
			name TEXT PRIMARY KEY,
			sha256 TEXT NOT NULL
		);`)
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	return index, nil
}

func (i *digestIndex) Begin(ctx context.Context) error {
	if i.tx != nil {
		return errors.New("export digest index transaction already open")
	}
	tx, err := i.db.BeginTx(ctx, nil)
	if err == nil {
		i.tx = tx
	}
	return err
}

func (i *digestIndex) Put(ctx context.Context, name, digest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := i.exec(ctx, `INSERT INTO entries(name, sha256) VALUES(?, ?)`, name, digest)
	return err
}

func (i *digestIndex) Has(ctx context.Context, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var one int
	err := i.queryRow(ctx, `SELECT 1 FROM entries WHERE name=?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (i *digestIndex) Rows(ctx context.Context) (*sql.Rows, error) {
	return i.query(ctx, `SELECT name, sha256 FROM entries ORDER BY name`)
}

func (i *digestIndex) Commit() error {
	if i.tx == nil {
		return nil
	}
	err := i.tx.Commit()
	i.tx = nil
	return err
}

func (i *digestIndex) Rollback() {
	if i.tx != nil {
		_ = i.tx.Rollback()
		i.tx = nil
	}
}

func (i *digestIndex) Close() error {
	i.Rollback()
	var errs []error
	if i.db != nil {
		errs = append(errs, i.db.Close())
		i.db = nil
	}
	if i.lock != nil {
		if err := unix.Flock(int(i.lock.Fd()), unix.LOCK_UN); err != nil {
			errs = append(errs, err)
		}
		if err := i.lock.Close(); err != nil {
			errs = append(errs, err)
		}
		i.lock = nil
	}
	if i.path != "" {
		if err := os.Remove(i.path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
		i.path = ""
	}
	return errors.Join(errs...)
}

func (i *digestIndex) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if i.tx != nil {
		return i.tx.ExecContext(ctx, query, args...)
	}
	return i.db.ExecContext(ctx, query, args...)
}

func (i *digestIndex) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if i.tx != nil {
		return i.tx.QueryContext(ctx, query, args...)
	}
	return i.db.QueryContext(ctx, query, args...)
}

func (i *digestIndex) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	if i.tx != nil {
		return i.tx.QueryRowContext(ctx, query, args...)
	}
	return i.db.QueryRowContext(ctx, query, args...)
}
