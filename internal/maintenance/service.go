package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/metrics"
	"github.com/prepaser/mytracker/internal/retention"
	"github.com/prepaser/mytracker/internal/stamp"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/worker"
)

const (
	leaseDuration  = 90 * time.Second
	leaseRenewal   = 15 * time.Second
	otsBatchBudget = time.Minute
	otsYieldDelay  = time.Second
)

var intervals = map[string]time.Duration{
	"retention":       24 * time.Hour,
	"ots_upgrade":     20 * time.Minute,
	"proof_gc":        30 * time.Second,
	"session_cleanup": time.Hour,
}

type Service struct {
	cfg       *config.Manager
	store     *store.Store
	stamp     *stamp.Service
	ctx       context.Context
	cancel    context.CancelFunc
	wake      *worker.Signal
	reload    *worker.Signal
	wg        sync.WaitGroup
	heartbeat *worker.Heartbeat
	accepting atomic.Bool
	now       func() time.Time
	run       func(context.Context, string) (map[string]any, error)
}

func New(cfg *config.Manager, st *store.Store, stampSvc *stamp.Service) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		cfg: cfg, store: st, stamp: stampSvc, ctx: ctx, cancel: cancel,
		wake: worker.NewSignal(), reload: worker.NewSignal(), now: time.Now,
	}
	s.heartbeat = worker.NewHeartbeat(func() time.Time { return s.now() })
	s.run = s.runKind
	s.accepting.Store(true)
	now := s.now().Unix()
	s.beat()
	if n, err := st.RecoverMaintenanceJobs(now); err != nil {
		slog.Error("recover maintenance jobs", "err", err)
	} else if n > 0 {
		metrics.MaintenanceLease("recovered", n)
	}
	s.reconcile()
	s.wg.Add(2)
	go s.worker()
	go s.scheduler()
	s.signal()
	return s
}

func (s *Service) Enqueue(kind, trigger string) (*store.MaintenanceJob, bool, error) {
	if !s.accepting.Load() {
		return nil, false, context.Canceled
	}
	job, coalesced, err := s.store.EnqueueMaintenance(kind, trigger, s.now().Unix())
	if err != nil {
		return nil, false, err
	}
	event := "enqueued"
	if coalesced {
		event = "coalesced"
	}
	metrics.MaintenanceEvent(kind, event)
	s.signal()
	return job, coalesced, nil
}

func (s *Service) Retry(id int64) (*store.MaintenanceJob, error) {
	if !s.accepting.Load() {
		return nil, context.Canceled
	}
	job, err := s.store.RetryMaintenanceJob(id, s.now().Unix())
	if err != nil {
		return nil, err
	}
	metrics.MaintenanceEvent(job.Kind, "manual_retry")
	s.signal()
	return job, nil
}

func (s *Service) Reload() {
	s.reload.Notify()
}

func (s *Service) Healthy() bool {
	return s.heartbeat.Healthy(30 * time.Second)
}

func (s *Service) beat() {
	s.heartbeat.Beat()
	metrics.SetMaintenanceWorkerHealthy(true)
}

func (s *Service) LeaseSeconds() int { return int(leaseDuration.Seconds()) }

func (s *Service) ShutdownContext(ctx context.Context) error {
	s.accepting.Store(false)
	s.cancel()
	if err := worker.Wait(ctx, &s.wg); err == nil {
		metrics.SetMaintenanceWorkerHealthy(false)
		return nil
	} else {
		return err
	}
}

func (s *Service) signal() {
	s.wake.Notify()
}

func (s *Service) worker() {
	defer s.wg.Done()
	owner, err := config.RandToken(16)
	if err != nil {
		owner = fmt.Sprintf("maintenance-%d", s.now().UnixNano())
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		s.beat()
		if s.runReady(owner) {
			continue
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake.C():
		case <-ticker.C:
		}
	}
}

func (s *Service) runReady(owner string) bool {
	worked := false
	for s.ctx.Err() == nil {
		now := s.now()
		expired, err := s.store.ExpireMaintenanceLeases(now.Unix())
		if err != nil {
			slog.Error("expire maintenance leases", "err", err)
			return worked
		}
		if expired > 0 {
			metrics.MaintenanceLease("expired", expired)
		}
		job, err := s.store.ClaimMaintenanceJob(now.Unix(), owner, now.Add(leaseDuration).Unix())
		if err != nil {
			slog.Error("claim maintenance job", "err", err)
			return worked
		}
		if job == nil {
			return worked
		}
		worked = true
		s.handleJob(job)
	}
	return worked
}

func (s *Service) handleJob(job *store.MaintenanceJob) {
	start := s.now()
	jobCtx, stopLease := worker.RenewLease(s.ctx, worker.LeaseOptions{
		Interval:      leaseRenewal,
		Now:           s.now,
		LeaseUntil:    time.Unix(job.LeaseUntil, 0),
		LeaseDuration: leaseDuration,
		Renew: func(ctx context.Context, now time.Time) (bool, error) {
			return s.store.RenewMaintenanceJobContext(ctx,
				job.ID, job.LeaseOwner, now.Add(leaseDuration).Unix(), now.Unix())
		},
		OnError: func(err error) {
			slog.Error("renew maintenance lease", "job_id", job.ID, "err", err)
		},
		OnLost: func() { metrics.MaintenanceLease("lost", 1) },
		OnRenewed: func() {
			s.beat()
			metrics.MaintenanceLease("renewed", 1)
		},
	})
	summary, execErr := s.execute(jobCtx, job.Kind)
	stopLease()
	s.beat()
	finished := s.now()
	result := "success"
	yielded, _ := summary["yielded"].(bool)
	if execErr != nil {
		result = "error"
		if errors.Is(execErr, context.Canceled) {
			result = "canceled"
		}
	}
	body, _ := json.Marshal(summary)
	run := &store.MaintenanceRun{
		Result: result, StartedAt: start.Unix(), FinishedAt: finished.Unix(),
		DurationMS: finished.Sub(start).Milliseconds(), Summary: body,
	}
	if execErr != nil {
		run.Error = execErr.Error()
	}
	remaining, hasRemaining := intValue(summary["remaining"])
	followUp := execErr == nil && !yielded && (job.Kind == "ots_upgrade" || job.Kind == "proof_gc") && hasRemaining && remaining > 0
	next := finished.Add(s.interval(job.Kind)).Unix()
	if followUp {
		next = finished.Unix()
	} else if yielded {
		next = finished.Add(otsYieldDelay).Unix()
	}
	err := s.store.FinishMaintenanceJob(job, run, execErr != nil, finished.Unix(), next)
	if errors.Is(err, store.ErrLeaseLost) {
		metrics.MaintenanceLease("lost", 1)
		return
	}
	if err != nil {
		slog.Error("finish maintenance job", "job_id", job.ID, "err", err)
		return
	}
	metrics.MaintenanceRun(job.Kind, result, finished.Sub(start))
	if execErr != nil {
		metrics.MaintenanceEvent(job.Kind, "retry")
	} else if yielded {
		metrics.MaintenanceEvent(job.Kind, "yielded")
	} else {
		metrics.MaintenanceEvent(job.Kind, "completed")
	}
	if followUp {
		s.reconcile()
	}
	s.signal()
}

func (s *Service) execute(ctx context.Context, kind string) (map[string]any, error) {
	return worker.GuardValue("maintenance worker", func() (map[string]any, error) {
		return s.run(ctx, kind)
	})
}

func (s *Service) runKind(ctx context.Context, kind string) (map[string]any, error) {
	switch kind {
	case "backup":
		path, err := backup.CreateContext(ctx, s.cfg.Snapshot(), s.store)
		if err != nil {
			metrics.Backup("fail")
			return nil, err
		}
		metrics.Backup("ok")
		return map[string]any{"file": filepath.Base(path)}, nil
	case "retention":
		n, err := retention.RunContext(ctx, s.cfg, s.store)
		return map[string]any{"deleted": n}, err
	case "ots_upgrade":
		cursor, failed := s.otsState()
		return runOTSBatch(ctx, func(batchCtx context.Context) (stamp.UpgradeResult, error) {
			return s.stamp.UpgradeBatch(batchCtx, 100, stamp.UpgradeCursor{
				AfterID: cursor, RetryIDs: failed,
			})
		})
	case "proof_gc":
		n, err := s.store.ProcessProofGCContext(ctx, s.cfg.Snapshot().ProofsDir(), 200)
		status, statusErr := s.store.ProofGCStatusContext(ctx)
		ready, readyErr := s.store.ProofGCReadyCountContext(ctx, s.now().Unix())
		if err == nil {
			err = statusErr
		}
		if err == nil {
			err = readyErr
		}
		if n > 0 {
			metrics.ProofGC("deleted", n)
		}
		return map[string]any{"deleted": n, "remaining": ready, "pending": status.Pending}, err
	case "session_cleanup":
		cfg := s.cfg.Snapshot()
		n, err := s.store.CleanupSessionsContext(ctx, s.now().Unix(), int64(cfg.SessionIdleDuration().Seconds()))
		return map[string]any{"deleted": n}, err
	default:
		return nil, fmt.Errorf("unknown maintenance kind")
	}
}

func runOTSBatch(ctx context.Context, run func(context.Context) (stamp.UpgradeResult, error)) (map[string]any, error) {
	return runOTSBatchWithBudget(ctx, otsBatchBudget, run)
}

func runOTSBatchWithBudget(ctx context.Context, budget time.Duration, run func(context.Context) (stamp.UpgradeResult, error)) (map[string]any, error) {
	batchCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	out, err := run(batchCtx)
	body, _ := json.Marshal(out)
	var summary map[string]any
	_ = json.Unmarshal(body, &summary)
	if err != nil && batchCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		summary["yielded"] = true
		delete(summary, "pending")
		delete(summary, "remaining")
		return summary, nil
	}
	return summary, err
}

func (s *Service) otsState() (int64, []int64) {
	run, err := s.store.LatestMaintenanceRun("ots_upgrade")
	if err != nil {
		return 0, nil
	}
	var summary struct {
		LastID    int64   `json:"last_id"`
		FailedIDs []int64 `json:"failed_ids"`
	}
	if json.Unmarshal(run.Summary, &summary) != nil {
		return 0, nil
	}
	if len(summary.FailedIDs) > 1000 {
		summary.FailedIDs = summary.FailedIDs[:1000]
	}
	return summary.LastID, summary.FailedIDs
}

func (s *Service) scheduler() {
	defer s.wg.Done()
	s.reconcile()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.reconcile()
		case <-s.reload.C():
			s.reconcile()
		}
	}
}

func (s *Service) reconcile() {
	now := s.now()
	jobs, err := s.store.ListMaintenanceJobs("", "")
	if err != nil {
		slog.Error("list maintenance jobs", "err", err)
		return
	}
	existing := make(map[string]store.MaintenanceJob, len(jobs))
	for _, job := range jobs {
		existing[job.Kind] = job
	}
	for _, kind := range []string{"backup", "retention", "ots_upgrade", "proof_gc", "session_cleanup"} {
		enabled := kind != "retention" || s.cfg.Snapshot().RetentionDays > 0
		next := s.initialNext(kind, now)
		if job, ok := existing[kind]; ok {
			next = job.NextRunAt
			if kind == "backup" {
				baseline := job.LastSuccessAt
				if job.State == "failed" {
					baseline = job.LastFinishedAt
				}
				if (job.State == "scheduled" || job.State == "failed") && baseline > 0 {
					if baseline > now.Unix() {
						next = now.Unix()
					} else {
						next = time.Unix(baseline, 0).Add(s.interval(kind)).Unix()
					}
				}
			}
			if next == 0 && job.State == "scheduled" {
				next = now.Unix()
			}
			if next == 0 && job.State == "failed" {
				next = now.Add(s.interval(kind)).Unix()
			}
		}
		if _, err := s.store.EnsureMaintenanceJob(kind, enabled, next, now.Unix()); err != nil {
			slog.Error("schedule maintenance job", "kind", kind, "err", err)
		}
	}
	s.refreshMetrics()
	s.signal()
}

func (s *Service) initialNext(kind string, now time.Time) int64 {
	switch kind {
	case "backup":
		_, modified, err := store.LatestBackup(s.cfg.Snapshot().BackupsDir())
		if err == nil {
			if modified.After(now) {
				return now.Unix()
			}
			return modified.Add(s.interval(kind)).Unix()
		}
		return now.Unix()
	case "ots_upgrade":
		return now.Add(s.interval(kind)).Unix()
	default:
		return now.Unix()
	}
}

func (s *Service) interval(kind string) time.Duration {
	if kind == "backup" {
		d, err := s.cfg.Snapshot().BackupDuration()
		if err == nil {
			return d
		}
		return 24 * time.Hour
	}
	return intervals[kind]
}

func (s *Service) refreshMetrics() {
	jobs, err := s.store.ListMaintenanceJobs("", "")
	if err != nil {
		return
	}
	for _, job := range jobs {
		metrics.SetMaintenanceJob(job.Kind, job.State, job.NextRunAt, job.LastSuccessAt)
	}
}

func intValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}
