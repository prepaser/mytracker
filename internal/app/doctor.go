package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
	"golang.org/x/sys/unix"
)

func inspectInstance(cfg *config.Config) (map[string]any, error) {
	if err := config.ValidateDataDir(cfg.DataDir); err != nil {
		return nil, err
	}
	base := map[string]any{"ok": true, "schema_version": 0, "state": "uninitialized"}
	info, err := os.Stat(cfg.DataDir)
	if os.IsNotExist(err) {
		return base, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("data_dir must be a directory")
	}
	lock, err := lockExistingFile(filepath.Join(cfg.DataDir, ".mytracker.lock"))
	if err != nil {
		return nil, fmt.Errorf("data directory is in use: %w", err)
	}
	if lock != nil {
		defer func() { _ = lock.Close() }()
	}
	journal, err := os.ReadFile(filepath.Join(cfg.DataDir, ".restore-journal.json"))
	if err == nil {
		var phase struct {
			Phase string `json:"phase"`
		}
		if err := json.Unmarshal(journal, &phase); err != nil {
			return nil, fmt.Errorf("invalid restore journal")
		}
		switch phase.Phase {
		case "staging", "prepared", "committed":
			base["state"], base["restore_phase"] = "recovery_required", phase.Phase
			return base, nil
		default:
			return nil, fmt.Errorf("invalid restore journal phase")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return base, nil
	} else if err != nil {
		return nil, err
	}
	st, err := store.OpenReadOnly(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	if err := store.CheckDB(cfg.DBPath()); err != nil {
		return nil, err
	}
	version, err := st.SchemaVersion()
	if err != nil {
		return nil, err
	}
	if version > store.CurrentSchemaVersion {
		return nil, fmt.Errorf("database schema v%d is newer than supported v%d", version, store.CurrentSchemaVersion)
	}
	if version < store.CurrentSchemaVersion {
		base["state"], base["schema_version"] = "migration_required", version
		return base, nil
	}
	settings, err := st.LoadSettings()
	if err != nil {
		return nil, err
	}
	if err := cfg.ApplySettings(settings); err != nil {
		return nil, err
	}
	return doctor(cfg, st, len(settings) > 0)
}

func doctor(cfg *config.Config, st *store.Store, settingsInitialized bool) (map[string]any, error) {
	now := time.Now().Unix()
	unlockBackup, err := st.LockBackupSnapshotContext(context.Background())
	if err != nil {
		return nil, fmt.Errorf("backup files are in use: %w", err)
	}
	defer unlockBackup()
	if err := st.Ping(); err != nil {
		return nil, err
	}
	if err := unix.Access(cfg.DBPath(), unix.W_OK); err != nil {
		return nil, fmt.Errorf("database is not writable: %w", err)
	}
	if err := store.CheckDB(cfg.DBPath()); err != nil {
		return nil, err
	}
	for _, dir := range []string{cfg.ProofsDir(), cfg.BackupsDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s must be a directory", dir)
		}
		if err := unix.Access(dir, unix.W_OK|unix.X_OK); err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
	}
	proofLock, err := lockExistingFile(filepath.Join(cfg.DataDir, ".mytracker-proofs.lock"))
	if err != nil {
		return nil, fmt.Errorf("proof files are in use: %w", err)
	}
	if proofLock != nil {
		defer func() { _ = proofLock.Close() }()
	}
	audit, err := st.AuditProofs(cfg.ProofsDir())
	if err != nil {
		return nil, err
	}
	gc, err := st.ProofGCStatus()
	if err != nil {
		return nil, err
	}
	expired, err := st.ExpiredLeaseCount(now)
	if err != nil {
		return nil, err
	}
	verifyExpired, err := st.VerificationExpiredLeaseCount(now)
	if err != nil {
		return nil, err
	}
	verifyJobs, err := st.VerificationJobStatus()
	if err != nil {
		return nil, err
	}
	maintenanceJobs, err := st.MaintenanceStatus()
	if err != nil {
		return nil, err
	}
	maintenanceExpired, err := st.MaintenanceExpiredLeaseCount(now)
	if err != nil {
		return nil, err
	}
	maintenanceOverdue, err := st.MaintenanceOverdueCount(now)
	if err != nil {
		return nil, err
	}
	timelineJobs, err := st.TimelineJobDiagnostics(now)
	if err != nil {
		return nil, err
	}
	geofenceDeliveries, err := st.GeofenceDeliveryDiagnostics(now)
	if err != nil {
		return nil, err
	}
	version, err := st.SchemaVersion()
	if err != nil {
		return nil, err
	}
	backupState := "missing"
	backupPath := ""
	if path, _, err := store.LatestBackup(cfg.BackupsDir()); err == nil {
		backupPath = path
		if strings.HasSuffix(path, ".mtbackup") {
			err = backup.CheckFile(path)
		} else {
			err = store.CheckDB(path)
		}
		if err != nil {
			return nil, fmt.Errorf("latest backup: %w", err)
		}
		backupState = "ok"
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	privacy, err := st.PrivacyStats()
	if err != nil {
		return nil, err
	}
	storage, err := st.StorageStats()
	if err != nil {
		return nil, err
	}
	missingIndexes, err := st.MissingRangeIndexes()
	if err != nil {
		return nil, err
	}
	_, initialErr := os.Stat(config.InitialPasswordPath())
	if os.IsNotExist(initialErr) {
		_, initialErr = os.Stat("initial-password")
	}
	if os.IsNotExist(initialErr) {
		_, initialErr = os.Stat(filepath.Join(cfg.DataDir, "initial-password"))
	}
	backupCount, oldestBackup := backupRetentionInfo(cfg.BackupsDir())
	return map[string]any{
		"ok":             true,
		"schema_version": version,
		"proofs":         audit,
		"proof_gc":       gc,
		"expired_leases": expired,
		"verification_jobs": map[string]any{
			"status":         verifyJobs,
			"expired_leases": verifyExpired,
		},
		"maintenance_jobs": map[string]any{
			"status":          maintenanceJobs,
			"expired_leases":  maintenanceExpired,
			"overdue":         maintenanceOverdue,
			"failed":          maintenanceJobs.Failed,
			"incomplete_runs": maintenanceJobs.Running,
		},
		"timeline_jobs":       timelineJobs,
		"geofence_deliveries": geofenceDeliveries,
		"backup": map[string]any{
			"state": backupState,
			"path":  backupPath,
		},
		"storage": map[string]any{
			"stats":                 storage,
			"missing_range_indexes": missingIndexes,
			"high_free_page_ratio":  storage.PageCount >= 1000 && storage.FreePageRatio >= 0.25,
		},
		"privacy": map[string]any{
			"initial_password_file": initialErr == nil,
			"settings_initialized":  settingsInitialized,
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

func backupRetentionInfo(dir string) (int, string) {
	entries, _ := os.ReadDir(dir)
	count := 0
	var oldest time.Time
	for _, entry := range entries {
		if entry.IsDir() ||
			(!strings.HasSuffix(entry.Name(), ".mtbackup") && !strings.HasSuffix(entry.Name(), ".db")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		count++
		if oldest.IsZero() || info.ModTime().Before(oldest) {
			oldest = info.ModTime()
		}
	}
	if oldest.IsZero() {
		return count, ""
	}
	return count, oldest.UTC().Format(time.RFC3339)
}
