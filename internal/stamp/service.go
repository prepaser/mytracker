package stamp

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/metrics"
	"github.com/prepaser/mytracker/internal/ots"
	"github.com/prepaser/mytracker/internal/proofverify"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/tsa"
	"github.com/prepaser/mytracker/internal/webhook"
	"github.com/prepaser/mytracker/internal/worker"
)

const (
	leaseDuration = 90 * time.Second
	leaseRenewal  = 15 * time.Second
)

type Service struct {
	cfg         *config.Manager
	store       *store.Store
	ctx         context.Context
	cancel      context.CancelFunc
	wake        *worker.Signal
	mu          sync.Mutex
	workers     []context.CancelFunc
	wg          sync.WaitGroup
	heartbeat   *worker.Heartbeat
	verifier    *proofverify.Service
	tsaStamp    func(context.Context, *config.Config, [32]byte) (*tsa.Result, error)
	otsStamp    func(context.Context, *config.Config, [32]byte) ([]byte, error)
	completeJob func(*store.StampJob, string, *int64, ...[]byte) error
}

func New(cfg *config.Manager, st *store.Store) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		cfg: cfg, store: st, ctx: ctx, cancel: cancel,
		wake: worker.NewSignal(), heartbeat: worker.NewHeartbeat(time.Now),
		tsaStamp: func(ctx context.Context, cfg *config.Config, digest [32]byte) (*tsa.Result, error) {
			return tsa.New(cfg.TSAURL).Stamp(ctx, digest)
		},
		otsStamp: func(ctx context.Context, cfg *config.Config, digest [32]byte) ([]byte, error) {
			return ots.New(cfg.OTSCalendars).Stamp(ctx, digest)
		},
		completeJob: st.CompleteStampJob,
	}
	snap := cfg.Snapshot()
	s.heartbeat.Beat()
	if err := st.RecoverStampJobs(snap.StampRetries); err != nil {
		slog.Error("recover stamp jobs", "err", err)
	}
	s.verifier = proofverify.New(cfg, st)
	s.resize(snap.StampWorkers)
	s.signal()
	s.refreshMetrics()
	return s
}

func (s *Service) Process(_ int64, _ [32]byte) {
	s.signal()
	s.refreshMetrics()
}

func (s *Service) Wake() { s.signal() }

func (s *Service) Reload() {
	s.resize(s.cfg.Snapshot().StampWorkers)
	s.signal()
}

func (s *Service) ReloadVerification() {
	if s.verifier != nil {
		s.verifier.Reload()
	}
}

func (s *Service) resize(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.workers) < n {
		ctx, cancel := context.WithCancel(s.ctx)
		s.workers = append(s.workers, cancel)
		owner, err := config.RandToken(16)
		if err != nil {
			owner = fmt.Sprintf("worker-%d", time.Now().UnixNano())
		}
		s.wg.Add(1)
		go s.worker(ctx, owner)
	}
	for len(s.workers) > n {
		last := len(s.workers) - 1
		s.workers[last]()
		s.workers = s.workers[:last]
	}
}

func (s *Service) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.ShutdownContext(ctx)
}

func (s *Service) ShutdownContext(ctx context.Context) error {
	s.cancel()
	var verifierErr error
	if s.verifier != nil {
		verifierErr = s.verifier.ShutdownContext(ctx)
	}
	if err := worker.Wait(ctx, &s.wg); err != nil {
		return err
	}
	return verifierErr
}

func (s *Service) signal() {
	s.wake.Notify()
}

func (s *Service) worker(ctx context.Context, owner string) {
	defer s.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		s.heartbeat.Beat()
		if !s.runReady(ctx, owner) {
			select {
			case <-ctx.Done():
				return
			case <-s.wake.C():
			case <-t.C:
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (s *Service) runReady(ctx context.Context, owner string) bool {
	worked := false
	for ctx.Err() == nil {
		now := time.Now()
		expired, err := s.store.ExpireStampJobLeases(now.Unix())
		if err != nil {
			slog.Error("expire stamp leases", "err", err)
			return worked
		}
		if expired > 0 {
			metrics.StampLease("expired", expired)
		}
		reclaimable, err := s.store.ExpiredLeaseCount(now.Unix())
		if err != nil {
			slog.Error("inspect stamp leases", "err", err)
			return worked
		}
		j, err := s.store.ClaimStampJob(now.Unix(), owner, now.Add(leaseDuration).Unix())
		if err != nil {
			slog.Error("claim stamp job", "err", err)
			return worked
		}
		if j == nil {
			return worked
		}
		if reclaimable > 0 {
			metrics.StampLease("reclaimed", 1)
		}
		worked = true
		s.handleJob(ctx, j)
		s.refreshMetrics()
	}
	return worked
}

func (s *Service) refreshMetrics() {
	status, err := s.store.JobStatus()
	if err == nil {
		metrics.SetStampJobs(status.Queued, status.Running, status.Retry, status.Failed, status.OldestQueued)
	}
}

func (s *Service) handleJob(ctx context.Context, j *store.StampJob) {
	start := time.Now()
	jobCtx, stopLease := worker.RenewLease(ctx, worker.LeaseOptions{
		Interval:      leaseRenewal,
		LeaseUntil:    time.Unix(j.LeaseUntil, 0),
		LeaseDuration: leaseDuration,
		Renew: func(ctx context.Context, now time.Time) (bool, error) {
			return s.store.RenewStampJobContext(ctx, j.ID, j.LeaseOwner, now.Add(leaseDuration).Unix())
		},
		OnError: func(err error) {
			slog.Error("renew stamp lease", "job_id", j.ID, "err", err)
		},
		OnLost: func() { metrics.StampLease("lost", 1) },
		OnRenewed: func() {
			s.heartbeat.Beat()
			metrics.StampLease("renewed", 1)
		},
	})
	err := s.executeJob(jobCtx, j)
	stopLease()
	s.heartbeat.Beat()
	metrics.ObserveStamp(j.Kind, time.Since(start))
	if err == nil {
		metrics.StampJob("completed", j.Kind)
		return
	}
	if errors.Is(err, store.ErrLeaseLost) {
		metrics.StampLease("lost", 1)
		return
	}
	retry, updateErr := s.store.FailStampJob(j, err)
	if errors.Is(updateErr, store.ErrLeaseLost) {
		metrics.StampLease("lost", 1)
		return
	}
	if updateErr != nil {
		slog.Error("update stamp job", "job_id", j.ID, "err", updateErr)
		return
	}
	if retry {
		metrics.StampJob("retry", j.Kind)
		s.signal()
		return
	}
	s.notifyFail(j.Kind, j.LocationID, err)
	metrics.StampJob("failed", j.Kind)
}

func (s *Service) executeJob(ctx context.Context, j *store.StampJob) error {
	return worker.Guard("stamp worker", func() error {
		switch j.Kind {
		case "tsa":
			return s.doTSA(ctx, j)
		case "ots":
			return s.doOTS(ctx, j)
		default:
			return fmt.Errorf("unknown stamp kind %q", j.Kind)
		}
	})
}

func (s *Service) doTSA(ctx context.Context, j *store.StampJob) error {
	cfg := s.cfg.Snapshot()
	var digest [32]byte
	copy(digest[:], j.Digest)
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := s.tsaStamp(reqCtx, cfg, digest)
	if err != nil {
		return err
	}
	ts := res.GenTime.Unix()
	unlockProofs, err := s.store.LockProofMutationContext(ctx)
	if err != nil {
		return err
	}
	defer unlockProofs()
	path := TSAPath(cfg.ProofsDir(), j.LocationID)
	valid, err := s.store.StampJobLeaseValid(j.ID, j.LeaseOwner, time.Now().Unix())
	if err != nil {
		return err
	}
	if !valid {
		return store.ErrLeaseLost
	}
	replacement, err := stageProof(path, res.Token)
	if err != nil {
		return err
	}
	if err := s.completeStampJob(j, "ok", &ts, res.Nonce); err != nil {
		return errors.Join(err, replacement.rollback())
	}
	if err := replacement.commit(); err != nil {
		slog.Warn("cleanup previous TSA proof", "location_id", j.LocationID, "err", err)
	}
	s.triggerVerify(j.LocationID, "tsa", "stamp")
	metrics.LastStampSuccess("tsa")
	return nil
}

func (s *Service) doOTS(ctx context.Context, j *store.StampJob) error {
	cfg := s.cfg.Snapshot()
	var digest [32]byte
	copy(digest[:], j.Digest)
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	proof, err := s.otsStamp(reqCtx, cfg, digest)
	if err != nil {
		return err
	}
	unlockProofs, err := s.store.LockProofMutationContext(ctx)
	if err != nil {
		return err
	}
	defer unlockProofs()
	path := filepath.Join(cfg.ProofsDir(), fmt.Sprintf("%d.ots", j.LocationID))
	valid, err := s.store.StampJobLeaseValid(j.ID, j.LeaseOwner, time.Now().Unix())
	if err != nil {
		return err
	}
	if !valid {
		return store.ErrLeaseLost
	}
	replacement, err := stageProof(path, proof)
	if err != nil {
		return err
	}
	if err := s.completeStampJob(j, "submitted", nil); err != nil {
		return errors.Join(err, replacement.rollback())
	}
	if err := replacement.commit(); err != nil {
		slog.Warn("cleanup previous OTS proof", "location_id", j.LocationID, "err", err)
	}
	s.triggerVerify(j.LocationID, "ots", "stamp")
	metrics.LastStampSuccess("ots")
	return nil
}

func (s *Service) notifyFail(kind string, id int64, err error) {
	safeErr := store.SafeJobError(err)
	slog.Error("stamp failed", "kind", kind, "location_id", id, "err", safeErr)
	metrics.IncStampFail(kind)
	cfg := s.cfg.Snapshot()
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	webhook.Notify(ctx, cfg.WebhookURL, cfg.WebhookSecret, "stamp_fail", map[string]any{
		"kind": kind, "id": id, "error": safeErr,
	})
}

func (s *Service) QueueDepth() int {
	st, err := s.store.JobStatus()
	if err != nil {
		return 0
	}
	return st.Queued + st.Retry + st.Running
}

func (s *Service) LeaseSeconds() int { return int(leaseDuration.Seconds()) }

func (s *Service) Healthy() bool {
	return s.heartbeat.Healthy(30 * time.Second)
}

func (s *Service) Retry(id int64) error {
	_, err := s.RetryResult(id)
	return err
}

func (s *Service) RetryResult(id int64) (int, error) {
	if _, err := s.store.Get(id); err != nil {
		return 0, err
	}
	n, err := s.store.RetryStampJobs(id, s.cfg.Snapshot().StampRetries)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("no retryable stamp jobs")
	}
	metrics.StampJob("manual_retry", "all")
	s.refreshMetrics()
	s.signal()
	return n, nil
}

func (s *Service) RetryAll() (int, error) {
	n, err := s.store.RetryAllStampJobs(s.cfg.Snapshot().StampRetries)
	if err != nil {
		return n, err
	}
	if n > 0 {
		metrics.StampJob("manual_retry", "all")
		s.signal()
	}
	s.refreshMetrics()
	return n, nil
}

type UpgradeResult struct {
	Processed int     `json:"processed"`
	Upgraded  int     `json:"upgraded"`
	Errors    int     `json:"errors"`
	Remaining int     `json:"remaining"`
	Pending   int     `json:"pending"`
	LastID    int64   `json:"last_id,omitempty"`
	FailedIDs []int64 `json:"failed_ids,omitempty"`
}

type UpgradeCursor struct {
	AfterID  int64
	RetryIDs []int64
}

func (s *Service) UpgradeBatch(ctx context.Context, limit int, cursor UpgradeCursor) (out UpgradeResult, err error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	cfg := s.cfg.Snapshot()
	after := int64(0)
	if cursor.AfterID > 0 {
		after = cursor.AfterID
	}
	out.LastID = after
	retryIDs := cursor.RetryIDs
	if len(retryIDs) > 1000 {
		retryIDs = retryIDs[:1000]
	}
	processedIDs := make(map[int64]struct{}, len(retryIDs)+limit)
	failedSet := make(map[int64]struct{}, len(retryIDs))
	addFailed := func(id int64) {
		if _, ok := failedSet[id]; ok {
			return
		}
		failedSet[id] = struct{}{}
		out.FailedIDs = append(out.FailedIDs, id)
	}
	preserveRetries := func() {
		for _, id := range retryIDs {
			if _, processed := processedIDs[id]; !processed {
				addFailed(id)
			}
		}
	}
	defer func() {
		if err != nil {
			preserveRetries()
		}
	}()
	if err := ctx.Err(); err != nil {
		return out, err
	}
	retrySet := make(map[int64]struct{}, len(retryIDs))
	for _, id := range retryIDs {
		retrySet[id] = struct{}{}
	}
	collectCandidates := func(start int64) ([]store.Location, bool, error) {
		cursor := start
		locs := make([]store.Location, 0, limit+1)
		for {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			candidates, err := s.store.OTSUpgradeCandidatesContext(ctx, cursor, 1001)
			if err != nil {
				return nil, false, err
			}
			if len(candidates) == 0 {
				return locs, false, nil
			}
			for _, loc := range candidates {
				cursor = loc.ID
				if _, retry := retrySet[loc.ID]; retry {
					continue
				}
				locs = append(locs, loc)
				if len(locs) > limit {
					return locs, true, nil
				}
			}
			if len(candidates) < 1001 {
				return locs, false, nil
			}
		}
	}
	locs, more, err := collectCandidates(after)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out, ctxErr
		}
		return out, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return out, ctxErr
	}
	if len(locs) == 0 && after > 0 {
		locs, more, err = collectCandidates(0)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return out, ctxErr
			}
			return out, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out, ctxErr
		}
	}
	if more {
		locs = locs[:limit]
	}
	budget := limit
	successes := 0
	process := func(loc store.Location, advance bool) {
		if advance {
			out.LastID = loc.ID
		}
		out.Processed++
		confirmed, err := s.upgradeLocation(ctx, cfg, loc)
		if err != nil {
			out.Errors++
			addFailed(loc.ID)
			return
		}
		successes++
		if confirmed {
			out.Upgraded++
		}
	}
	if len(locs) > budget {
		locs = locs[:budget]
	}
	for _, loc := range locs {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		processedIDs[loc.ID] = struct{}{}
		process(loc, true)
		budget--
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	for _, id := range retryIDs {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if _, ok := processedIDs[id]; ok {
			continue
		}
		processedIDs[id] = struct{}{}
		if budget == 0 {
			addFailed(id)
			continue
		}
		loc, err := s.store.GetContext(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			addFailed(id)
			out.Errors++
			continue
		}
		if loc.OtsStatus != "submitted" {
			continue
		}
		process(*loc, false)
		budget--
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return out, ctxErr
	}
	out.Pending, err = s.store.CountSubmittedOTSContext(ctx)
	if err != nil {
		return out, err
	}
	if more {
		out.Remaining = out.Pending
	}
	if out.Errors > 0 && successes == 0 {
		return out, fmt.Errorf("all OTS upgrades failed")
	}
	return out, nil
}

func (s *Service) upgradeLocation(ctx context.Context, cfg *config.Config, loc store.Location) (bool, error) {
	proof, err := ots.LoadProof(cfg.ProofsDir(), loc.ID)
	if err != nil {
		return false, err
	}
	var digest [32]byte
	copy(digest[:], loc.Digest)
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	newProof, upgraded, err := ots.Upgrade(reqCtx, proof, digest, cfg.OTSCalendars)
	cancel()
	if err != nil {
		return false, err
	}
	var unlockProofs func()
	verificationQueued := false
	if upgraded {
		unlockProofs, err = s.store.LockProofMutationContext(ctx)
		if err != nil {
			return false, err
		}
		defer unlockProofs()
		current, err := s.store.Get(loc.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if current.OtsStatus != "submitted" || !bytes.Equal(current.Digest, loc.Digest) {
			return false, nil
		}
		if err := ots.SaveProof(cfg.ProofsDir(), loc.ID, newProof); err != nil {
			return false, err
		}
		s.triggerVerify(loc.ID, "ots", "upgrade")
		verificationQueued = true
	}
	if ots.Pending(newProof) || !ots.HasBitcoinAnchor(newProof) {
		return false, nil
	}
	if err := s.store.UpdateOTS(loc.ID, "confirmed"); err != nil {
		if errors.Is(err, sql.ErrNoRows) && upgraded {
			_ = os.Remove(filepath.Join(cfg.ProofsDir(), fmt.Sprintf("%d.ots", loc.ID)))
			return false, nil
		}
		return false, err
	}
	if !verificationQueued {
		s.triggerVerify(loc.ID, "ots", "upgrade")
	}
	metrics.LastStampSuccess("ots")
	return true, nil
}

func (s *Service) triggerVerify(id int64, kind, trigger string) {
	if s.verifier != nil {
		s.verifier.Trigger(id, kind, trigger)
	}
}

func (s *Service) EnqueueVerification(id int64) ([]store.VerificationJob, error) {
	return s.verifier.EnqueueAll(id, "manual")
}

func (s *Service) LatestVerification(id int64) (*proofverify.View, error) {
	return s.verifier.Latest(id)
}

func (s *Service) VerificationHistory(id, before int64, limit int) ([]store.VerificationRun, error) {
	return s.verifier.History(id, before, limit)
}

func (s *Service) VerificationSummary() (store.VerificationStats, int, error) {
	return s.verifier.Summary()
}

func (s *Service) VerificationSummaryContext(ctx context.Context) (store.VerificationStats, int, error) {
	return s.verifier.SummaryContext(ctx)
}

func (s *Service) VerificationPolicyStale() (int, error) {
	return s.verifier.PolicyStaleCount()
}

func (s *Service) VerificationPolicyStaleContext(ctx context.Context) (int, error) {
	return s.verifier.PolicyStaleCountContext(ctx)
}

func (s *Service) VerificationHealthy() bool {
	return s.verifier != nil && s.verifier.Healthy()
}

func (s *Service) VerificationLeaseSeconds() int {
	if s.verifier == nil {
		return 0
	}
	return s.verifier.LeaseSeconds()
}

func (s *Service) TriggerVerification(id int64) {
	s.triggerVerify(id, "tsa", "import")
	s.triggerVerify(id, "ots", "import")
}

func TSAPath(dir string, id int64) string {
	return filepath.Join(dir, fmt.Sprintf("%d.tsr", id))
}

func (s *Service) completeStampJob(j *store.StampJob, status string, tsaTime *int64, tsaNonce ...[]byte) error {
	if s.completeJob != nil {
		return s.completeJob(j, status, tsaTime, tsaNonce...)
	}
	return s.store.CompleteStampJob(j, status, tsaTime, tsaNonce...)
}

type proofReplacement struct {
	path     string
	backup   string
	hadProof bool
}

func stageProof(path string, data []byte) (*proofReplacement, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".new-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	cleanupTmp := true
	defer func() {
		_ = tmp.Close()
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		return nil, err
	}
	if _, err := tmp.Write(data); err != nil {
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	backupFile, err := os.CreateTemp(dir, "."+filepath.Base(path)+".old-*")
	if err != nil {
		return nil, err
	}
	backupPath := backupFile.Name()
	if err := backupFile.Close(); err != nil {
		_ = os.Remove(backupPath)
		return nil, err
	}
	if err := os.Remove(backupPath); err != nil {
		return nil, err
	}
	replacement := &proofReplacement{path: path, backup: backupPath}
	if err := os.Link(path, backupPath); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		replacement.backup = ""
	} else {
		replacement.hadProof = true
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return nil, errors.Join(err, replacement.restore())
	}
	cleanupTmp = false
	if err := syncProofDir(dir); err != nil {
		return nil, errors.Join(err, replacement.rollback())
	}
	return replacement, nil
}

func (r *proofReplacement) rollback() error {
	if r == nil {
		return nil
	}
	if err := os.Remove(r.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return r.restore()
}

func (r *proofReplacement) restore() error {
	if r.backup == "" {
		return syncProofDir(filepath.Dir(r.path))
	}
	if !r.hadProof {
		_ = os.Remove(r.backup)
		return syncProofDir(filepath.Dir(r.path))
	}
	if err := os.Rename(r.backup, r.path); err != nil {
		return err
	}
	r.backup = ""
	return syncProofDir(filepath.Dir(r.path))
}

func (r *proofReplacement) commit() error {
	if r == nil || r.backup == "" {
		return nil
	}
	if err := os.Remove(r.backup); err != nil && !os.IsNotExist(err) {
		return err
	}
	r.backup = ""
	return syncProofDir(filepath.Dir(r.path))
}

func syncProofDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
