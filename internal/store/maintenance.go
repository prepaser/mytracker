package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const maintenanceJobColumns = `id,kind,trigger,state,generation,claimed_generation,attempts,max_attempts,
	next_run_at,last_error,lease_owner,lease_until,last_started_at,last_finished_at,last_success_at,created_at,updated_at`

type MaintenanceJob struct {
	ID                int64  `json:"id"`
	Kind              string `json:"kind"`
	Trigger           string `json:"trigger"`
	State             string `json:"state"`
	Generation        int64  `json:"generation"`
	ClaimedGeneration int64  `json:"-"`
	Attempts          int    `json:"attempts"`
	MaxAttempts       int    `json:"max_attempts"`
	NextRunAt         int64  `json:"next_run_at,omitempty"`
	LastError         string `json:"last_error,omitempty"`
	LeaseOwner        string `json:"-"`
	LeaseUntil        int64  `json:"lease_until,omitempty"`
	LastStartedAt     int64  `json:"last_started_at,omitempty"`
	LastFinishedAt    int64  `json:"last_finished_at,omitempty"`
	LastSuccessAt     int64  `json:"last_success_at,omitempty"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

type MaintenanceRun struct {
	ID         int64           `json:"id"`
	JobID      int64           `json:"job_id"`
	Kind       string          `json:"kind"`
	Trigger    string          `json:"trigger"`
	Result     string          `json:"result"`
	StartedAt  int64           `json:"started_at"`
	FinishedAt int64           `json:"finished_at"`
	DurationMS int64           `json:"duration_ms"`
	Summary    json.RawMessage `json:"summary"`
	Error      string          `json:"error,omitempty"`
}

type MaintenanceStatus struct {
	Scheduled int `json:"scheduled"`
	Queued    int `json:"queued"`
	Running   int `json:"running"`
	Retry     int `json:"retry"`
	Failed    int `json:"failed"`
	Disabled  int `json:"disabled"`
}

func ValidMaintenanceKind(kind string) bool {
	switch kind {
	case "backup", "retention", "ots_upgrade", "proof_gc", "session_cleanup":
		return true
	default:
		return false
	}
}

func (s *Store) EnsureMaintenanceJob(kind string, enabled bool, next, now int64) (*MaintenanceJob, error) {
	if !ValidMaintenanceKind(kind) {
		return nil, fmt.Errorf("invalid maintenance kind")
	}
	state := "scheduled"
	if !enabled {
		state = "disabled"
	} else if next <= now {
		state = "queued"
	}
	row := s.db.QueryRow(`
		INSERT INTO maintenance_jobs (
			kind,trigger,state,generation,max_attempts,next_run_at,created_at,updated_at
		) VALUES (?,'scheduled',?,CASE WHEN ?='queued' THEN 1 ELSE 0 END,5,?,?,?)
		ON CONFLICT(kind) DO UPDATE SET
			state=CASE
				WHEN maintenance_jobs.state='running' THEN 'running'
				WHEN excluded.state='disabled' THEN 'disabled'
				WHEN maintenance_jobs.state='disabled' THEN excluded.state
				WHEN maintenance_jobs.state IN ('scheduled','failed') AND excluded.state='queued' THEN 'queued'
				ELSE maintenance_jobs.state
			END,
			trigger=CASE
				WHEN maintenance_jobs.state IN ('scheduled','failed','disabled') AND excluded.state='queued' THEN 'scheduled'
				ELSE maintenance_jobs.trigger
			END,
			generation=CASE
				WHEN maintenance_jobs.state!='running'
					AND maintenance_jobs.state IN ('scheduled','failed','disabled')
					AND excluded.state='queued'
				THEN maintenance_jobs.generation+1
				ELSE maintenance_jobs.generation
			END,
			attempts=CASE
				WHEN maintenance_jobs.state IN ('scheduled','failed','disabled') AND excluded.state='queued' THEN 0
				ELSE maintenance_jobs.attempts
			END,
			next_run_at=CASE
				WHEN maintenance_jobs.state IN ('running','queued','retry') THEN maintenance_jobs.next_run_at
				ELSE excluded.next_run_at
			END,
			last_error=CASE
				WHEN maintenance_jobs.state IN ('scheduled','failed','disabled') AND excluded.state='queued' THEN ''
				ELSE maintenance_jobs.last_error
			END,
			updated_at=excluded.updated_at
		WHERE maintenance_jobs.state!='running' AND (
			maintenance_jobs.state IS NOT CASE
				WHEN excluded.state='disabled' THEN 'disabled'
				WHEN maintenance_jobs.state='disabled' THEN excluded.state
				WHEN maintenance_jobs.state IN ('scheduled','failed') AND excluded.state='queued' THEN 'queued'
				ELSE maintenance_jobs.state END
			OR maintenance_jobs.generation IS NOT CASE
				WHEN maintenance_jobs.state IN ('scheduled','failed','disabled') AND excluded.state='queued'
				THEN maintenance_jobs.generation+1 ELSE maintenance_jobs.generation END
			OR maintenance_jobs.attempts IS NOT CASE
				WHEN maintenance_jobs.state IN ('scheduled','failed','disabled') AND excluded.state='queued'
				THEN 0 ELSE maintenance_jobs.attempts END
			OR maintenance_jobs.next_run_at IS NOT CASE
				WHEN maintenance_jobs.state IN ('queued','retry') THEN maintenance_jobs.next_run_at
				ELSE excluded.next_run_at END
			OR maintenance_jobs.last_error IS NOT CASE
				WHEN maintenance_jobs.state IN ('scheduled','failed','disabled') AND excluded.state='queued'
				THEN '' ELSE maintenance_jobs.last_error END
		)
		RETURNING `+maintenanceJobColumns,
		kind, state, state, next, now, now)
	job, err := scanMaintenanceJob(row)
	if !errors.Is(err, sql.ErrNoRows) {
		return job, err
	}
	return scanMaintenanceJob(s.db.QueryRow(`SELECT `+maintenanceJobColumns+` FROM maintenance_jobs WHERE kind=?`, kind))
}

func (s *Store) EnqueueMaintenance(kind, trigger string, now int64) (*MaintenanceJob, bool, error) {
	if !ValidMaintenanceKind(kind) {
		return nil, false, fmt.Errorf("invalid maintenance kind")
	}
	if trigger != "manual" {
		trigger = "scheduled"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE maintenance_jobs SET id=id WHERE kind=?`, kind); err != nil {
		return nil, false, err
	}
	var previousState string
	if err := tx.QueryRow(`SELECT state FROM maintenance_jobs WHERE kind=?`, kind).Scan(&previousState); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	row := tx.QueryRow(`
		INSERT INTO maintenance_jobs (
			kind,trigger,state,generation,max_attempts,next_run_at,created_at,updated_at
		) VALUES (?,?,'queued',1,5,0,?,?)
		ON CONFLICT(kind) DO UPDATE SET
			trigger=excluded.trigger,generation=maintenance_jobs.generation+1,
			state=CASE WHEN maintenance_jobs.state='running' THEN 'running' ELSE 'queued' END,
			attempts=CASE WHEN maintenance_jobs.state='running' THEN maintenance_jobs.attempts ELSE 0 END,
			next_run_at=CASE WHEN maintenance_jobs.state='running' THEN maintenance_jobs.next_run_at ELSE 0 END,
			last_error=CASE WHEN maintenance_jobs.state='running' THEN maintenance_jobs.last_error ELSE '' END,
			updated_at=excluded.updated_at
		RETURNING `+maintenanceJobColumns, kind, trigger, now, now)
	job, err := scanMaintenanceJob(row)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	s.publish(Change{Topic: ChangeMaintenance})
	return job, activeMaintenanceState(previousState), nil
}

func activeMaintenanceState(state string) bool {
	return state == "queued" || state == "running" || state == "retry"
}

func (s *Store) RetryMaintenanceJob(id, now int64) (*MaintenanceJob, error) {
	row := s.db.QueryRow(`
		UPDATE maintenance_jobs SET trigger='manual',state='queued',generation=generation+1,
			attempts=0,next_run_at=0,last_error='',lease_owner='',lease_until=0,updated_at=?
		WHERE id=? AND state='failed'
		RETURNING `+maintenanceJobColumns, now, id)
	job, err := scanMaintenanceJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("maintenance job is not failed")
	}
	if err == nil {
		s.publish(Change{Topic: ChangeMaintenance})
	}
	return job, err
}

func (s *Store) RecoverMaintenanceJobs(now int64) (int, error) {
	res, err := s.db.Exec(`
		UPDATE maintenance_jobs
		SET state=CASE WHEN generation>claimed_generation OR attempts<max_attempts THEN 'queued' ELSE 'failed' END,
			attempts=CASE WHEN generation>claimed_generation THEN 0 ELSE attempts END,
			next_run_at=0,lease_owner='',lease_until=0,updated_at=?
		WHERE state='running'`, now)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *Store) ClaimMaintenanceJob(now int64, owner string, leaseUntil int64) (*MaintenanceJob, error) {
	row := s.db.QueryRow(`
		UPDATE maintenance_jobs
		SET state='running',attempts=CASE WHEN generation>claimed_generation THEN 1 ELSE attempts+1 END,
			claimed_generation=generation,
			lease_owner=?,lease_until=?,last_started_at=?,updated_at=?
		WHERE id=(
			SELECT id FROM maintenance_jobs
			WHERE (generation>claimed_generation OR attempts<max_attempts) AND (
				(state IN ('queued','retry') AND next_run_at<=?)
				OR (state='running' AND lease_until<=?)
			)
			ORDER BY CASE WHEN state='running' THEN 0 ELSE 1 END,next_run_at,id LIMIT 1
		)
		RETURNING `+maintenanceJobColumns, owner, leaseUntil, now, now, now, now)
	job, err := scanMaintenanceJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err == nil {
		s.publish(Change{Topic: ChangeMaintenance})
	}
	return job, err
}

func (s *Store) RenewMaintenanceJob(id int64, owner string, leaseUntil, now int64) (bool, error) {
	return s.RenewMaintenanceJobContext(context.Background(), id, owner, leaseUntil, now)
}

func (s *Store) RenewMaintenanceJobContext(ctx context.Context, id int64, owner string, leaseUntil, now int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE maintenance_jobs SET lease_until=?,updated_at=?
		WHERE id=? AND state='running' AND lease_owner=? AND lease_until>?`,
		leaseUntil, now, id, owner, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) FinishMaintenanceJob(job *MaintenanceJob, run *MaintenanceRun, retry bool, now, nextScheduled int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	state := "scheduled"
	next := nextScheduled
	lastError := ""
	if retry {
		state = "failed"
		lastError = SafeJobError(errors.New(run.Error))
		if job.Attempts < job.MaxAttempts {
			state = "retry"
			next = now + int64((time.Second << min(job.Attempts-1, 8)).Seconds())
		}
	}
	res, err := tx.Exec(`
		UPDATE maintenance_jobs SET
			state=CASE WHEN generation>claimed_generation THEN 'queued' ELSE ? END,
			attempts=CASE WHEN generation>claimed_generation OR ?='scheduled' THEN 0 ELSE attempts END,
			next_run_at=CASE WHEN generation>claimed_generation THEN 0 ELSE ? END,
			last_error=CASE WHEN generation>claimed_generation THEN '' ELSE ? END,
			lease_owner='',lease_until=0,last_finished_at=?,
			last_success_at=CASE WHEN ?='scheduled' THEN ? ELSE last_success_at END,updated_at=?
		WHERE id=? AND state='running' AND lease_owner=? AND lease_until>?`,
		state, state, next, lastError, now, state, now, now, job.ID, job.LeaseOwner, now)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrLeaseLost
	}
	run.JobID = job.ID
	run.Kind = job.Kind
	run.Trigger = job.Trigger
	run.Error = SafeJobError(errors.New(run.Error))
	if _, err := tx.Exec(`
		INSERT INTO maintenance_runs (
			job_id,kind,trigger,result,started_at,finished_at,duration_ms,summary,error
		) VALUES (?,?,?,?,?,?,?,?,?)`,
		run.JobID, run.Kind, run.Trigger, run.Result, run.StartedAt, run.FinishedAt,
		run.DurationMS, string(run.Summary), run.Error); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM maintenance_runs WHERE finished_at<?`, now-30*24*60*60); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publish(Change{Topic: ChangeMaintenance})
	return nil
}

func (s *Store) ExpireMaintenanceLeases(now int64) (int, error) {
	res, err := s.db.Exec(`
		UPDATE maintenance_jobs SET
			state=CASE WHEN generation>claimed_generation THEN 'queued' ELSE 'failed' END,
			attempts=CASE WHEN generation>claimed_generation THEN 0 ELSE attempts END,
			next_run_at=0,
			last_error=CASE WHEN generation>claimed_generation THEN '' ELSE 'lease expired' END,
			lease_owner='',lease_until=0,last_finished_at=?,updated_at=?
		WHERE state='running' AND lease_until<=? AND attempts>=max_attempts`, now, now, now)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err == nil && n > 0 {
		s.publish(Change{Topic: ChangeMaintenance})
	}
	return int(n), err
}

func (s *Store) MaintenanceExpiredLeaseCount(now int64) (int, error) {
	return s.MaintenanceExpiredLeaseCountContext(context.Background(), now)
}

func (s *Store) MaintenanceExpiredLeaseCountContext(ctx context.Context, now int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM maintenance_jobs WHERE state='running' AND lease_until<=?`, now).Scan(&n)
	return n, err
}

func (s *Store) MaintenanceOverdueCount(now int64) (int, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM maintenance_jobs
		WHERE state IN ('scheduled','failed') AND next_run_at>0 AND next_run_at<=?`, now).Scan(&n)
	return n, err
}

func (s *Store) MaintenanceStatus() (MaintenanceStatus, error) {
	return s.MaintenanceStatusContext(context.Background())
}

func (s *Store) MaintenanceStatusContext(ctx context.Context) (MaintenanceStatus, error) {
	var out MaintenanceStatus
	rows, err := s.db.QueryContext(ctx, `SELECT state,COUNT(*) FROM maintenance_jobs GROUP BY state`)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return out, err
		}
		switch state {
		case "scheduled":
			out.Scheduled = n
		case "queued":
			out.Queued = n
		case "running":
			out.Running = n
		case "retry":
			out.Retry = n
		case "failed":
			out.Failed = n
		case "disabled":
			out.Disabled = n
		}
	}
	return out, rows.Err()
}

func (s *Store) ListMaintenanceJobs(kind, state string) ([]MaintenanceJob, error) {
	return s.ListMaintenanceJobsContext(context.Background(), kind, state)
}

func (s *Store) ListMaintenanceJobsContext(ctx context.Context, kind, state string) ([]MaintenanceJob, error) {
	q := `SELECT ` + maintenanceJobColumns + ` FROM maintenance_jobs WHERE 1=1`
	var args []any
	if kind != "" {
		if !ValidMaintenanceKind(kind) {
			return nil, ErrInvalidMaintenanceKind
		}
		q += ` AND kind=?`
		args = append(args, kind)
	}
	if state != "" {
		switch state {
		case "scheduled", "queued", "running", "retry", "failed", "disabled":
		default:
			return nil, ErrInvalidMaintenanceState
		}
		q += ` AND state=?`
		args = append(args, state)
	}
	q += ` ORDER BY kind`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	jobs := make([]MaintenanceJob, 0)
	for rows.Next() {
		job, err := scanMaintenanceJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, *job)
	}
	return jobs, rows.Err()
}

func (s *Store) ListMaintenanceRuns(before int64, limit int) ([]MaintenanceRun, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 201 {
		limit = 201
	}
	q := `SELECT id,job_id,kind,trigger,result,started_at,finished_at,duration_ms,summary,error
		FROM maintenance_runs`
	var args []any
	if before > 0 {
		q += ` WHERE id<?`
		args = append(args, before)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	runs := make([]MaintenanceRun, 0)
	for rows.Next() {
		var run MaintenanceRun
		var summary string
		if err := rows.Scan(&run.ID, &run.JobID, &run.Kind, &run.Trigger, &run.Result,
			&run.StartedAt, &run.FinishedAt, &run.DurationMS, &summary, &run.Error); err != nil {
			return nil, err
		}
		run.Summary = json.RawMessage(summary)
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) LatestMaintenanceRun(kind string) (*MaintenanceRun, error) {
	if !ValidMaintenanceKind(kind) {
		return nil, fmt.Errorf("invalid maintenance kind")
	}
	var run MaintenanceRun
	var summary string
	err := s.db.QueryRow(`SELECT id,job_id,kind,trigger,result,started_at,finished_at,duration_ms,summary,error
		FROM maintenance_runs WHERE kind=? ORDER BY id DESC LIMIT 1`, kind).Scan(
		&run.ID, &run.JobID, &run.Kind, &run.Trigger, &run.Result, &run.StartedAt,
		&run.FinishedAt, &run.DurationMS, &summary, &run.Error)
	if err != nil {
		return nil, err
	}
	run.Summary = json.RawMessage(summary)
	return &run, nil
}

func scanMaintenanceJob(row interface{ Scan(...any) error }) (*MaintenanceJob, error) {
	var job MaintenanceJob
	err := row.Scan(&job.ID, &job.Kind, &job.Trigger, &job.State, &job.Generation,
		&job.ClaimedGeneration, &job.Attempts, &job.MaxAttempts, &job.NextRunAt,
		&job.LastError, &job.LeaseOwner, &job.LeaseUntil, &job.LastStartedAt,
		&job.LastFinishedAt, &job.LastSuccessAt, &job.CreatedAt, &job.UpdatedAt)
	return &job, err
}
