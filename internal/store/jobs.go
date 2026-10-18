package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/prepaser/mytracker/internal/slogx"
)

var ErrLeaseLost = errors.New("stamp job lease lost")

func stampMaxAttempts(attempts []int) int {
	if len(attempts) == 0 {
		return 3
	}
	return max(1, attempts[0])
}

type StampJob struct {
	ID            int64  `json:"id"`
	LocationID    int64  `json:"location_id"`
	Kind          string `json:"kind"`
	State         string `json:"state"`
	Attempts      int    `json:"attempts"`
	MaxAttempts   int    `json:"max_attempts"`
	NextAttemptAt int64  `json:"next_attempt_at"`
	LastError     string `json:"last_error,omitempty"`
	UpdatedAt     int64  `json:"updated_at"`
	LeaseOwner    string `json:"lease_owner,omitempty"`
	LeaseUntil    int64  `json:"lease_until,omitempty"`
	Digest        []byte `json:"-"`
}

type JobStatus struct {
	Queued       int   `json:"queued"`
	Running      int   `json:"running"`
	Retry        int   `json:"retry"`
	Failed       int   `json:"failed"`
	OldestQueued int64 `json:"oldest_queued,omitempty"`
}

func (s *Store) RecoverStampJobs(maxAttempts int) error {
	if maxAttempts < 1 {
		maxAttempts = 3
	}
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`UPDATE locations SET tsa_status='fail' WHERE id IN (
			SELECT location_id FROM stamp_jobs
			WHERE state IN ('running','queued','retry') AND attempts>=? AND kind='tsa')`,
		`UPDATE locations SET ots_status='fail' WHERE id IN (
			SELECT location_id FROM stamp_jobs
			WHERE state IN ('running','queued','retry') AND attempts>=? AND kind='ots')`,
	} {
		if _, err := tx.Exec(q, maxAttempts); err != nil {
			return err
		}
	}
	res, err := tx.Exec(`
		UPDATE stamp_jobs
		SET state=CASE WHEN attempts>=? THEN 'failed' ELSE 'queued' END,
		    max_attempts=?, next_attempt_at=0, lease_owner='', lease_until=0, updated_at=?
		WHERE state='running'`, maxAttempts, maxAttempts, now)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	res, err = tx.Exec(`UPDATE stamp_jobs SET
		state=CASE WHEN attempts>=? THEN 'failed' ELSE state END,
		max_attempts=?,next_attempt_at=CASE WHEN attempts>=? THEN 0 ELSE next_attempt_at END,
		last_error=CASE WHEN attempts>=? THEN 'retry limit exceeded' ELSE last_error END
		WHERE state IN ('queued','retry')`, maxAttempts, maxAttempts, maxAttempts, maxAttempts)
	if err != nil {
		return err
	}
	updated, err := res.RowsAffected()
	if err != nil {
		return err
	}
	changed += updated
	if err := tx.Commit(); err != nil {
		return err
	}
	if changed > 0 {
		s.publish(Change{Topic: ChangeStampJobs, Bulk: true})
	}
	return nil
}

func (s *Store) SetJobMaxAttempts(locationID int64, attempts int) error {
	if attempts < 1 {
		attempts = 1
	}
	_, err := s.db.Exec(`UPDATE stamp_jobs SET max_attempts=? WHERE location_id=? AND state NOT IN ('done','failed')`, attempts, locationID)
	return err
}

func (s *Store) ClaimStampJob(now int64, owner string, leaseUntil int64) (*StampJob, error) {
	var j StampJob
	err := s.db.QueryRow(`
		UPDATE stamp_jobs
		SET state='running',attempts=attempts+1,lease_owner=?,lease_until=?,updated_at=?
		WHERE id=(
			SELECT id FROM stamp_jobs
			WHERE attempts<max_attempts AND (
				(state IN ('queued','retry') AND next_attempt_at<=?)
				OR (state='running' AND lease_until<=?)
			)
			ORDER BY CASE WHEN state='running' THEN 0 ELSE 1 END,next_attempt_at,id LIMIT 1
		)
		RETURNING id,location_id,kind,state,attempts,max_attempts,next_attempt_at,last_error,updated_at,lease_owner,lease_until`,
		owner, leaseUntil, now, now, now).Scan(&j.ID, &j.LocationID, &j.Kind, &j.State, &j.Attempts,
		&j.MaxAttempts, &j.NextAttemptAt, &j.LastError, &j.UpdatedAt, &j.LeaseOwner, &j.LeaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT digest FROM locations WHERE id=?`, j.LocationID).Scan(&j.Digest); err != nil {
		state := "retry"
		next := now + 1
		if j.Attempts >= j.MaxAttempts {
			state = "failed"
			next = 0
		}
		res, updateErr := s.db.Exec(`
			UPDATE stamp_jobs
			SET state=?,lease_owner='',lease_until=0,next_attempt_at=?,last_error=?,updated_at=?
			WHERE id=? AND lease_owner=?`,
			state, next, SafeJobError(fmt.Errorf("load digest: %w", err)), now, j.ID, owner)
		if changed(res, updateErr) {
			s.publish(Change{Topic: ChangeStampJobs, LocationID: j.LocationID})
		}
		return nil, err
	}
	s.publish(Change{Topic: ChangeStampJobs, LocationID: j.LocationID})
	return &j, nil
}

func (s *Store) RenewStampJob(id int64, owner string, leaseUntil int64) (bool, error) {
	return s.RenewStampJobContext(context.Background(), id, owner, leaseUntil)
}

func (s *Store) RenewStampJobContext(ctx context.Context, id int64, owner string, leaseUntil int64) (bool, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE stamp_jobs SET lease_until=?,updated_at=?
		WHERE id=? AND state='running' AND lease_owner=? AND lease_until>?`,
		leaseUntil, now, id, owner, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) ExpireStampJobLeases(now int64) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`UPDATE locations SET tsa_status='fail' WHERE id IN (
			SELECT location_id FROM stamp_jobs
			WHERE state='running' AND lease_until<=? AND attempts>=max_attempts AND kind='tsa')`,
		`UPDATE locations SET ots_status='fail' WHERE id IN (
			SELECT location_id FROM stamp_jobs
			WHERE state='running' AND lease_until<=? AND attempts>=max_attempts AND kind='ots')`,
	} {
		if _, err := tx.Exec(q, now); err != nil {
			return 0, err
		}
	}
	res, err := tx.Exec(`
		UPDATE stamp_jobs
		SET state='failed',lease_owner='',lease_until=0,last_error='lease expired',updated_at=?
		WHERE state='running' AND lease_until<=? AND attempts>=max_attempts`, now, now)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if n > 0 {
		s.publish(Change{Topic: ChangeStampJobs, Bulk: true})
	}
	return int(n), nil
}

func (s *Store) CompleteStampJob(j *StampJob, status string, tsaTime *int64, tsaNonce ...[]byte) error {
	if j == nil {
		return fmt.Errorf("stamp job is required")
	}
	if j.Kind != "tsa" && j.Kind != "ots" {
		return fmt.Errorf("unknown stamp kind %q", j.Kind)
	}
	if j.Kind == "tsa" && tsaTime == nil {
		return fmt.Errorf("tsa completion requires timestamp")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	res, err := tx.Exec(`
		UPDATE stamp_jobs
		SET state='done',last_error='',lease_owner='',lease_until=0,updated_at=?
		WHERE id=? AND state='running' AND lease_owner=? AND lease_until>?`, now, j.ID, j.LeaseOwner, now)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrLeaseLost
	}
	switch j.Kind {
	case "tsa":
		var nonce []byte
		if len(tsaNonce) > 0 {
			nonce = tsaNonce[0]
		}
		if nonce == nil {
			nonce = []byte{}
		}
		_, err = tx.Exec(`UPDATE locations SET tsa_status=?,tsa_time=?,tsa_nonce=? WHERE id=?`,
			status, *tsaTime, nonce, j.LocationID)
	case "ots":
		_, err = tx.Exec(`UPDATE locations SET ots_status=? WHERE id=?`, status, j.LocationID)
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publish(Change{Topic: ChangeStampJobs, LocationID: j.LocationID})
	return nil
}

func (s *Store) StampJobLeaseValid(id int64, owner string, now int64) (bool, error) {
	var valid bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM stamp_jobs WHERE id=? AND state='running' AND lease_owner=? AND lease_until>?)`, id, owner, now).Scan(&valid)
	return valid, err
}

func (s *Store) FailStampJob(j *StampJob, cause error) (retry bool, err error) {
	now := time.Now().Unix()
	msg := SafeJobError(cause)
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var state, kind string
	var locationID int64
	err = tx.QueryRow(`
		UPDATE stamp_jobs
		SET state=CASE WHEN attempts<max_attempts THEN 'retry' ELSE 'failed' END,
			next_attempt_at=CASE WHEN attempts<max_attempts THEN ? + (1 << CASE
				WHEN attempts<1 THEN 0 WHEN attempts-1<8 THEN attempts-1 ELSE 8 END) ELSE 0 END,
			last_error=?,lease_owner='',lease_until=0,updated_at=?
		WHERE id=? AND state='running' AND lease_owner=? AND lease_until>?
		RETURNING state,kind,location_id`, now, msg, now, j.ID, j.LeaseOwner, now).Scan(&state, &kind, &locationID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrLeaseLost
		}
		return false, err
	}
	retry = state == "retry"
	if !retry {
		col := "tsa_status"
		if kind == "ots" {
			col = "ots_status"
		}
		if _, err := tx.Exec(`UPDATE locations SET `+col+`='fail' WHERE id=?`, locationID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.publish(Change{Topic: ChangeStampJobs, LocationID: locationID})
	return retry, nil
}

func SafeJobError(err error) string {
	return slogx.SafeError(err)
}

func (s *Store) RetryStampJobs(locationID int64, maxAttempts int) (int, error) {
	if maxAttempts < 1 {
		maxAttempts = 3
	}
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`
		UPDATE stamp_jobs
		SET state='queued',attempts=0,max_attempts=?,next_attempt_at=0,last_error='',lease_owner='',lease_until=0,updated_at=?
		WHERE location_id=? AND state!='running' AND (
			(kind='tsa' AND EXISTS(SELECT 1 FROM locations WHERE id=? AND tsa_status IN ('pending','fail')))
			OR
			(kind='ots' AND EXISTS(SELECT 1 FROM locations WHERE id=? AND ots_status IN ('pending','fail')))
		)`, maxAttempts, now, locationID, locationID, locationID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		if _, err := tx.Exec(`
			UPDATE locations SET
				tsa_status=CASE WHEN tsa_status='fail' AND EXISTS(
					SELECT 1 FROM stamp_jobs j WHERE j.location_id=locations.id AND j.kind='tsa' AND j.state='queued'
				) THEN 'pending' ELSE tsa_status END,
				ots_status=CASE WHEN ots_status='fail' AND EXISTS(
					SELECT 1 FROM stamp_jobs j WHERE j.location_id=locations.id AND j.kind='ots' AND j.state='queued'
				) THEN 'pending' ELSE ots_status END
			WHERE id=?`, locationID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if n > 0 {
		s.publish(Change{Topic: ChangeStampJobs, LocationID: locationID})
	}
	return int(n), nil
}

func (s *Store) RetryAllStampJobs(maxAttempts int) (int, error) {
	if maxAttempts < 1 {
		maxAttempts = 3
	}
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`
		UPDATE stamp_jobs
		SET state='queued',attempts=0,max_attempts=?,next_attempt_at=0,last_error='',lease_owner='',lease_until=0,updated_at=?
		WHERE state != 'running' AND (
			state IN ('failed','retry')
			OR (kind='tsa' AND EXISTS(
				SELECT 1 FROM locations l WHERE l.id=stamp_jobs.location_id AND l.tsa_status='fail'
			))
			OR (kind='ots' AND EXISTS(
				SELECT 1 FROM locations l WHERE l.id=stamp_jobs.location_id AND l.ots_status='fail'
			)))`, maxAttempts, now)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		if _, err := tx.Exec(`
			UPDATE locations SET
				tsa_status=CASE WHEN tsa_status='fail' AND EXISTS(
					SELECT 1 FROM stamp_jobs j WHERE j.location_id=locations.id AND j.kind='tsa' AND j.state='queued'
				) THEN 'pending' ELSE tsa_status END,
				ots_status=CASE WHEN ots_status='fail' AND EXISTS(
					SELECT 1 FROM stamp_jobs j WHERE j.location_id=locations.id AND j.kind='ots' AND j.state='queued'
				) THEN 'pending' ELSE ots_status END`); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if n > 0 {
		s.publish(Change{Topic: ChangeStampJobs, Bulk: true})
	}
	return int(n), nil
}

func (s *Store) JobStatus() (JobStatus, error) {
	return s.JobStatusContext(context.Background())
}

func (s *Store) JobStatusContext(ctx context.Context) (JobStatus, error) {
	var out JobStatus
	rows, err := s.db.QueryContext(ctx, `SELECT state,COUNT(*),MIN(updated_at) FROM stamp_jobs WHERE state!='done' GROUP BY state`)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var state string
		var n int
		var oldest sql.NullInt64
		if err := rows.Scan(&state, &n, &oldest); err != nil {
			return out, err
		}
		switch state {
		case "queued":
			out.Queued = n
		case "running":
			out.Running = n
		case "retry":
			out.Retry = n
		case "failed":
			out.Failed = n
		}
		if state != "failed" && oldest.Valid && (out.OldestQueued == 0 || oldest.Int64 < out.OldestQueued) {
			out.OldestQueued = oldest.Int64
		}
	}
	return out, rows.Err()
}

func (s *Store) ListStampJobs(limit int) ([]StampJob, error) {
	return s.ListStampJobsByState("", limit)
}

func (s *Store) ListStampJobsByState(state string, limit int) ([]StampJob, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	query := `
		SELECT id,location_id,kind,state,attempts,max_attempts,next_attempt_at,last_error,updated_at,lease_owner,lease_until,NULL
		FROM stamp_jobs WHERE state!='done'`
	var args []any
	if state != "" {
		switch state {
		case "queued", "running", "retry", "failed":
			query += ` AND state=?`
			args = append(args, state)
		default:
			return nil, ErrInvalidStampJobState
		}
	}
	query += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]StampJob, 0)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func (s *Store) ListLocationStampJobs(locationID int64) ([]StampJob, error) {
	rows, err := s.db.Query(`
		SELECT id,location_id,kind,state,attempts,max_attempts,next_attempt_at,last_error,updated_at,lease_owner,lease_until,NULL
		FROM stamp_jobs WHERE location_id=? ORDER BY kind`, locationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []StampJob
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func scanJob(row scannable) (*StampJob, error) {
	var j StampJob
	err := row.Scan(&j.ID, &j.LocationID, &j.Kind, &j.State, &j.Attempts,
		&j.MaxAttempts, &j.NextAttemptAt, &j.LastError, &j.UpdatedAt,
		&j.LeaseOwner, &j.LeaseUntil, &j.Digest)
	return &j, err
}
