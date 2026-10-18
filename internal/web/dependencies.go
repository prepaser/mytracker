package web

import (
	"context"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/proofverify"
	"github.com/prepaser/mytracker/internal/realtime"
	"github.com/prepaser/mytracker/internal/store"
)

type ConfigManager interface {
	Snapshot() *config.Config
	SnapshotWithRevision() (*config.Config, string)
	InitialPasswordPath() string
	RemoveInitialPassword() error
	Update(func(*config.Settings) error) (*config.Config, error)
	UpdateIfRevision(string, func(*config.Settings) error) (*config.Config, error)
	UpdateWithSessionRevocation(func(*config.Settings) error) (*config.Config, error)
	UpdateWithSessionRevocationIfRevision(string, func(*config.Settings) error) (*config.Config, error)
}

type StampService interface {
	Process(int64, [32]byte)
	Wake()
	QueueDepth() int
	Healthy() bool
	LeaseSeconds() int
	RetryResult(int64) (int, error)
	RetryAll() (int, error)
}

type VerificationService interface {
	TriggerVerification(int64)
	ReloadVerification()
	EnqueueVerification(int64) ([]store.VerificationJob, error)
	LatestVerification(int64) (*proofverify.View, error)
	VerificationHistory(int64, int64, int) ([]store.VerificationRun, error)
	VerificationSummaryContext(context.Context) (store.VerificationStats, int, error)
	VerificationPolicyStaleContext(context.Context) (int, error)
	VerificationHealthy() bool
	VerificationLeaseSeconds() int
}

type MaintenanceService interface {
	Enqueue(string, string) (*store.MaintenanceJob, bool, error)
	Retry(int64) (*store.MaintenanceJob, error)
	Reload()
	Healthy() bool
	LeaseSeconds() int
}

type TimelineService interface {
	Reload() error
	Wake()
	Healthy() bool
}
type GeofenceService interface{ Healthy() bool }

type Dependencies struct {
	Config       ConfigManager
	Store        *store.Store
	Stamp        StampService
	Verification VerificationService
	Maintenance  MaintenanceService
	Events       *realtime.Hub
	Timeline     TimelineService
	Geofence     GeofenceService
}
