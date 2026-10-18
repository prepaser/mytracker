package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

type ExportFilter struct {
	From int64
	To   int64
}

type ExportSnapshot struct {
	tx     *sql.Tx
	filter ExportFilter
	ctx    context.Context
}

func (s *Store) BeginExportSnapshot(f ExportFilter) (*ExportSnapshot, error) {
	return s.BeginExportSnapshotContext(context.Background(), f)
}

func (s *Store) BeginExportSnapshotContext(ctx context.Context, f ExportFilter) (*ExportSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	snapshot := &ExportSnapshot{tx: tx, filter: f, ctx: ctx}
	// Force SQLite to establish the read snapshot before control returns.
	if err := pinSnapshot(ctx, tx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return snapshot, nil
}

func (s *ExportSnapshot) Close() error {
	return s.tx.Rollback()
}

func (s *Store) ListAll() ([]Location, error) {
	return s.ListExport(ExportFilter{})
}

func locationFilter(f ExportFilter) (string, []any) {
	return buildLocationFilter(locationQueryFilter{from: f.From, to: f.To})
}

func (s *Store) exportQuery(f ExportFilter) (string, []any) {
	where, args := locationFilter(f)
	q := `SELECT ` + locationColumns + ` FROM locations WHERE ` + where
	q += ` ORDER BY tst ASC, id ASC`
	return q, args
}

func (s *Store) CountExport(f ExportFilter) (int, error) {
	where, args := locationFilter(f)
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM locations WHERE `+where, args...).Scan(&n)
	return n, err
}

func (s *ExportSnapshot) Count() (int, error) {
	where, args := locationFilter(s.filter)
	var n int
	err := s.tx.QueryRowContext(s.ctx, `SELECT COUNT(*) FROM locations WHERE `+where, args...).Scan(&n)
	return n, err
}

func (s *Store) CountExportProofs(f ExportFilter) (int, error) {
	where, args := locationFilter(f)
	q := `SELECT COALESCE(SUM(
		CASE WHEN tsa_status='ok' THEN 1 ELSE 0 END +
		CASE WHEN ots_status IN ('submitted','confirmed') THEN 1 ELSE 0 END
	),0) FROM locations WHERE ` + where
	var n int
	err := s.db.QueryRow(q, args...).Scan(&n)
	return n, err
}

func (s *Store) ListExport(f ExportFilter) ([]Location, error) {
	q, args := s.exportQuery(f)
	return s.queryLocations(q, args...)
}

func (s *Store) IterateExport(f ExportFilter, batch int, fn func([]Location) error) error {
	snapshot, err := s.BeginExportSnapshot(f)
	if err != nil {
		return err
	}
	defer func() { _ = snapshot.Close() }()
	return snapshot.Iterate(batch, fn)
}

func (s *ExportSnapshot) Iterate(batch int, fn func([]Location) error) error {
	where, args := locationFilter(s.filter)
	return iterateLocationKeyset(s.ctx, s.tx, `SELECT `+locationColumns+` FROM locations WHERE `+where, args, batch, fn)
}

func (s *Store) ListFailed() ([]Location, error) {
	return s.queryLocations(`SELECT ` + locationColumns + ` FROM locations WHERE tsa_status='fail' OR ots_status='fail' ORDER BY id ASC`)
}

func (s *Store) IterateFailed(batch int, fn func([]Location) error) error {
	if batch < 1 {
		batch = 100
	}
	var after int64
	for {
		locs, err := s.queryLocations(`SELECT `+locationColumns+`
			FROM locations
			WHERE id>? AND (tsa_status='fail' OR ots_status='fail')
			ORDER BY id ASC LIMIT ?`, after, batch)
		if err != nil {
			return err
		}
		if len(locs) == 0 {
			return nil
		}
		if err := fn(locs); err != nil {
			return err
		}
		if len(locs) < batch {
			return nil
		}
		after = locs[len(locs)-1].ID
	}
}

func (s *Store) DeleteOlderThan(proofsDir string, cutoff int64, by string) (int64, error) {
	return s.DeleteOlderThanContext(context.Background(), proofsDir, cutoff, by)
}

func (s *Store) DeleteOlderThanContext(ctx context.Context, proofsDir string, cutoff int64, by string) (int64, error) {
	return s.deleteOlderThanContext(ctx, proofsDir, cutoff, by, nil)
}

func (s *Store) LocationWatermarkContext(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM locations`).Scan(&id)
	return id, err
}

func (s *Store) DeleteOlderThanThroughContext(ctx context.Context, proofsDir string, cutoff int64, by string, maxID int64) (int64, error) {
	return s.deleteOlderThanContext(ctx, proofsDir, cutoff, by, &maxID)
}

func (s *Store) deleteOlderThanContext(ctx context.Context, proofsDir string, cutoff int64, by string, maxID *int64) (int64, error) {
	col := "tst"
	if by == "received" {
		col = "created_at"
	}
	return s.deleteWhereContext(ctx, proofsDir, col+` < ?`, []any{cutoff}, maxID)
}

func (s *Store) deleteWhereContext(ctx context.Context, proofsDir, where string, args []any, maxID *int64) (int64, error) {
	if maxID != nil {
		if *maxID < 0 {
			return 0, fmt.Errorf("invalid deletion watermark")
		}
		where += ` AND id<=?`
		args = append(args, *maxID)
	}
	unlockProofs, err := s.LockProofMutationContext(ctx)
	if err != nil {
		return 0, err
	}
	defer unlockProofs()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := invalidateTimelineWhereTx(tx, where, args...); err != nil {
		return 0, err
	}
	if err := prepareGeofenceStateKeysTx(tx); err != nil {
		return 0, err
	}
	if err := stageGeofenceStateKeysWhereTx(tx, where, args...); err != nil {
		return 0, err
	}
	if err := s.enqueueProofGCWhere(tx, where, args...); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM locations WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		if err := rebuildGeofenceStateKeysTx(tx); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if n > 0 {
		s.publish(Change{Topic: ChangeLocations, Bulk: true})
		s.publish(Change{Topic: ChangeGeofences, Bulk: true})
		s.publish(Change{Topic: ChangeTimeline, Bulk: true})
	}
	unlockProofs()
	return n, s.finishDeletionContext(ctx, proofsDir)
}

func (s *Store) DeleteRange(proofsDir string, f ExportFilter) (int64, error) {
	return s.DeleteRangeContext(context.Background(), proofsDir, f)
}

func (s *Store) DeleteRangeContext(ctx context.Context, proofsDir string, f ExportFilter) (int64, error) {
	return s.deleteRangeContext(ctx, proofsDir, f, nil)
}

func (s *Store) DeleteRangeThroughContext(ctx context.Context, proofsDir string, f ExportFilter, maxID int64) (int64, error) {
	return s.deleteRangeContext(ctx, proofsDir, f, &maxID)
}

func (s *Store) deleteRangeContext(ctx context.Context, proofsDir string, f ExportFilter, maxID *int64) (int64, error) {
	where, args := locationFilter(f)
	return s.deleteWhereContext(ctx, proofsDir, where, args, maxID)
}

func (s *Store) finishDeletionContext(ctx context.Context, proofsDir string) error {
	if _, err := s.ProcessProofGCContext(ctx, proofsDir, 1000); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: proof cleanup: %v", ErrDeletionPending, err)
	}
	status, err := s.ProofGCStatusContext(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: proof cleanup status: %v", ErrDeletionPending, err)
	}
	if status.Pending > 0 {
		return fmt.Errorf("%w: %d proof files remain", ErrDeletionPending, status.Pending)
	}
	if err := s.CheckpointContext(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrDeletionPending, err)
	}
	return nil
}

func (s *Store) ImportLocation(loc *Location) (int64, error) {
	var id int64
	err := s.ImportAtomic(func(tx *ImportTx) error {
		var err error
		id, err = tx.InsertLocation(loc)
		return err
	})
	return id, err
}

type ImportTx struct {
	tx              *sql.Tx
	timestampsReady bool
}

func (tx *ImportTx) EnqueueVerification(locationID int64, kind, trigger string, now int64) error {
	if kind != "tsa" && kind != "ots" {
		return fmt.Errorf("invalid verification kind")
	}
	_, err := tx.tx.Exec(`
		INSERT INTO proof_verification_jobs (
			location_id,kind,trigger,state,generation,attempts,max_attempts,
			next_attempt_at,last_error,created_at,updated_at
		) VALUES (?,?,?,'queued',1,0,5,0,'',?,?)
		ON CONFLICT(location_id,kind) DO UPDATE SET
			trigger=excluded.trigger,
			generation=proof_verification_jobs.generation+1,
			state=CASE WHEN proof_verification_jobs.state='running' THEN 'running' ELSE 'queued' END,
			attempts=CASE WHEN proof_verification_jobs.state='running' THEN proof_verification_jobs.attempts ELSE 0 END,
			next_attempt_at=CASE WHEN proof_verification_jobs.state='running' THEN proof_verification_jobs.next_attempt_at ELSE 0 END,
			last_error=CASE WHEN proof_verification_jobs.state='running' THEN proof_verification_jobs.last_error ELSE '' END,
			updated_at=excluded.updated_at`, locationID, kind, trigger, now, now)
	return err
}

func (tx *ImportTx) MarkImportTimestamp(tst int64) (bool, error) {
	if !tx.timestampsReady {
		if _, err := tx.tx.Exec(`CREATE TEMP TABLE IF NOT EXISTS mytracker_import_timestamps(tst INTEGER PRIMARY KEY)`); err != nil {
			return false, err
		}
		if _, err := tx.tx.Exec(`DELETE FROM mytracker_import_timestamps`); err != nil {
			return false, err
		}
		tx.timestampsReady = true
	}
	r, err := tx.tx.Exec(`INSERT OR IGNORE INTO mytracker_import_timestamps(tst) VALUES(?)`, tst)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (tx *ImportTx) RestoreImportedProof(locationID int64, kind, status string, tsaTime sql.NullInt64, tsaNonce []byte) error {
	if locationID < 1 {
		return fmt.Errorf("invalid location id")
	}
	var query string
	var args []any
	switch kind {
	case "tsa":
		if status != "pending" && status != "ok" && status != "fail" {
			return fmt.Errorf("invalid TSA status")
		}
		if tsaNonce == nil {
			tsaNonce = []byte{}
		}
		query = `UPDATE locations SET tsa_status=?,tsa_time=?,tsa_nonce=? WHERE id=?`
		args = []any{status, tsaTime, tsaNonce, locationID}
	case "ots":
		if status != "pending" && status != "submitted" && status != "confirmed" && status != "fail" {
			return fmt.Errorf("invalid OTS status")
		}
		query = `UPDATE locations SET ots_status=? WHERE id=?`
		args = []any{status, locationID}
	default:
		return fmt.Errorf("invalid proof kind")
	}
	result, err := tx.tx.Exec(query, args...)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return sql.ErrNoRows
	}
	result, err = tx.tx.Exec(`UPDATE stamp_jobs SET state=?,attempts=0,next_attempt_at=0,last_error='',
		lease_owner='',lease_until=0,updated_at=? WHERE location_id=? AND kind=?`,
		importedJobState(status), time.Now().Unix(), locationID, kind)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ImportAtomic(fn func(*ImportTx) error) error {
	return s.ImportAtomicContext(context.Background(), fn)
}

func (s *Store) ImportAtomicContext(ctx context.Context, fn func(*ImportTx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(&ImportTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publish(Change{Topic: ChangeLocations, Bulk: true})
	s.publish(Change{Topic: ChangeStampJobs, Bulk: true})
	s.publish(Change{Topic: ChangeVerification, Bulk: true})
	return nil
}

func (tx *ImportTx) InsertLocation(loc *Location, maxAttempts ...int) (int64, error) {
	if loc.TSANonce == nil {
		loc.TSANonce = []byte{}
	}
	id, err := allocateLocationID(tx.tx)
	if err != nil {
		return 0, err
	}
	res, err := tx.tx.Exec(`
		INSERT OR IGNORE INTO locations
			(id, user, device, tst, lat, lon, payload, digest, tsa_nonce, tsa_time, tsa_status, ots_status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, loc.User, loc.Device, loc.Tst, loc.Lat, loc.Lon, loc.Payload, loc.Digest,
		loc.TSANonce, loc.TsaTime, loc.TsaStatus, loc.OtsStatus, loc.CreatedAt,
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
	if _, err := tx.tx.Exec(`
		INSERT INTO stamp_jobs(location_id,kind,state,max_attempts,updated_at)
		VALUES (?,'tsa',?,?,?),(?,'ots',?,?,?)`,
		id, importedJobState(loc.TsaStatus), stampMaxAttempts(maxAttempts), now,
		id, importedJobState(loc.OtsStatus), stampMaxAttempts(maxAttempts), now); err != nil {
		return 0, err
	}
	if err := enqueueTimelineTx(tx.tx, loc.User, loc.Device, loc.Tst, now); err != nil {
		return 0, err
	}
	if _, err := processGeofencesTx(tx.tx, loc, id, true); err != nil {
		return 0, err
	}
	return id, nil
}

func (tx *ImportTx) ExistingLocation(user, device string, tst int64) (int64, []byte, error) {
	var id int64
	var digest []byte
	err := tx.tx.QueryRow(`SELECT id,digest FROM locations WHERE user=? AND device=? AND tst=?`,
		user, device, tst).Scan(&id, &digest)
	return id, digest, err
}

func importedJobState(status string) string {
	switch status {
	case "ok", "submitted", "confirmed":
		return "done"
	case "fail":
		return "failed"
	default:
		return "queued"
	}
}

func secureDB(path string) error {
	if err := os.Chmod(path, 0600); err != nil {
		return err
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if err := os.Chmod(sidecar, 0600); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *Store) SetImportedStatus(id int64, tsa, ots string, tsaTime sql.NullInt64) error {
	_, err := s.db.Exec(`UPDATE locations SET tsa_status=?, ots_status=?, tsa_time=? WHERE id=?`,
		tsa, ots, tsaTime, id)
	return err
}

func (s *Store) DeleteIDs(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if len(ids) > 0 {
		if _, err := tx.Exec(`UPDATE locations SET id=id WHERE id=?`, ids[0]); err != nil {
			return err
		}
		if err := prepareGeofenceStateKeysTx(tx); err != nil {
			return err
		}
	}
	for _, id := range ids {
		var user, device string
		var tst int64
		if err := tx.QueryRow(`SELECT user,device,tst FROM locations WHERE id=?`, id).Scan(&user, &device, &tst); err == nil {
			if err = enqueueTimelineTx(tx, user, device, tst, time.Now().Unix()); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := s.enqueueProofGCWhere(tx, `id=?`, id); err != nil {
			return err
		}
		if len(ids) > 0 {
			if err := stageGeofenceStateKeysWhereTx(tx, `l.id=?`, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM locations WHERE id=?`, id); err != nil {
			return err
		}
	}
	if len(ids) > 0 {
		if err := rebuildGeofenceStateKeysTx(tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(ids) > 0 {
		s.publish(Change{Topic: ChangeLocations, Bulk: true})
		s.publish(Change{Topic: ChangeGeofences, Bulk: true})
	}
	return nil
}
