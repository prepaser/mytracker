package proofverify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/metrics"
	"github.com/prepaser/mytracker/internal/ots"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/tsa"
	"github.com/prepaser/mytracker/internal/verification"
	"github.com/prepaser/mytracker/internal/worker"
)

const (
	verifyInterval    = 24 * time.Hour
	reconcileInterval = 5 * time.Minute
	historyKeep       = 90 * 24 * time.Hour
	historyCleanup    = 24 * time.Hour
	leaseDuration     = 90 * time.Second
	leaseRenewal      = 15 * time.Second
	maxBacklog        = 500
	engineVersion     = 7
	maxArtifactBytes  = 8 << 20
)

var errArtifactTooLarge = errors.New("verification artifact too large")

type verificationInput struct {
	cfg      *config.Config
	location *store.Location
	proof    []byte
	readErr  error
}

type Service struct {
	cfg            *config.Manager
	store          *store.Store
	ctx            context.Context
	cancel         context.CancelFunc
	wake           *worker.Signal
	reconcileWake  *worker.Signal
	wg             sync.WaitGroup
	heartbeat      *worker.Heartbeat
	accepting      atomic.Bool
	cleanupAt      atomic.Int64
	reconcileMu    sync.Mutex
	reconcileID    int64
	reconcileKind  string
	reconcileEpoch uint64
	verifyFn       func(context.Context, *store.Location, [32]byte, bool, string, string) *store.VerificationRun
}

type View struct {
	ID               int64                   `json:"id"`
	Digest           string                  `json:"digest"`
	DigestRecomputed string                  `json:"digest_recomputed"`
	PayloadOK        bool                    `json:"payload_ok"`
	Overall          string                  `json:"overall"`
	TSA              *store.VerificationRun  `json:"tsa,omitempty"`
	OTS              *store.VerificationRun  `json:"ots,omitempty"`
	TSAStale         bool                    `json:"tsa_stale"`
	OTSStale         bool                    `json:"ots_stale"`
	CheckedAt        int64                   `json:"checked_at,omitempty"`
	TrustPolicy      TrustPolicyView         `json:"trust_policy"`
	Jobs             []store.VerificationJob `json:"jobs"`
}

type TrustPolicyView struct {
	EngineVersion       int  `json:"engine_version"`
	TSARootsConfigured  bool `json:"tsa_roots_configured"`
	TSAPolicyConfigured bool `json:"tsa_policy_configured"`
	OTSMinConfirmations int  `json:"ots_min_confirmations"`
	OTSRequiredSources  int  `json:"ots_required_sources"`
}

func New(cfg *config.Manager, st *store.Store) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		cfg: cfg, store: st, ctx: ctx, cancel: cancel,
		wake: worker.NewSignal(), reconcileWake: worker.NewSignal(),
		heartbeat: worker.NewHeartbeat(time.Now),
	}
	s.accepting.Store(true)
	s.beat()
	if n, err := st.RecoverVerificationJobs(time.Now().Unix()); err != nil {
		slog.Error("recover proof verification jobs", "err", err)
	} else if n > 0 {
		metrics.VerificationLease("recovered", n)
	}
	s.refreshMetrics()
	for range 2 {
		s.wg.Add(1)
		owner, err := config.RandToken(16)
		if err != nil {
			owner = fmt.Sprintf("verify-%d", time.Now().UnixNano())
		}
		go s.worker(owner)
	}
	s.wg.Add(1)
	go s.scheduler()
	s.signal()
	return s
}

func (s *Service) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.ShutdownContext(ctx)
}

func (s *Service) ShutdownContext(ctx context.Context) error {
	s.accepting.Store(false)
	s.cancel()
	if err := worker.Wait(ctx, &s.wg); err == nil {
		metrics.SetVerificationWorkerHealthy(false)
		return nil
	} else {
		return err
	}
}

func (s *Service) Reload() {
	s.reconcileMu.Lock()
	s.reconcileID, s.reconcileKind = 0, ""
	s.reconcileEpoch++
	s.reconcileMu.Unlock()
	s.reconcileWake.Notify()
}

func (s *Service) Trigger(id int64, kind, trigger string) {
	if _, _, err := s.Enqueue(id, kind, trigger); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("enqueue proof verification", "location_id", id, "kind", kind, "err", err)
	}
}

func (s *Service) Enqueue(id int64, kind, trigger string) (*store.VerificationJob, bool, error) {
	if !s.accepting.Load() {
		return nil, false, context.Canceled
	}
	job, coalesced, err := s.store.EnqueueVerification(id, kind, trigger, time.Now().Unix())
	if err != nil {
		return nil, false, err
	}
	result := "enqueued"
	if coalesced {
		result = "coalesced"
	}
	metrics.VerificationJob(result, kind)
	s.signal()
	s.refreshMetrics()
	return job, coalesced, nil
}

func (s *Service) EnqueueAll(id int64, trigger string) ([]store.VerificationJob, error) {
	if !s.accepting.Load() {
		return nil, context.Canceled
	}
	jobs, coalesced, err := s.store.EnqueueVerificationPair(id, trigger, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	for i, kind := range []string{"tsa", "ots"} {
		result := "enqueued"
		if coalesced[i] {
			result = "coalesced"
		}
		metrics.VerificationJob(result, kind)
	}
	s.signal()
	s.refreshMetrics()
	return jobs, nil
}

func (s *Service) signal() {
	s.wake.Notify()
}

func (s *Service) Latest(id int64) (*View, error) {
	loc, err := s.store.Get(id)
	if err != nil {
		return nil, err
	}
	stored, recomputed, payloadOK, err := digestMatches(loc)
	if err != nil {
		copy(stored[:], loc.Digest)
		payloadOK = false
	}
	cfg := s.cfg.Snapshot()
	out := &View{
		ID: id, Digest: hex.EncodeToString(stored[:]),
		DigestRecomputed: hex.EncodeToString(recomputed[:]), PayloadOK: payloadOK,
		TrustPolicy: TrustPolicyView{
			EngineVersion: engineVersion, TSARootsConfigured: cfg.TSATrustRoots != "",
			TSAPolicyConfigured: len(cfg.TSAPolicyOIDs) > 0,
			OTSMinConfirmations: cfg.OTSConfirmations, OTSRequiredSources: 2,
		},
	}
	out.TSA, err = latestOrNil(s.store, id, "tsa")
	if err != nil {
		return nil, err
	}
	out.OTS, err = latestOrNil(s.store, id, "ots")
	if err != nil {
		return nil, err
	}
	out.Jobs, err = s.store.ListLocationVerificationJobs(id)
	if err != nil {
		return nil, err
	}
	out.TSAStale = stale(out.TSA, filepath.Join(cfg.ProofsDir(), fmt.Sprintf("%d.tsr", id)),
		policyHash(cfg, "tsa")) || verificationPending(out.TSA, out.Jobs, "tsa")
	out.OTSStale = stale(out.OTS, filepath.Join(cfg.ProofsDir(), fmt.Sprintf("%d.ots", id)),
		policyHash(cfg, "ots")) || verificationPending(out.OTS, out.Jobs, "ots")
	out.CheckedAt = max(checkedAt(out.TSA), checkedAt(out.OTS))
	out.Overall = overall(out.TSA, out.OTS, out.TSAStale, out.OTSStale, payloadOK)
	return out, nil
}

func (s *Service) PolicyStaleCount() (int, error) {
	return s.PolicyStaleCountContext(context.Background())
}

func (s *Service) PolicyStaleCountContext(ctx context.Context) (int, error) {
	cfg := s.cfg.Snapshot()
	n, err := s.store.VerificationPolicyStaleCountContext(ctx, engineVersion,
		policyHash(cfg, "tsa"), policyHash(cfg, "ots"))
	if err == nil {
		metrics.SetVerificationPolicyStale(n)
	}
	return n, err
}

func (s *Service) History(id, before int64, limit int) ([]store.VerificationRun, error) {
	if _, err := s.store.Get(id); err != nil {
		return nil, err
	}
	return s.store.VerificationHistory(id, before, limit)
}

func (s *Service) Summary() (store.VerificationStats, int, error) {
	return s.SummaryContext(context.Background())
}

func (s *Service) SummaryContext(ctx context.Context) (store.VerificationStats, int, error) {
	stats, err := s.store.VerificationStatsContext(ctx)
	if err != nil {
		return stats, 0, err
	}
	cfg := s.cfg.Snapshot()
	stale, err := s.store.VerificationStaleCountContext(ctx, time.Now().Add(-verifyInterval).Unix(),
		engineVersion, policyHash(cfg, "tsa"), policyHash(cfg, "ots"))
	if err == nil {
		metrics.SetVerificationStale(stale)
	}
	return stats, stale, err
}

func latestOrNil(st *store.Store, id int64, kind string) (*store.VerificationRun, error) {
	run, err := st.LatestVerification(id, kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return run, err
}

func (s *Service) Healthy() bool {
	return s.heartbeat.Healthy(30 * time.Second)
}

func (s *Service) beat() {
	s.heartbeat.Beat()
	metrics.SetVerificationWorkerHealthy(true)
}

func (s *Service) LeaseSeconds() int { return int(leaseDuration.Seconds()) }

func (s *Service) JobStatus() (store.VerificationJobStatus, error) {
	return s.store.VerificationJobStatus()
}

func (s *Service) worker(owner string) {
	defer s.wg.Done()
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
		now := time.Now()
		expired, err := s.store.ExpireVerificationJobLeases(now.Unix())
		if err != nil {
			slog.Error("expire proof verification leases", "err", err)
			return worked
		}
		if expired > 0 {
			metrics.VerificationLease("expired", expired)
			s.refreshMetrics()
		}
		reclaimable, err := s.store.VerificationExpiredLeaseCount(now.Unix())
		if err != nil {
			slog.Error("inspect proof verification leases", "err", err)
			return worked
		}
		job, err := s.store.ClaimVerificationJob(now.Unix(), owner, now.Add(leaseDuration).Unix())
		if err != nil {
			slog.Error("claim proof verification job", "err", err)
			return worked
		}
		if job == nil {
			return worked
		}
		s.refreshMetrics()
		if reclaimable > 0 {
			metrics.VerificationLease("reclaimed", 1)
		}
		worked = true
		s.handleJob(job)
		s.refreshMetrics()
	}
	return worked
}

func (s *Service) handleJob(job *store.VerificationJob) {
	start := time.Now()
	jobCtx, stopLease := worker.RenewLease(s.ctx, worker.LeaseOptions{
		Interval:      leaseRenewal,
		LeaseUntil:    time.Unix(job.LeaseUntil, 0),
		LeaseDuration: leaseDuration,
		Renew: func(ctx context.Context, now time.Time) (bool, error) {
			return s.store.RenewVerificationJobContext(ctx, job.ID, job.LeaseOwner,
				now.Add(leaseDuration).Unix(), now.Unix())
		},
		OnError: func(err error) {
			slog.Error("renew proof verification lease", "job_id", job.ID, "err", err)
		},
		OnLost: func() { metrics.VerificationLease("lost", 1) },
		OnRenewed: func() {
			s.beat()
			metrics.VerificationLease("renewed", 1)
		},
	})
	run, execErr := s.executeJob(jobCtx, job)
	ctxErr := jobCtx.Err()
	stopLease()
	s.beat()
	if execErr == nil && ctxErr != nil {
		execErr = ctxErr
		run = nil
	}
	if run == nil && execErr != nil {
		policy := []byte(nil)
		if s.cfg != nil {
			policy = policyHash(s.cfg.Snapshot(), job.Kind)
		}
		run = &store.VerificationRun{
			LocationID: job.LocationID, Kind: job.Kind, Trigger: job.Trigger,
			Result: "error", CheckedAt: time.Now().Unix(),
			EngineVersion: engineVersion, PolicySHA256: policy,
		}
	}
	retry := execErr != nil || run != nil && run.Result == "error"
	cause := execErr
	if cause == nil && run != nil {
		cause = errors.New(run.ErrorCode)
	}
	err := s.store.FinishVerificationJob(job, run, retry, cause, time.Now().Unix())
	if errors.Is(err, store.ErrLeaseLost) {
		metrics.VerificationLease("lost", 1)
		return
	}
	if err != nil {
		slog.Error("finish proof verification job", "job_id", job.ID, "err", err)
		return
	}
	result := "completed"
	if retry {
		result = "retry"
		if job.Attempts >= job.MaxAttempts {
			result = "failed"
		}
	}
	metrics.VerificationJob(result, job.Kind)
	if run != nil {
		s.observeRun(run, start)
	}
	s.signal()
	s.reconcileWake.Notify()
}

func (s *Service) executeJob(ctx context.Context, job *store.VerificationJob) (*store.VerificationRun, error) {
	return worker.GuardValue("verification worker", func() (*store.VerificationRun, error) {
		var input verificationInput
		var loc *store.Location
		var err error
		if s.verifyFn != nil {
			loc, err = s.store.GetContext(ctx, job.LocationID)
		} else {
			input, err = s.readVerificationInput(ctx, job.LocationID, job.Kind)
			loc = input.location
		}
		if err != nil {
			return nil, err
		}
		stored, _, payloadOK, err := digestMatches(loc)
		if err != nil {
			copy(stored[:], loc.Digest)
			payloadOK = false
		}
		if s.verifyFn != nil {
			return s.verifyFn(ctx, loc, stored, payloadOK, job.Kind, job.Trigger), nil
		}
		return s.verifyProof(ctx, input, stored, payloadOK, job.Kind, job.Trigger), nil
	})
}

func (s *Service) readVerificationInput(ctx context.Context, id int64, kind string) (verificationInput, error) {
	input := verificationInput{cfg: s.cfg.Snapshot()}
	unlock, err := s.store.LockProofSnapshotContext(ctx)
	if err != nil {
		return input, err
	}
	defer unlock()
	input.location, err = s.store.GetContext(ctx, id)
	if err != nil {
		return input, err
	}
	ext := ".tsr"
	if kind == "ots" {
		ext = ".ots"
	}
	input.proof, input.readErr = readBoundedFile(filepath.Join(input.cfg.ProofsDir(), fmt.Sprintf("%d%s", id, ext)), maxArtifactBytes)
	return input, ctx.Err()
}

func (s *Service) observeRun(run *store.VerificationRun, start time.Time) {
	metrics.Verification(run.Kind, run.Result, run.Trigger, time.Since(start))
	for _, attempt := range run.Attempts {
		metrics.VerificationAttempt(attempt.ProviderID, attempt.Result)
		if run.Kind == "ots" {
			metrics.OTSAnchor("header", store.VerificationEvidenceStatus(attempt.HeaderValid, attempt.ErrorCode, "header"))
			metrics.OTSAnchor("pow", store.VerificationEvidenceStatus(attempt.POWValid, attempt.ErrorCode, "pow"))
			metrics.OTSAnchor("merkle", store.VerificationEvidenceStatus(attempt.MerkleValid, attempt.ErrorCode, "merkle"))
			confirmation := "unavailable"
			if attempt.Confirmations > 0 {
				confirmation = "mature"
				if attempt.Result == "pending" {
					confirmation = "pending"
				}
			}
			metrics.OTSAnchor("confirmations", confirmation)
		}
	}
}

func (s *Service) scheduler() {
	defer s.wg.Done()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
		case <-s.reconcileWake.C():
		}
		s.reconcile()
		s.reconcileMu.Lock()
		continuing := s.reconcileID != 0
		s.reconcileMu.Unlock()
		delay := reconcileInterval
		if continuing {
			delay = time.Second
		}
		timer.Reset(delay)
	}
}

func (s *Service) reconcile() {
	defer s.cleanupHistory()
	active, err := s.store.ActiveVerificationJobCount()
	if err != nil {
		slog.Error("count proof verification jobs", "err", err)
		return
	}
	remaining := maxBacklog - active
	if remaining <= 0 {
		return
	}
	cfg := s.cfg.Snapshot()
	stop := errors.New("verification backlog full")
	type candidate struct {
		id   int64
		kind string
	}
	candidates := make([]candidate, 0, remaining)
	policies := map[string][]byte{"tsa": policyHash(cfg, "tsa"), "ots": policyHash(cfg, "ots")}
	s.reconcileMu.Lock()
	afterID, afterKind := s.reconcileID, s.reconcileKind
	epoch := s.reconcileEpoch
	s.reconcileMu.Unlock()
	lastID, lastKind := afterID, afterKind
	scanned := 0
	err = s.store.IterateVerificationReconcileAfter(afterID, afterKind, 500, func(items []store.VerificationReconcileItem) error {
		for _, item := range items {
			if scanned == 500 {
				return stop
			}
			scanned++
			lastID, lastKind = item.LocationID, item.Kind
			if s.ctx.Err() != nil {
				return s.ctx.Err()
			}
			if item.JobState == "queued" || item.JobState == "running" || item.JobState == "retry" {
				continue
			}
			var run *store.VerificationRun
			if item.RunID > 0 {
				run = &store.VerificationRun{ID: item.RunID, Result: item.Result,
					CheckedAt: item.CheckedAt, ProofSHA256: item.ProofHash,
					PolicySHA256: item.PolicyHash, EngineVersion: item.Engine,
					ErrorCode: item.ErrorCode}
			}
			ext := ".tsr"
			if item.Kind == "ots" {
				ext = ".ots"
			}
			path := filepath.Join(cfg.ProofsDir(), fmt.Sprintf("%d%s", item.LocationID, ext))
			if !stale(run, path, policies[item.Kind]) {
				continue
			}
			if item.JobState == "failed" && time.Since(time.Unix(item.JobUpdated, 0)) < verifyInterval &&
				!verificationPolicyChanged(run, policies[item.Kind]) &&
				!proofChangedSinceRun(run, path) {
				continue
			}
			candidates = append(candidates, candidate{item.LocationID, item.Kind})
			remaining--
			if remaining == 0 {
				return stop
			}
		}
		return nil
	})
	s.reconcileMu.Lock()
	if epoch != s.reconcileEpoch {
		s.reconcileMu.Unlock()
		return
	}
	if errors.Is(err, stop) {
		s.reconcileID, s.reconcileKind = lastID, lastKind
	} else if err == nil {
		s.reconcileID, s.reconcileKind = 0, ""
	}
	s.reconcileMu.Unlock()
	if err != nil && !errors.Is(err, stop) && !errors.Is(err, context.Canceled) {
		slog.Error("reconcile proof verification jobs", "err", err)
	}
	for _, item := range candidates {
		if _, _, err := s.Enqueue(item.id, item.kind, "scheduled"); err != nil &&
			!errors.Is(err, context.Canceled) {
			slog.Error("enqueue scheduled proof verification", "location_id", item.id,
				"kind", item.kind, "err", err)
			s.Reload()
			break
		}
	}
}

func (s *Service) cleanupHistory() {
	now := time.Now()
	for {
		previous := s.cleanupAt.Load()
		if previous > 0 && now.Sub(time.Unix(previous, 0)) < historyCleanup {
			return
		}
		if s.cleanupAt.CompareAndSwap(previous, now.Unix()) {
			break
		}
	}
	if _, err := s.store.DeleteVerificationHistory(now.Add(-historyKeep).Unix()); err != nil {
		s.cleanupAt.Store(0)
		slog.Error("clean proof verification history", "err", err)
		metrics.VerificationHistoryCleanup("error")
		return
	}
	metrics.VerificationHistoryCleanup("ok")
}

func (s *Service) refreshMetrics() {
	status, err := s.store.VerificationJobStatus()
	if err == nil {
		metrics.SetVerificationJobs(status.Queued, status.Running, status.Retry,
			status.Failed, status.OldestQueued)
	}
}

func (s *Service) verifyProof(ctx context.Context, input verificationInput, digest [32]byte, payloadOK bool, kind, trigger string) *store.VerificationRun {
	loc, cfg := input.location, input.cfg
	run := &store.VerificationRun{
		LocationID: loc.ID, Kind: kind, Trigger: trigger, CheckedAt: time.Now().Unix(),
	}
	run.EngineVersion = engineVersion
	run.PolicySHA256 = policyHash(cfg, kind)
	if !payloadOK {
		run.Result, run.ErrorCode = "invalid", "payload_digest_mismatch"
		return run
	}
	proof, err := input.proof, input.readErr
	if errors.Is(err, os.ErrNotExist) {
		run.Result, run.ErrorCode = "missing", "proof_missing"
		return run
	}
	if err != nil {
		run.Result, run.ErrorCode = "error", "proof_read"
		return run
	}
	proofHash := sha256.Sum256(proof)
	run.ProofSHA256 = proofHash[:]
	if kind == "tsa" {
		rootsFile, cleanup, rootErr := stageTrustRoots(cfg.TSATrustRoots)
		if rootErr != nil {
			probe, probeCleanup, probeErr := stageEmptyTrustRoots()
			if probeErr == nil {
				_, verifyErr := tsa.VerifyTokenPolicy(proof, digest, tsa.TrustPolicy{
					RootsFile: probe, PolicyOIDs: cfg.TSAPolicyOIDs,
					ExpectedNonce: loc.TSANonce,
				})
				probeCleanup()
				if verifyErr != nil && !errors.Is(verifyErr, tsa.ErrTrustUnavailable) {
					run.Result, run.ErrorCode = "invalid", "tsa_invalid"
					switch {
					case errors.Is(verifyErr, tsa.ErrNonceMismatch):
						run.ErrorCode = "tsa_nonce_mismatch"
					case errors.Is(verifyErr, tsa.ErrPolicyMismatch):
						run.ErrorCode = "tsa_policy_mismatch"
					}
					return run
				}
			}
			run.Result, run.ErrorCode = "error", "tsa_trust_unavailable"
			return run
		}
		defer cleanup()
		result, err := tsa.VerifyTokenPolicy(proof, digest, tsa.TrustPolicy{
			RootsFile: rootsFile, PolicyOIDs: cfg.TSAPolicyOIDs,
			ExpectedNonce: loc.TSANonce,
		})
		if err != nil {
			run.Result, run.ErrorCode = "invalid", "tsa_invalid"
			switch {
			case errors.Is(err, tsa.ErrNonceMismatch):
				run.ErrorCode = "tsa_nonce_mismatch"
			case errors.Is(err, tsa.ErrPolicyMismatch):
				run.ErrorCode = "tsa_policy_mismatch"
			case errors.Is(err, tsa.ErrTrustUnavailable):
				run.Result, run.ErrorCode = "error", "tsa_trust_unavailable"
			}
			return run
		}
		run.Result = "degraded"
		if result.Pinned && result.NonceStatus == "matched" && result.PolicyStatus == "matched" {
			run.Result = "verified"
		}
		genTime := result.GenTime.Unix()
		run.TSAGenTime = &genTime
		run.TSAPolicyOID = result.PolicyOID
		run.TSASerial = result.Serial
		run.TSASignerSubject = result.SignerSubject
		run.TSASignerFingerprint = result.SignerFingerprint
		run.TSANonceStatus = result.NonceStatus
		run.TSAPolicyStatus = result.PolicyStatus
		run.TSATrustMode = result.TrustMode
		run.TSAAccuracyMS = result.Accuracy.Milliseconds()
		run.TSATrustAnchorFingerprint = result.TrustAnchorFingerprint
		metrics.TSATrust("nonce", result.NonceStatus)
		metrics.TSATrust("policy", result.PolicyStatus)
		metrics.TSATrust("roots", result.TrustMode)
		return run
	}
	vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, verifyErr := ots.VerifyPool(vctx, proof, digest, cfg.EsploraURLs, cfg.OTSConfirmations)
	run.Result = result.Result
	run.ErrorCode = result.ErrorCode
	run.AgreeingSources = result.Agreeing
	run.SourceCount = len(result.Attempts)
	if result.Height > 0 {
		run.BitcoinHeight = &result.Height
	}
	run.BitcoinBlockHash = result.BlockHash
	run.BitcoinPOWValid = result.POWValid
	run.BitcoinConfirmations = result.Confirmations
	if verifyErr != nil {
		run.ErrorCode = verifyErr.Error()
	}
	for _, attempt := range result.Attempts {
		a := store.VerificationAttempt{
			ProviderID: attempt.ProviderID, Result: attempt.Result,
			BlockHash: attempt.BlockHash, DurationMS: attempt.Duration.Milliseconds(),
			ErrorCode: attempt.ErrorCode, TipHeight: attempt.TipHeight,
			Confirmations: attempt.Confirmations, HeaderValid: attempt.HeaderValid,
			POWValid: attempt.POWValid, MerkleValid: attempt.MerkleValid,
		}
		if attempt.Height > 0 {
			height := attempt.Height
			a.BlockHeight = &height
		}
		run.Attempts = append(run.Attempts, a)
	}
	return run
}

func digestMatches(loc *store.Location) (stored, recomputed [32]byte, ok bool, err error) {
	copy(stored[:], loc.Digest)
	var raw map[string]any
	if err := json.Unmarshal(loc.Payload, &raw); err != nil {
		return stored, recomputed, false, err
	}
	rec, err := canonical.FromLocation(loc.User, loc.Device, raw)
	if err != nil {
		return stored, recomputed, false, err
	}
	recomputed, err = rec.Digest()
	if err != nil {
		return stored, recomputed, false, err
	}
	return stored, recomputed, subtle.ConstantTimeCompare(stored[:], recomputed[:]) == 1, nil
}

func stale(run *store.VerificationRun, path string, policy []byte) bool {
	if run == nil {
		return true
	}
	if verificationPolicyChanged(run, policy) {
		return true
	}
	proof, err := readBoundedFile(path, maxArtifactBytes)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return true
		}
		return run.Result != "missing" ||
			time.Since(time.Unix(run.CheckedAt, 0)) >= verifyInterval
	}
	sum := sha256.Sum256(proof)
	return !bytes.Equal(run.ProofSHA256, sum[:]) ||
		time.Since(time.Unix(run.CheckedAt, 0)) >= verifyInterval
}

func proofChangedSinceRun(run *store.VerificationRun, path string) bool {
	proof, err := readBoundedFile(path, maxArtifactBytes)
	if run == nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return len(run.ProofSHA256) > 0 || run.ErrorCode == "proof_read"
		}
		return len(run.ProofSHA256) > 0 || run.Result == "missing" || run.ErrorCode == "proof_missing"
	}
	if len(run.ProofSHA256) == 0 {
		return run.ErrorCode == "proof_read" || run.ErrorCode == "proof_missing"
	}
	sum := sha256.Sum256(proof)
	return !bytes.Equal(run.ProofSHA256, sum[:])
}

func verificationPolicyChanged(run *store.VerificationRun, policy []byte) bool {
	return run != nil && (run.EngineVersion != engineVersion || !bytes.Equal(run.PolicySHA256, policy))
}

func verificationPending(run *store.VerificationRun, jobs []store.VerificationJob, kind string) bool {
	found := false
	for _, job := range jobs {
		if job.Kind == kind {
			found = true
			if job.State != "done" {
				return true
			}
		}
	}
	return run != nil && run.Trigger == "import" && !found
}

func policyHash(cfg *config.Config, kind string) []byte {
	type policy struct {
		Engine        int      `json:"engine"`
		Kind          string   `json:"kind"`
		TrustRoots    string   `json:"trust_roots,omitempty"`
		PolicyOIDs    []string `json:"policy_oids,omitempty"`
		Providers     []string `json:"providers,omitempty"`
		Confirmations int      `json:"confirmations,omitempty"`
	}
	p := policy{Engine: engineVersion, Kind: kind}
	if kind == "tsa" {
		p.PolicyOIDs = append([]string(nil), cfg.TSAPolicyOIDs...)
		if cfg.TSATrustRoots != "" {
			b, err := readBoundedFile(cfg.TSATrustRoots, maxArtifactBytes)
			if err != nil {
				p.TrustRoots = "unavailable"
			} else {
				sum := sha256.Sum256(b)
				p.TrustRoots = hex.EncodeToString(sum[:])
			}
		}
	} else {
		p.Providers = append([]string(nil), cfg.EsploraURLs...)
		p.Confirmations = cfg.OTSConfirmations
	}
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return sum[:]
}

func checkedAt(run *store.VerificationRun) int64 {
	if run == nil {
		return 0
	}
	return run.CheckedAt
}

func overall(tsaRun, otsRun *store.VerificationRun, tsaStale, otsStale, payloadOK bool) string {
	if !payloadOK {
		return "invalid"
	}
	if tsaStale || otsStale || tsaRun == nil || otsRun == nil {
		return "stale"
	}
	return verification.MergeOverall(tsaRun.Result, otsRun.Result)
}
