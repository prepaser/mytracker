package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/metrics"
	"github.com/prepaser/mytracker/internal/store"
)

func Run(cfg *config.Manager, st *store.Store) (int64, error) {
	return RunContext(context.Background(), cfg, st)
}

func RunContext(ctx context.Context, cfg *config.Manager, st *store.Store) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	snap := cfg.Snapshot()
	if snap.RetentionDays <= 0 {
		return 0, nil
	}
	unlock, err := st.LockBackupContext(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock()
	maxID, err := st.LocationWatermarkContext(ctx)
	if err != nil {
		return 0, err
	}
	if _, err := backup.CreateLockedContext(ctx, snap, st); err != nil {
		slog.Error("retention backup failed, skipping", "err", err)
		metrics.Backup("fail")
		return 0, err
	}
	metrics.Backup("ok")
	cutoff := time.Now().Add(-time.Duration(snap.RetentionDays) * 24 * time.Hour).Unix()
	n, err := st.DeleteOlderThanThroughContext(ctx, snap.ProofsDir(), cutoff, snap.RetentionBy, maxID)
	if err != nil {
		slog.Error("retention", "err", err)
		return 0, err
	}
	if n > 0 {
		metrics.AddRetentionDeleted(n)
		slog.Info("retention deleted", "count", n, "days", snap.RetentionDays)
	}
	return n, nil
}
