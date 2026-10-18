package web

import (
	"fmt"
	"net/http"
)

func (s *Server) routes() error {
	static, err := staticHandler()
	if err != nil {
		return err
	}
	s.mux.Handle("GET /static/", static)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.HandleFunc("POST /login", s.handleLogin)
	s.mux.HandleFunc("POST /logout", s.auth(s.handleLogout))
	s.mux.HandleFunc("GET /share/{token}", s.handleSharePage)
	s.mux.HandleFunc("GET /share/{token}/api/track", s.handleShareTrack)
	s.mux.HandleFunc("GET /share/{token}/api/events", s.handleShareEvents)
	s.mux.HandleFunc("GET /", s.auth(s.handleDashboard))
	s.mux.HandleFunc("GET /map", s.auth(s.handleMap))
	s.mux.HandleFunc("GET /timeline", s.auth(s.handleTimeline))
	s.mux.HandleFunc("GET /settings", s.auth(s.handleSettings))
	s.mux.HandleFunc("POST /settings", s.auth(s.handleSettingsSave))
	s.mux.HandleFunc("POST /api/settings", s.auth(s.handleSettingsSave))
	s.mux.HandleFunc("GET /api/track", s.auth(s.apiTrack))
	s.mux.HandleFunc("GET /api/timeline", s.auth(s.apiTimeline))
	s.mux.HandleFunc("GET /api/geofences", s.auth(s.apiGeofences))
	s.mux.HandleFunc("POST /api/geofences", s.auth(s.apiGeofenceCreate))
	s.mux.HandleFunc("PATCH /api/geofences/{id}", s.auth(s.apiGeofenceUpdate))
	s.mux.HandleFunc("DELETE /api/geofences/{id}", s.auth(s.apiGeofenceDelete))
	s.mux.HandleFunc("GET /api/geofence-events", s.auth(s.apiGeofenceEvents))
	s.mux.HandleFunc("GET /api/locations", s.auth(s.apiLocations))
	s.mux.HandleFunc("GET /api/status", s.auth(s.apiStatus))
	s.mux.HandleFunc("GET /api/events", s.auth(s.apiEvents))
	s.mux.HandleFunc("GET /api/stamp-jobs", s.auth(s.apiStampJobs))
	s.mux.HandleFunc("GET /api/verification-jobs", s.auth(s.apiVerificationJobs))
	s.mux.HandleFunc("GET /api/maintenance-jobs", s.auth(s.apiMaintenanceJobs))
	s.mux.HandleFunc("GET /api/maintenance-runs", s.auth(s.apiMaintenanceRuns))
	s.mux.HandleFunc("POST /api/maintenance-jobs/{id}/retry", s.auth(s.apiMaintenanceRetry))
	s.mux.HandleFunc("POST /api/stamps/{id}/retry", s.auth(s.apiStampRetry))
	s.mux.HandleFunc("POST /api/stamps/retry", s.auth(s.apiStampRetryAll))
	s.mux.HandleFunc("POST /api/backups", s.auth(s.apiBackup))
	s.mux.HandleFunc("POST /api/owntracks-url", s.auth(s.apiOwnTracksURL))
	s.mux.HandleFunc("GET /api/sessions", s.auth(s.apiSessions))
	s.mux.HandleFunc("DELETE /api/sessions/{id}", s.auth(s.apiSessionDelete))
	s.mux.HandleFunc("POST /api/sessions/revoke-others", s.auth(s.apiSessionsRevokeOthers))
	s.mux.HandleFunc("GET /api/stats", s.auth(s.apiStats))
	s.mux.HandleFunc("GET /api/identities", s.auth(s.apiIdentities))
	s.mux.HandleFunc("POST /api/ots/upgrade", s.auth(s.apiOTSUpgrade))
	s.mux.HandleFunc("GET /api/export", s.auth(s.apiExport))
	s.mux.HandleFunc("GET /api/export/gpx", s.auth(s.apiGPXExport))
	s.mux.HandleFunc("GET /api/shares", s.auth(s.apiSharesList))
	s.mux.HandleFunc("POST /api/shares", s.auth(s.apiShareCreate))
	s.mux.HandleFunc("POST /api/shares/{id}/rotate", s.auth(s.apiShareRotate))
	s.mux.HandleFunc("DELETE /api/shares/{id}", s.auth(s.apiShareDelete))
	s.mux.HandleFunc("POST /api/import", s.auth(s.apiImport))
	s.mux.HandleFunc("POST /api/import/gpx", s.auth(s.apiGPXImport))
	s.mux.HandleFunc("GET /api/delete-preview", s.auth(s.apiDeletePreview))
	s.mux.HandleFunc("POST /api/delete", s.auth(s.apiDelete))
	s.mux.HandleFunc("GET /metrics", s.auth(s.apiMetrics))
	s.mux.HandleFunc("GET /api/verify/{id}", s.auth(s.apiVerify))
	s.mux.HandleFunc("POST /api/verify/{id}", s.auth(s.apiVerifyEnqueue))
	s.mux.HandleFunc("GET /api/verify/{id}/history", s.auth(s.apiVerifyHistory))
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		s.mux.HandleFunc(method+" /api/", func(w http.ResponseWriter, r *http.Request) {
			jsonErr(w, fmt.Errorf("API endpoint not found"), http.StatusNotFound)
		})
	}
	return nil
}
