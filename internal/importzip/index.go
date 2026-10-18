package importzip

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

type archiveIndex struct {
	db   *sql.DB
	tx   *sql.Tx
	path string
	lock *os.File
}

type indexedEntry struct {
	Name   string
	Offset int64
	Size   uint64
	Digest string
}

func newArchiveIndex(ctx context.Context) (*archiveIndex, error) {
	f, err := os.CreateTemp("", "mytracker-import-index-*.db")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	db.SetMaxOpenConns(1)
	idx := &archiveIndex{db: db, path: path, lock: f}
	_, err = db.ExecContext(ctx, `
		PRAGMA journal_mode=OFF;
		PRAGMA synchronous=OFF;
		PRAGMA cache_size=-2048;
		PRAGMA temp_store=FILE;
		CREATE TABLE locations (
			old_id INTEGER PRIMARY KEY,
			new_id INTEGER,
			verify INTEGER NOT NULL DEFAULT 0,
			queued_digest BLOB
		);
		CREATE TABLE runs (
			old_id INTEGER PRIMARY KEY,
			location_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			new_id INTEGER
		);
		CREATE TABLE latest_runs (
			location_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			run_id INTEGER NOT NULL,
			PRIMARY KEY(location_id, kind)
		);
		CREATE TABLE providers (
			run_id INTEGER NOT NULL,
			provider_id TEXT NOT NULL,
			PRIMARY KEY(run_id, provider_id)
		);
		CREATE TABLE proof_refs (name TEXT PRIMARY KEY);
		CREATE TABLE entries (
			name TEXT PRIMARY KEY,
			offset INTEGER NOT NULL,
			size INTEGER NOT NULL,
			digest TEXT NOT NULL
		);
		CREATE TABLE manifest_digests (
			kind TEXT NOT NULL,
			name TEXT NOT NULL,
			digest TEXT NOT NULL,
			PRIMARY KEY(kind, name)
		);
	`)
	if err != nil {
		_ = idx.Close()
		return nil, err
	}
	return idx, nil
}

func (i *archiveIndex) addEntry(ctx context.Context, entry indexedEntry) error {
	_, err := i.exec(ctx, `INSERT INTO entries(name,offset,size,digest) VALUES(?,?,?,?)`,
		entry.Name, entry.Offset, entry.Size, entry.Digest)
	return err
}

func (i *archiveIndex) entry(ctx context.Context, name string) (indexedEntry, bool, error) {
	var entry indexedEntry
	err := i.row(ctx, `SELECT name,offset,size,digest FROM entries WHERE name=?`, name).
		Scan(&entry.Name, &entry.Offset, &entry.Size, &entry.Digest)
	if errors.Is(err, sql.ErrNoRows) {
		return indexedEntry{}, false, nil
	}
	return entry, err == nil, err
}

func (i *archiveIndex) eachEntryWithManifest(ctx context.Context, kind string, fn func(indexedEntry, string, bool) error) error {
	rows, err := i.rows(ctx, `SELECT e.name,e.offset,e.size,e.digest,m.digest
		FROM entries e LEFT JOIN manifest_digests m ON m.name=e.name AND m.kind=?
		WHERE e.name!='manifest.json' ORDER BY e.offset`, kind)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var entry indexedEntry
		var expected sql.NullString
		if err := rows.Scan(&entry.Name, &entry.Offset, &entry.Size, &entry.Digest, &expected); err != nil {
			return err
		}
		if err := fn(entry, expected.String, expected.Valid); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (i *archiveIndex) entryCount(ctx context.Context, prefix string) (int, error) {
	var count int
	err := i.row(ctx, `SELECT count(*) FROM entries WHERE name LIKE ?`, prefix+`%`).Scan(&count)
	return count, err
}

func (i *archiveIndex) setManifestDigest(ctx context.Context, kind, name, digest string) error {
	_, err := i.exec(ctx, `INSERT INTO manifest_digests(kind,name,digest) VALUES(?,?,?)
		ON CONFLICT(kind,name) DO UPDATE SET digest=excluded.digest`, kind, name, digest)
	return err
}

func (i *archiveIndex) manifestDigestCount(ctx context.Context, kind string) (int, error) {
	var count int
	err := i.row(ctx, `SELECT count(*) FROM manifest_digests WHERE kind=?`, kind).Scan(&count)
	return count, err
}

func (i *archiveIndex) Begin(ctx context.Context) error {
	if i.tx != nil {
		return errors.New("archive index transaction already open")
	}
	tx, err := i.db.BeginTx(ctx, nil)
	if err == nil {
		i.tx = tx
	}
	return err
}

func (i *archiveIndex) Commit() error {
	if i.tx == nil {
		return nil
	}
	err := i.tx.Commit()
	i.tx = nil
	return err
}

func (i *archiveIndex) Rollback() {
	if i.tx != nil {
		_ = i.tx.Rollback()
		i.tx = nil
	}
}

func (i *archiveIndex) Close() error {
	i.Rollback()
	var errs []error
	if i.db != nil {
		if err := i.db.Close(); err != nil {
			errs = append(errs, err)
		}
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
	}
	return errors.Join(errs...)
}

func (i *archiveIndex) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if i.tx != nil {
		return i.tx.ExecContext(ctx, query, args...)
	}
	return i.db.ExecContext(ctx, query, args...)
}

func (i *archiveIndex) row(ctx context.Context, query string, args ...any) *sql.Row {
	if i.tx != nil {
		return i.tx.QueryRowContext(ctx, query, args...)
	}
	return i.db.QueryRowContext(ctx, query, args...)
}

func (i *archiveIndex) rows(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if i.tx != nil {
		return i.tx.QueryContext(ctx, query, args...)
	}
	return i.db.QueryContext(ctx, query, args...)
}

func (i *archiveIndex) addLocation(ctx context.Context, id int64) error {
	_, err := i.exec(ctx, `INSERT INTO locations(old_id) VALUES(?)`, id)
	return err
}

func (i *archiveIndex) hasLocation(ctx context.Context, id int64) (bool, error) {
	var found int
	err := i.row(ctx, `SELECT 1 FROM locations WHERE old_id=?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (i *archiveIndex) mapLocation(ctx context.Context, oldID, newID int64, verify bool, queuedDigest []byte) error {
	_, err := i.exec(ctx, `UPDATE locations SET new_id=?, verify=verify OR ?, queued_digest=? WHERE old_id=?`,
		newID, verify, queuedDigest, oldID)
	return err
}

func (i *archiveIndex) markVerify(ctx context.Context, oldID int64) error {
	_, err := i.exec(ctx, `UPDATE locations SET verify=1 WHERE old_id=?`, oldID)
	return err
}

func (i *archiveIndex) locationID(ctx context.Context, oldID int64) (int64, bool, error) {
	var id sql.NullInt64
	err := i.row(ctx, `SELECT new_id FROM locations WHERE old_id=?`, oldID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id.Int64, id.Valid, nil
}

func (i *archiveIndex) addRun(ctx context.Context, id, locationID int64, kind string) error {
	if _, err := i.exec(ctx, `INSERT INTO runs(old_id,location_id,kind) VALUES(?,?,?)`, id, locationID, kind); err != nil {
		return err
	}
	_, err := i.exec(ctx, `INSERT INTO latest_runs(location_id,kind,run_id) VALUES(?,?,?)
		ON CONFLICT(location_id,kind) DO UPDATE SET run_id=max(run_id,excluded.run_id)`, locationID, kind, id)
	return err
}

func (i *archiveIndex) hasRun(ctx context.Context, id int64) (bool, error) {
	var found int
	err := i.row(ctx, `SELECT 1 FROM runs WHERE old_id=?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (i *archiveIndex) isLatestRun(ctx context.Context, id, locationID int64, kind string) (bool, error) {
	var latest int64
	err := i.row(ctx, `SELECT run_id FROM latest_runs WHERE location_id=? AND kind=?`, locationID, kind).Scan(&latest)
	return latest == id, err
}

func (i *archiveIndex) mapRun(ctx context.Context, oldID, newID int64) error {
	_, err := i.exec(ctx, `UPDATE runs SET new_id=? WHERE old_id=?`, newID, oldID)
	return err
}

func (i *archiveIndex) runID(ctx context.Context, oldID int64) (int64, bool, error) {
	var id sql.NullInt64
	err := i.row(ctx, `SELECT new_id FROM runs WHERE old_id=?`, oldID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id.Int64, id.Valid, nil
}

func (i *archiveIndex) addProvider(ctx context.Context, runID int64, provider string) error {
	_, err := i.exec(ctx, `INSERT INTO providers(run_id,provider_id) VALUES(?,?)`, runID, provider)
	return err
}

func (i *archiveIndex) providerCount(ctx context.Context, runID int64) (int, error) {
	var count int
	err := i.row(ctx, `SELECT count(*) FROM providers WHERE run_id=?`, runID).Scan(&count)
	return count, err
}

func (i *archiveIndex) addProofReference(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	_, err := i.exec(ctx, `INSERT INTO proof_refs(name) VALUES(?)`, name)
	return err
}

func (i *archiveIndex) proofReferenceCount(ctx context.Context) (int, error) {
	var count int
	err := i.row(ctx, `SELECT count(*) FROM proof_refs`).Scan(&count)
	return count, err
}

func (i *archiveIndex) eachVerificationLocation(ctx context.Context, fn func(int64) error) error {
	rows, err := i.rows(ctx, `SELECT new_id FROM locations WHERE verify=1 AND new_id IS NOT NULL ORDER BY old_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if err := fn(id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (i *archiveIndex) eachQueuedStamp(ctx context.Context, fn func(int64, []byte)) error {
	rows, err := i.rows(ctx, `SELECT new_id,queued_digest FROM locations WHERE queued_digest IS NOT NULL ORDER BY old_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var digest []byte
		if err := rows.Scan(&id, &digest); err != nil {
			return err
		}
		fn(id, digest)
	}
	return rows.Err()
}
