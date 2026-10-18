package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
	_ "modernc.org/sqlite"
)

var (
	ErrDuplicate                   = errors.New("duplicate location")
	ErrDeletionPending             = errors.New("deletion cleanup pending")
	ErrInvalidStampJobState        = errors.New("invalid stamp job state")
	ErrInvalidVerificationJobState = errors.New("invalid verification job state")
	ErrInvalidMaintenanceKind      = errors.New("invalid maintenance kind")
	ErrInvalidMaintenanceState     = errors.New("invalid maintenance state")
)

type Location struct {
	ID        int64
	User      string
	Device    string
	Tst       int64
	Lat       float64
	Lon       float64
	Payload   json.RawMessage
	Digest    []byte
	TSANonce  []byte
	TsaTime   sql.NullInt64
	TsaStatus string
	OtsStatus string
	CreatedAt int64
}

type Stats struct {
	Total      int
	TsaOK      int
	OtsPending int
	OtsOK      int
	TsaFail    int
	OtsFail    int
}

type Store struct {
	db             *sql.DB
	backupSem      chan struct{}
	proofSem       *semaphore.Weighted
	proofLockPath  string
	changes        changePublisher
	privacyMu      *semaphore.Weighted
	privacyScan    chan struct{}
	privacy        privacyCache
	shareTouchMu   sync.Mutex
	shareTouches   map[int64]int64
	timelineCursor atomic.Int64
}

const proofMutationWeight = 1 << 20

const locationColumns = `id,user,device,tst,lat,lon,payload,digest,tsa_nonce,tsa_time,tsa_status,ots_status,created_at`

func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := newStore(db, abs)
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := syncLocationAllocator(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sync location allocator: %w", err)
	}
	unlockBackup, err := s.LockBackupContext(context.Background())
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	unlockBackup()
	unlockProofs, err := s.LockProofMutationContext(context.Background())
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	defer unlockProofs()
	if err := s.recoverProofReplacements(filepath.Join(filepath.Dir(path), "proofs")); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("recover proof replacements: %w", err)
	}
	if err := s.recoverProofImports(filepath.Join(filepath.Dir(path), "proofs")); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("recover proof imports: %w", err)
	}
	if err := secureDB(path); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure database permissions: %w", err)
	}
	return s, nil
}

func newStore(db *sql.DB, path string) *Store {
	backupSem := make(chan struct{}, 1)
	backupSem <- struct{}{}
	return &Store{
		db: db, backupSem: backupSem, proofSem: semaphore.NewWeighted(proofMutationWeight),
		privacyScan:   make(chan struct{}, 1),
		privacyMu:     semaphore.NewWeighted(1),
		proofLockPath: filepath.Join(filepath.Dir(path), ".mytracker-proofs.lock"),
		shareTouches:  make(map[int64]int64),
	}
}

func OpenReadOnly(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return newStore(db, abs), nil
}

func syncLocationAllocator(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`
		WITH candidates(id) AS (
			SELECT id FROM locations
			UNION ALL
			SELECT CAST(substr(path,1,length(path)-4) AS INTEGER) FROM proof_gc
			WHERE substr(path,-4) IN ('.tsr','.ots') AND length(path)>4
				AND substr(path,1,length(path)-4) NOT GLOB '*[^0-9]*'
		), floor(id) AS (SELECT MAX(id) FROM candidates)
		INSERT INTO location_id_allocator(id)
		SELECT id FROM floor
		WHERE id>COALESCE((SELECT seq FROM sqlite_sequence WHERE name='location_id_allocator'),0)`); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM location_id_allocator`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) SetChangeSink(sink ChangeSink) { s.changes.set(sink) }

func (s *Store) LockProofSnapshot() func() {
	unlock, err := s.LockProofSnapshotContext(context.Background())
	if err != nil {
		panic(err)
	}
	return unlock
}

func (s *Store) LockProofMutation() func() {
	unlock, err := s.LockProofMutationContext(context.Background())
	if err != nil {
		panic(err)
	}
	return unlock
}

func (s *Store) LockProofSnapshotContext(ctx context.Context) (func(), error) {
	return s.lockProofs(ctx, 1, false)
}

func (s *Store) LockProofMutationContext(ctx context.Context) (func(), error) {
	return s.lockProofs(ctx, proofMutationWeight, true)
}

func (s *Store) publish(change Change) {
	if change.Topic == ChangeLocations {
		_ = s.privacyMu.Acquire(context.Background(), 1)
		s.privacy.generation++
		s.privacyMu.Release(1)
	}
	s.changes.publish(change)
}

func (s *Store) Ping() error { return s.db.Ping() }

func (s *Store) Checkpoint() error {
	return s.CheckpointContext(context.Background())
}

func (s *Store) CheckpointContext(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("wal checkpoint busy (%d/%d frames)", checkpointed, logFrames)
	}
	return nil
}

func (s *Store) Ready() error {
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	_, err = conn.ExecContext(context.Background(), `ROLLBACK`)
	return err
}

func (s *Store) Insert(loc *Location, maxAttempts ...int) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	id, err := allocateLocationID(tx)
	if err != nil {
		return 0, err
	}
	res, err := tx.Exec(`
		INSERT OR IGNORE INTO locations (id, user, device, tst, lat, lon, payload, digest, tsa_status, ots_status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', 'pending', ?)`,
		id, loc.User, loc.Device, loc.Tst, loc.Lat, loc.Lon, loc.Payload, loc.Digest, time.Now().Unix(),
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrDuplicate
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`
		INSERT INTO stamp_jobs(location_id,kind,state,max_attempts,updated_at)
		VALUES (?,'tsa','queued',?,?),(?,'ots','queued',?,?)`,
		id, stampMaxAttempts(maxAttempts), now, id, stampMaxAttempts(maxAttempts), now); err != nil {
		return 0, err
	}
	if err := enqueueTimelineTx(tx, loc.User, loc.Device, loc.Tst, now); err != nil {
		return 0, err
	}
	geofenceChanged, err := processGeofencesTx(tx, loc, id, false)
	if err != nil {
		return 0, err
	}
	if err := s.commitPrivacyInsert(tx, loc.Payload); err != nil {
		return 0, err
	}
	s.changes.publish(Change{Topic: ChangeLocations, LocationID: id, User: loc.User, Device: loc.Device, Tst: loc.Tst})
	s.publish(Change{Topic: ChangeStampJobs, LocationID: id})
	s.publish(Change{Topic: ChangeTimeline, LocationID: id})
	if geofenceChanged {
		s.publish(Change{Topic: ChangeGeofences, LocationID: id})
	}
	return id, nil
}

func allocateLocationID(tx *sql.Tx) (int64, error) {
	res, err := tx.Exec(`INSERT INTO location_id_allocator DEFAULT VALUES`)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM location_id_allocator WHERE id=?`, id); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) UpdateTSA(id int64, status string, tsaTime *int64) error {
	var (
		res sql.Result
		err error
	)
	if tsaTime != nil {
		res, err = s.db.Exec(`UPDATE locations SET tsa_status=?, tsa_time=? WHERE id=?`, status, *tsaTime, id)
	} else {
		res, err = s.db.Exec(`UPDATE locations SET tsa_status=? WHERE id=?`, status, id)
	}
	if changed(res, err) {
		s.publish(Change{Topic: ChangeStampJobs, LocationID: id})
	}
	return err
}

func (s *Store) UpdateOTS(id int64, status string) error {
	res, err := s.db.Exec(`UPDATE locations SET ots_status=? WHERE id=?`, status, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	if n > 0 {
		s.publish(Change{Topic: ChangeStampJobs, LocationID: id})
	}
	return nil
}

func changed(res sql.Result, err error) bool {
	if err != nil {
		return false
	}
	n, err := res.RowsAffected()
	return err == nil && n > 0
}

func (s *Store) Get(id int64) (*Location, error) {
	return s.GetContext(context.Background(), id)
}

func (s *Store) GetContext(ctx context.Context, id int64) (*Location, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+locationColumns+` FROM locations WHERE id=?`, id)
	return scanLocation(row)
}

func (s *Store) List(limit, offset int) ([]Location, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.queryLocations(`SELECT `+locationColumns+` FROM locations ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
}

func (s *Store) PendingOTS() ([]Location, error) {
	return s.queryLocations(`SELECT ` + locationColumns + ` FROM locations WHERE ots_status IN ('pending','submitted') ORDER BY id ASC`)
}

func (s *Store) PendingOTSLimit(limit int) ([]Location, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	return s.queryLocations(`SELECT `+locationColumns+`
		FROM locations WHERE ots_status IN ('pending','submitted') ORDER BY id ASC LIMIT ?`, limit)
}

func (s *Store) OTSUpgradeCandidates(after int64, limit int) ([]Location, error) {
	return s.OTSUpgradeCandidatesContext(context.Background(), after, limit)
}

func (s *Store) OTSUpgradeCandidatesContext(ctx context.Context, after int64, limit int) ([]Location, error) {
	if limit < 1 || limit > 1001 {
		limit = 101
	}
	return queryLocationsFromContext(ctx, s.db, `SELECT `+locationColumns+`
		FROM locations WHERE ots_status='submitted' AND id>? ORDER BY id ASC LIMIT ?`, after, limit)
}

func (s *Store) CountSubmittedOTS() (int, error) {
	return s.CountSubmittedOTSContext(context.Background())
}

func (s *Store) CountSubmittedOTSContext(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM locations WHERE ots_status='submitted'`).Scan(&n)
	return n, err
}

func (s *Store) CountPendingOTS() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM locations WHERE ots_status IN ('pending','submitted')`).Scan(&n)
	return n, err
}

func (s *Store) queryLocations(query string, args ...any) ([]Location, error) {
	return queryLocationsFromContext(context.Background(), s.db, query, args...)
}

type contextQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryLocationsFromContext(ctx context.Context, q contextQueryer, query string, args ...any) ([]Location, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Location
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		loc, err := scanLocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *loc)
	}
	return out, rows.Err()
}

func (s *Store) Stats() (Stats, error) {
	return s.StatsContext(context.Background())
}

func (s *Store) StatsContext(ctx context.Context) (Stats, error) {
	var initial Stats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM locations`).Scan(&initial.Total); err != nil {
		return initial, err
	}
	var next Stats
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
  COUNT(*) FILTER (WHERE tsa_status='ok'),
  COUNT(*) FILTER (WHERE ots_status IN ('pending','submitted')),
  COUNT(*) FILTER (WHERE ots_status='confirmed'),
  COUNT(*) FILTER (WHERE tsa_status='fail'),
  COUNT(*) FILTER (WHERE ots_status='fail') FROM locations`).Scan(
		&next.Total, &next.TsaOK, &next.OtsPending, &next.OtsOK, &next.TsaFail, &next.OtsFail)
	if err != nil {
		return initial, err
	}
	return next, nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanLocation(row scannable) (*Location, error) {
	var loc Location
	err := row.Scan(&loc.ID, &loc.User, &loc.Device, &loc.Tst, &loc.Lat, &loc.Lon,
		&loc.Payload, &loc.Digest, &loc.TSANonce, &loc.TsaTime, &loc.TsaStatus, &loc.OtsStatus, &loc.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &loc, nil
}

func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM locations`).Scan(&n)
	return n, err
}

func FormatTime(unix int64) string {
	return time.Unix(unix, 0).Format("2006-01-02 15:04:05")
}

func DigestHex(d []byte) string {
	return fmt.Sprintf("%x", d)
}
