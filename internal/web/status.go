package web

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/mqtt"
	"github.com/prepaser/mytracker/internal/store"
)

const backupInvalidCacheTTL = time.Minute

func (s *Server) collectStatusContext(ctx context.Context, now time.Time) (map[string]any, error) {
	cfg := s.Cfg.Snapshot()
	stats, err := s.Store.StatsContext(ctx)
	if err != nil {
		return nil, err
	}
	jobs, err := s.Store.JobStatusContext(ctx)
	if err != nil {
		return nil, err
	}
	verifyJobs, err := s.Store.VerificationJobStatusContext(ctx)
	if err != nil {
		return nil, err
	}
	verificationExpired, err := s.Store.VerificationExpiredLeaseCountContext(ctx, now.Unix())
	if err != nil {
		return nil, err
	}
	gc, err := s.Store.ProofGCStatusContext(ctx)
	if err != nil {
		return nil, err
	}
	expiredLeases, err := s.Store.ExpiredLeaseCountContext(ctx, now.Unix())
	if err != nil {
		return nil, err
	}
	unlockBackup, err := s.Store.LockBackupSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	backupPath, backupAt, backupErr := store.LatestBackup(cfg.BackupsDir())
	var backupState string
	backupCreatedAt := ""
	if backupErr != nil {
		if !os.IsNotExist(backupErr) {
			unlockBackup()
			return nil, backupErr
		}
		backupState, backupPath = "missing", ""
	} else {
		backupCreatedAt = backupAt.UTC().Format(time.RFC3339)
		backupState = s.verifyBackupStateContext(ctx, backupPath, time.Now())
	}
	unlockBackup()
	privacy, err := s.Store.PrivacyStatsContext(ctx)
	if err != nil {
		return nil, err
	}
	_, initialErr := os.Stat(s.Cfg.InitialPasswordPath())
	backupCount, oldestBackup := backupInventory(cfg.BackupsDir())
	verification, stale, err := s.Verification.VerificationSummaryContext(ctx)
	if err != nil {
		return nil, err
	}
	policyStale, err := s.Verification.VerificationPolicyStaleContext(ctx)
	if err != nil {
		return nil, err
	}
	providers, err := s.Store.VerificationProviderStatusesContext(ctx)
	if err != nil {
		return nil, err
	}
	maintenanceJobs, err := s.Store.ListMaintenanceJobsContext(ctx, "", "")
	if err != nil {
		return nil, err
	}
	maintenanceStatus, err := s.Store.MaintenanceStatusContext(ctx)
	if err != nil {
		return nil, err
	}
	maintenanceExpired, err := s.Store.MaintenanceExpiredLeaseCountContext(ctx, now.Unix())
	if err != nil {
		return nil, err
	}
	storage, err := s.Store.StorageStatsContext(ctx)
	if err != nil {
		return nil, err
	}
	timelineJobs, err := s.Store.TimelineJobCountContext(ctx)
	if err != nil {
		return nil, err
	}
	geofenceFailures, err := s.Store.GeofenceDeliveryFailuresContext(ctx)
	if err != nil {
		return nil, err
	}
	adminEvents, shareEvents := s.eventCounts()
	return map[string]any{
		"locations": stats,
		"storage":   storage,
		"timeline":  map[string]any{"rebuild_backlog": timelineJobs, "worker_healthy": s.Timeline == nil || s.Timeline.Healthy()},
		"geofences": map[string]any{"delivery_failures": geofenceFailures, "worker_healthy": s.Geofence == nil || s.Geofence.Healthy()},
		"realtime": map[string]any{
			"admin_connections": adminEvents,
			"share_connections": shareEvents,
			"hub":               s.events.Stats(),
		},
		"stamp_jobs": jobs,
		"workers": map[string]any{
			"healthy":        s.Stamp.Healthy(),
			"running_leases": jobs.Running,
			"expired_leases": expiredLeases,
			"lease_seconds":  s.Stamp.LeaseSeconds(),
		},
		"proof_gc": gc,
		"proof_verification": map[string]any{
			"results":        verification.Results,
			"stale":          stale,
			"policy_stale":   policyStale,
			"last_success":   verification.LastSuccess,
			"providers":      providers,
			"jobs":           verifyJobs,
			"worker_healthy": s.Verification.VerificationHealthy(),
			"lease_seconds":  s.Verification.VerificationLeaseSeconds(),
			"expired_leases": verificationExpired,
		},
		"mqtt": map[string]any{"connected": mqtt.Connected(), "enabled": cfg.MQTTEnabled},
		"maintenance": map[string]any{
			"jobs":           maintenanceJobs,
			"status":         maintenanceStatus,
			"worker_healthy": s.Maintenance.Healthy(),
			"lease_seconds":  s.Maintenance.LeaseSeconds(),
			"expired_leases": maintenanceExpired,
		},
		"backup": map[string]any{
			"state": backupState, "path": backupPath, "created_at": backupCreatedAt,
		},
		"privacy": map[string]any{
			"initial_password_file": initialErr == nil,
			"purge_pending":         privacySafetyBackupExists(cfg.DataDir),
			"legacy_raw_payloads":   privacy.LegacyRawPayloads,
			"risky_shares":          privacy.RiskyShares,
			"backup_retention_days": cfg.BackupRetentionDays(),
			"backup_keep":           cfg.BackupKeep,
			"backup_count":          backupCount,
			"oldest_backup_at":      oldestBackup,
		},
	}, nil
}

func (s *Server) verifyBackupState(path string) string {
	return s.verifyBackupStateAt(path, time.Now())
}

func (s *Server) verifyBackupStateAt(path string, now time.Time) string {
	return s.verifyBackupStateContext(context.Background(), path, now)
}

func (s *Server) verifyBackupStateContext(ctx context.Context, path string, now time.Time) string {
	if ctx.Err() != nil {
		return "invalid"
	}
	info, err := os.Stat(path)
	if err != nil {
		return "invalid"
	}
	for {
		if ctx.Err() != nil {
			return "invalid"
		}
		s.backupMu.Lock()
		if cached := s.backupCheck; cached.path == path && cached.size == info.Size() &&
			cached.mtimeNano == info.ModTime().UnixNano() {
			if cached.state == "ok" || now.Before(cached.checkedAt.Add(backupInvalidCacheTTL)) {
				s.backupMu.Unlock()
				return cached.state
			}
		}
		if s.backupBusy == nil {
			s.backupBusy = make(chan struct{})
			done := s.backupBusy
			s.backupMu.Unlock()
			defer func() {
				s.backupMu.Lock()
				s.backupBusy = nil
				close(done)
				s.backupMu.Unlock()
			}()
			break
		}
		done := s.backupBusy
		s.backupMu.Unlock()
		select {
		case <-ctx.Done():
			return "invalid"
		case <-done:
		}
	}
	if strings.HasSuffix(path, ".mtbackup") {
		err = backup.CheckFileContext(ctx, path)
	} else {
		err = store.CheckDBContext(ctx, path)
	}
	state := "ok"
	if err != nil {
		state = "invalid"
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return state
	}
	s.backupMu.Lock()
	s.backupCheck = backupCheck{
		path: path, size: info.Size(), mtimeNano: info.ModTime().UnixNano(), state: state, checkedAt: now,
	}
	s.backupMu.Unlock()
	return state
}
