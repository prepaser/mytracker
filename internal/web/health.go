package web

import (
	"encoding/json"
	"net/http"
	"os"
	"time"
)

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Ping(); err != nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":          true,
		"stamp_queue": s.Stamp.QueueDepth(),
	})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	cfg := s.Cfg.Snapshot()
	checks := map[string]bool{
		"db":                   s.Store.Ready() == nil,
		"proofs":               writableDir(cfg.ProofsDir()),
		"backups":              writableDir(cfg.BackupsDir()),
		"stamp_workers":        s.Stamp.Healthy(),
		"verification_workers": s.Verification.VerificationHealthy(),
		"maintenance_worker":   s.Maintenance.Healthy(),
		"timeline_worker":      s.Timeline == nil || s.Timeline.Healthy(),
		"geofence_worker":      s.Geofence == nil || s.Geofence.Healthy(),
	}
	jobs, err := s.Store.JobStatus()
	verifyJobs, verifyErr := s.Store.VerificationJobStatus()
	maintenanceJobs, maintenanceErr := s.Store.MaintenanceStatus()
	now := time.Now().Unix()
	stampExpired, stampLeaseErr := s.Store.ExpiredLeaseCount(now)
	verificationExpired, verificationLeaseErr := s.Store.VerificationExpiredLeaseCount(now)
	maintenanceExpired, maintenanceLeaseErr := s.Store.MaintenanceExpiredLeaseCount(now)
	checks["stamp_leases"] = stampLeaseErr == nil && stampExpired == 0
	checks["verification_leases"] = verificationLeaseErr == nil && verificationExpired == 0
	checks["maintenance_leases"] = maintenanceLeaseErr == nil && maintenanceExpired == 0
	ok := err == nil && verifyErr == nil && maintenanceErr == nil &&
		stampLeaseErr == nil && verificationLeaseErr == nil && maintenanceLeaseErr == nil
	for _, check := range checks {
		ok = ok && check
	}
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": ok, "checks": checks, "stamp_jobs": jobs, "verification_jobs": verifyJobs,
		"maintenance_jobs": maintenanceJobs,
	})
}

func writableDir(dir string) bool {
	f, err := os.CreateTemp(dir, ".ready-*")
	if err != nil {
		return false
	}
	name := f.Name()
	ok := f.Close() == nil && os.Remove(name) == nil
	return ok
}
