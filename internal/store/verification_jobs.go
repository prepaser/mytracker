package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const verificationJobColumns = `id,location_id,kind,trigger,state,generation,claimed_generation,
	attempts,max_attempts,next_attempt_at,last_error,lease_owner,lease_until,created_at,updated_at`

type VerificationJob struct {
	ID                int64  `json:"id"`
	LocationID        int64  `json:"location_id"`
	Kind              string `json:"kind"`
	Trigger           string `json:"trigger"`
	State             string `json:"state"`
	Generation        int64  `json:"generation"`
	ClaimedGeneration int64  `json:"-"`
	Attempts          int    `json:"attempts"`
	MaxAttempts       int    `json:"max_attempts"`
	NextAttemptAt     int64  `json:"next_attempt_at,omitempty"`
	LastError         string `json:"last_error,omitempty"`
	LeaseOwner        string `json:"-"`
	LeaseUntil        int64  `json:"lease_until,omitempty"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

type VerificationJobStatus struct {
	Queued       int   `json:"queued"`
	Running      int   `json:"running"`
	Retry        int   `json:"retry"`
	Failed       int   `json:"failed"`
	OldestQueued int64 `json:"oldest_queued,omitempty"`
}

type VerificationReconcileItem struct {
	LocationID int64
	Kind       string
	JobState   string
	JobUpdated int64
	RunID      int64
	Result     string
	CheckedAt  int64
	ErrorCode  string
	ProofHash  []byte
	PolicyHash []byte
	Engine     int
}

func (s *Store) EnqueueVerification(locationID int64, kind, trigger string, now int64) (*VerificationJob, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	job, coalesced, err := enqueueVerificationTx(tx, locationID, kind, trigger, now)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	s.publish(Change{Topic: ChangeVerification, LocationID: locationID})
	return job, coalesced, nil
}

func (s *Store) EnqueueVerificationPair(locationID int64, trigger string, now int64) ([]VerificationJob, []bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE proof_verification_jobs SET id=id WHERE location_id=?`, locationID); err != nil {
		return nil, nil, err
	}
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM locations WHERE id=?`, locationID).Scan(&exists); err != nil {
		return nil, nil, err
	}
	jobs := make([]VerificationJob, 0, 2)
	coalesced := make([]bool, 0, 2)
	for _, kind := range []string{"tsa", "ots"} {
		job, merged, err := enqueueVerificationTx(tx, locationID, kind, trigger, now)
		if err != nil {
			return nil, nil, err
		}
		jobs = append(jobs, *job)
		coalesced = append(coalesced, merged)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	s.publish(Change{Topic: ChangeVerification, LocationID: locationID})
	return jobs, coalesced, nil
}

func enqueueVerificationTx(tx *sql.Tx, locationID int64, kind, trigger string, now int64) (*VerificationJob, bool, error) {
	if kind != "tsa" && kind != "ots" {
		return nil, false, fmt.Errorf("invalid verification kind")
	}
	if !validVerificationTrigger(trigger) {
		trigger = "scheduled"
	}
	if _, err := tx.Exec(`UPDATE proof_verification_jobs SET id=id WHERE location_id=? AND kind=?`, locationID, kind); err != nil {
		return nil, false, err
	}
	var previousState string
	if err := tx.QueryRow(`SELECT state FROM proof_verification_jobs WHERE location_id=? AND kind=?`, locationID, kind).Scan(&previousState); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	row := tx.QueryRow(`
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
			updated_at=excluded.updated_at
		RETURNING `+verificationJobColumns,
		locationID, kind, trigger, now, now)
	job, err := scanVerificationJob(row)
	if err != nil {
		return nil, false, err
	}
	return job, activeVerificationState(previousState), nil
}

func activeVerificationState(state string) bool {
	return state == "queued" || state == "running" || state == "retry"
}

func (s *Store) RecoverVerificationJobs(now int64) (int, error) {
	res, err := s.db.Exec(`
		UPDATE proof_verification_jobs
		SET state=CASE WHEN generation>claimed_generation OR attempts<max_attempts THEN 'queued' ELSE 'failed' END,
			attempts=CASE WHEN generation>claimed_generation THEN 0 ELSE attempts END,
			next_attempt_at=0,lease_owner='',lease_until=0,updated_at=?
		WHERE state='running'`, now)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *Store) ClaimVerificationJob(now int64, owner string, leaseUntil int64) (*VerificationJob, error) {
	row := s.db.QueryRow(`
		UPDATE proof_verification_jobs
		SET state='running',attempts=CASE WHEN generation>claimed_generation THEN 1 ELSE attempts+1 END,
			claimed_generation=generation,
			lease_owner=?,lease_until=?,updated_at=?
		WHERE id=(
			SELECT id FROM proof_verification_jobs
			WHERE (generation>claimed_generation OR attempts<max_attempts) AND (
				(state IN ('queued','retry') AND next_attempt_at<=?)
				OR (state='running' AND lease_until<=?)
			)
			ORDER BY CASE WHEN state='running' THEN 0 ELSE 1 END,next_attempt_at,id LIMIT 1
		)
		RETURNING `+verificationJobColumns,
		owner, leaseUntil, now, now, now)
	job, err := scanVerificationJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err == nil {
		s.publish(Change{Topic: ChangeVerification, LocationID: job.LocationID})
	}
	return job, err
}

func (s *Store) RenewVerificationJob(id int64, owner string, leaseUntil, now int64) (bool, error) {
	return s.RenewVerificationJobContext(context.Background(), id, owner, leaseUntil, now)
}

func (s *Store) RenewVerificationJobContext(ctx context.Context, id int64, owner string, leaseUntil, now int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE proof_verification_jobs SET lease_until=?,updated_at=?
		WHERE id=? AND state='running' AND lease_owner=? AND lease_until>?`,
		leaseUntil, now, id, owner, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) FinishVerificationJob(job *VerificationJob, run *VerificationRun, retry bool, cause error, now int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	state := "done"
	next := int64(0)
	lastError := ""
	if retry {
		state = "failed"
		lastError = SafeJobError(cause)
		if job.Attempts < job.MaxAttempts {
			state = "retry"
			next = now + int64((time.Second << min(job.Attempts-1, 8)).Seconds())
		}
	}
	res, err := tx.Exec(`
		UPDATE proof_verification_jobs
		SET state=CASE WHEN generation>claimed_generation THEN 'queued' ELSE ? END,
			attempts=CASE WHEN generation>claimed_generation THEN 0 ELSE attempts END,
			next_attempt_at=CASE WHEN generation>claimed_generation THEN 0 ELSE ? END,
			last_error=CASE WHEN generation>claimed_generation THEN '' ELSE ? END,
			lease_owner='',lease_until=0,updated_at=?
		WHERE id=? AND state='running' AND lease_owner=? AND lease_until>?`,
		state, next, lastError, now, job.ID, job.LeaseOwner, now)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrLeaseLost
	}
	if run != nil {
		if err := insertVerification(tx, run); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publish(Change{Topic: ChangeVerification, LocationID: job.LocationID})
	return nil
}

func (s *Store) ExpireVerificationJobLeases(now int64) (int, error) {
	res, err := s.db.Exec(`
		UPDATE proof_verification_jobs
		SET state=CASE WHEN generation>claimed_generation THEN 'queued' ELSE 'failed' END,
			attempts=CASE WHEN generation>claimed_generation THEN 0 ELSE attempts END,
			next_attempt_at=0,last_error=CASE WHEN generation>claimed_generation THEN '' ELSE 'lease expired' END,
			lease_owner='',lease_until=0,updated_at=?
		WHERE state='running' AND lease_until<=? AND attempts>=max_attempts`,
		now, now)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err == nil && n > 0 {
		s.publish(Change{Topic: ChangeVerification, Bulk: true})
	}
	return int(n), err
}

func (s *Store) VerificationExpiredLeaseCount(now int64) (int, error) {
	return s.VerificationExpiredLeaseCountContext(context.Background(), now)
}

func (s *Store) VerificationExpiredLeaseCountContext(ctx context.Context, now int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM proof_verification_jobs
		WHERE state='running' AND lease_until<=?`, now).Scan(&n)
	return n, err
}

func (s *Store) VerificationJobStatus() (VerificationJobStatus, error) {
	return s.VerificationJobStatusContext(context.Background())
}

func (s *Store) VerificationJobStatusContext(ctx context.Context) (VerificationJobStatus, error) {
	var out VerificationJobStatus
	rows, err := s.db.QueryContext(ctx, `
		SELECT state,COUNT(*),MIN(updated_at)
		FROM proof_verification_jobs WHERE state!='done' GROUP BY state`)
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

func (s *Store) ListVerificationJobs(state string, limit int) ([]VerificationJob, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	q := `SELECT ` + verificationJobColumns + ` FROM proof_verification_jobs WHERE 1=1`
	var args []any
	if state != "" {
		switch state {
		case "queued", "running", "retry", "done", "failed":
		default:
			return nil, ErrInvalidVerificationJobState
		}
		q += ` AND state=?`
		args = append(args, state)
	}
	q += ` ORDER BY updated_at DESC,id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var jobs []VerificationJob
	for rows.Next() {
		job, err := scanVerificationJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, *job)
	}
	return jobs, rows.Err()
}

func (s *Store) ListLocationVerificationJobs(locationID int64) ([]VerificationJob, error) {
	rows, err := s.db.Query(`
		SELECT `+verificationJobColumns+` FROM proof_verification_jobs
		WHERE location_id=? ORDER BY kind`, locationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var jobs []VerificationJob
	for rows.Next() {
		job, err := scanVerificationJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, *job)
	}
	return jobs, rows.Err()
}

func (s *Store) ActiveVerificationJobCount() (int, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM proof_verification_jobs
		WHERE state IN ('queued','running','retry')`).Scan(&n)
	return n, err
}

func (s *Store) IterateVerificationReconcile(batch int, fn func([]VerificationReconcileItem) error) error {
	return s.IterateVerificationReconcileAfter(0, "", batch, fn)
}

func (s *Store) IterateVerificationReconcileAfter(afterID int64, afterKind string, batch int, fn func([]VerificationReconcileItem) error) error {
	if batch < 1 {
		batch = 500
	}
	afterOrd := -1
	switch afterKind {
	case "tsa":
		afterOrd = 0
	case "ots":
		afterOrd = 1
	}
	rows, err := s.db.Query(`
		WITH kinds(kind,ord) AS (VALUES('tsa',0),('ots',1))
		SELECT l.id,k.kind,j.state,j.updated_at,v.id,v.result,v.checked_at,
			v.error_code,v.proof_sha256,v.policy_sha256,v.engine_version
		FROM locations l CROSS JOIN kinds k
		LEFT JOIN proof_verification_jobs j ON j.location_id=l.id AND j.kind=k.kind
		LEFT JOIN proof_verification_runs v ON v.id=(
			SELECT MAX(v2.id) FROM proof_verification_runs v2
			WHERE v2.location_id=l.id AND v2.kind=k.kind)
		WHERE l.id>? OR (l.id=? AND k.ord>?)
		ORDER BY l.id,k.ord`, afterID, afterID, afterOrd)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	items := make([]VerificationReconcileItem, 0, batch)
	for rows.Next() {
		var item VerificationReconcileItem
		var jobState, result sql.NullString
		var jobUpdated, runID, checkedAt sql.NullInt64
		var errorCode sql.NullString
		var proofHash, policyHash []byte
		var engine sql.NullInt64
		if err := rows.Scan(&item.LocationID, &item.Kind, &jobState, &jobUpdated, &runID,
			&result, &checkedAt, &errorCode, &proofHash, &policyHash, &engine); err != nil {
			return err
		}
		item.JobState, item.JobUpdated = jobState.String, jobUpdated.Int64
		item.RunID, item.Result, item.CheckedAt = runID.Int64, result.String, checkedAt.Int64
		item.ErrorCode = errorCode.String
		item.ProofHash, item.PolicyHash, item.Engine = proofHash, policyHash, int(engine.Int64)
		items = append(items, item)
		if len(items) == batch {
			if err := fn(items); err != nil {
				return err
			}
			items = items[:0]
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(items) > 0 {
		return fn(items)
	}
	return nil
}

func scanVerificationJob(row interface{ Scan(...any) error }) (*VerificationJob, error) {
	var job VerificationJob
	err := row.Scan(&job.ID, &job.LocationID, &job.Kind, &job.Trigger, &job.State,
		&job.Generation, &job.ClaimedGeneration, &job.Attempts, &job.MaxAttempts,
		&job.NextAttemptAt, &job.LastError, &job.LeaseOwner, &job.LeaseUntil,
		&job.CreatedAt, &job.UpdatedAt)
	return &job, err
}

func validVerificationTrigger(trigger string) bool {
	switch trigger {
	case "stamp", "upgrade", "scheduled", "manual", "import":
		return true
	default:
		return false
	}
}
