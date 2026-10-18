package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreOperationsHonorCanceledContext(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name string
		fn   func() error
	}{
		{"location", func() error { _, err := st.GetContext(ctx, 1); return err }},
		{"location stats", func() error { _, err := st.StatsContext(ctx); return err }},
		{"privacy stats", func() error { _, err := st.PrivacyStatsContext(ctx); return err }},
		{"storage stats", func() error { _, err := st.StorageStatsContext(ctx); return err }},
		{"stamp status", func() error { _, err := st.JobStatusContext(ctx); return err }},
		{"stamp leases", func() error { _, err := st.ExpiredLeaseCountContext(ctx, 1); return err }},
		{"verification status", func() error { _, err := st.VerificationJobStatusContext(ctx); return err }},
		{"verification leases", func() error { _, err := st.VerificationExpiredLeaseCountContext(ctx, 1); return err }},
		{"verification stats", func() error { _, err := st.VerificationStatsContext(ctx); return err }},
		{"verification providers", func() error { _, err := st.VerificationProviderStatusesContext(ctx); return err }},
		{"verification stale", func() error { _, err := st.VerificationStaleCountContext(ctx, 1, 1, nil, nil); return err }},
		{"verification policy", func() error { _, err := st.VerificationPolicyStaleCountContext(ctx, 1, nil, nil); return err }},
		{"maintenance jobs", func() error { _, err := st.ListMaintenanceJobsContext(ctx, "", ""); return err }},
		{"maintenance status", func() error { _, err := st.MaintenanceStatusContext(ctx); return err }},
		{"maintenance leases", func() error { _, err := st.MaintenanceExpiredLeaseCountContext(ctx, 1); return err }},
		{"timeline jobs", func() error { _, err := st.TimelineJobCountContext(ctx); return err }},
		{"geofence failures", func() error { _, err := st.GeofenceDeliveryFailuresContext(ctx); return err }},
		{"stamp renewal", func() error { _, err := st.RenewStampJobContext(ctx, 1, "owner", 2); return err }},
		{"maintenance renewal", func() error { _, err := st.RenewMaintenanceJobContext(ctx, 1, "owner", 2, 1); return err }},
		{"verification renewal", func() error { _, err := st.RenewVerificationJobContext(ctx, 1, "owner", 2, 1); return err }},
		{"timeline renewal", func() error { _, err := st.RenewTimelineJobContext(ctx, 1, "owner", 2); return err }},
		{"geofence renewal", func() error { _, err := st.RenewGeofenceDeliveryContext(ctx, 1, "owner", 2); return err }},
		{"session cleanup", func() error { _, err := st.CleanupSessionsContext(ctx, 1, 1); return err }},
		{"proof GC status", func() error { _, err := st.ProofGCStatusContext(ctx); return err }},
		{"retention delete", func() error { _, err := st.DeleteOlderThanContext(ctx, t.TempDir(), 1, "tst"); return err }},
		{"range delete", func() error { _, err := st.DeleteRangeContext(ctx, t.TempDir(), ExportFilter{From: 1}); return err }},
		{"export snapshot", func() error { _, err := st.BeginExportSnapshotContext(ctx, ExportFilter{}); return err }},
		{"track snapshot", func() error { _, err := st.BeginTrackSnapshotContext(ctx, TrackFilter{}); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.fn(); !errors.Is(err, context.Canceled) {
				t.Fatalf("err=%v, want context.Canceled", err)
			}
		})
	}
}

func TestPrivacyStatsCancellationWhileWaitingForScan(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	st.privacyScan <- struct{}{}
	defer func() { <-st.privacyScan }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := st.PrivacyStatsContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("privacy scan wait err=%v", err)
	}
}

func TestPrivacyStatsCancellationWhileWaitingForCommit(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.privacyMu.Acquire(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	defer st.privacyMu.Release(1)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := st.PrivacyStatsContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("privacy commit wait err=%v", err)
	}
}

func TestImportTxEnqueueVerification(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.ImportLocation(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ImportAtomic(func(tx *ImportTx) error {
		return tx.EnqueueVerification(id, "tsa", "import", 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ImportAtomic(func(tx *ImportTx) error {
		return tx.EnqueueVerification(id, "bad", "import", 1)
	}); err == nil {
		t.Fatal("invalid verification kind accepted")
	}
	geofence, err := st.CreateGeofence(&Geofence{Name: "test", Lat: 0, Lon: 0, RadiusM: 100, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RebuildGeofenceState(geofence.ID); err != nil {
		t.Fatal(err)
	}
	if valid, err := st.StampJobLeaseValid(999, "owner", 1); err != nil || valid {
		t.Fatalf("lease valid=%t err=%v", valid, err)
	}
	if err := st.IterateVerificationRuns(ExportFilter{}, 1, func([]VerificationRun) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
