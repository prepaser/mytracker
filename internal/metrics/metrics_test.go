package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/metrics"
)

func TestHandler(t *testing.T) {
	metrics.IncLocation()
	metrics.IncExport()
	metrics.IncStampFail("tsa")
	metrics.SetStampQueue(2)
	metrics.SetStampJobs(1, 1, 1, 1, time.Now().Add(-time.Minute).Unix())
	metrics.ObserveStamp("tsa", time.Second)
	metrics.AddRetentionDeleted(2)
	metrics.StampJob("retry", "tsa")
	metrics.LastStampSuccess("tsa")
	metrics.Ingest("http", "stored")
	metrics.SetMQTTConnected(true)
	metrics.SetMQTTConnected(false)
	metrics.Backup("ok")
	metrics.Backup("fail")
	metrics.StampLease("renewed", 1)
	metrics.ProofGC("deleted", 1)
	metrics.HTTP("GET", 200, time.Millisecond)
	metrics.Restore("ok")
	metrics.Verification("ots", "verified", "manual", time.Millisecond)
	metrics.VerificationAttempt("esplora-1", "verified")
	metrics.SetVerificationStale(2)
	metrics.SetVerificationPolicyStale(1)
	metrics.VerificationJob("enqueued", "tsa")
	metrics.VerificationJob("coalesced", "ots")
	metrics.VerificationHistoryCleanup("ok")
	metrics.VerificationHistoryCleanup("error")
	metrics.VerificationLease("renewed", 1)
	metrics.SetVerificationJobs(1, 2, 3, 4, time.Now().Add(-time.Minute).Unix())
	metrics.SetVerificationJobs(0, 0, 0, 0, 0)
	metrics.SetVerificationWorkerHealthy(true)
	metrics.SetVerificationWorkerHealthy(false)
	metrics.TSATrust("nonce", "matched")
	metrics.OTSAnchor("pow", "valid")
	metrics.OTSAnchor("pow", "unexpected")
	metrics.MaintenanceEvent("backup", "enqueued")
	metrics.MaintenanceRun("backup", "success", time.Second)
	metrics.MaintenanceLease("renewed", 1)
	metrics.SetMaintenanceJob("backup", "scheduled", time.Now().Unix(), time.Now().Unix())
	metrics.SetMaintenanceWorkerHealthy(true)
	metrics.SetMaintenanceWorkerHealthy(false)
	metrics.SSEConnection("admin", 1)
	metrics.SSEConnection("admin", -1)
	metrics.SSEEvent("admin", "change", "sent")
	metrics.SSEDuration("admin", time.Second)
	rr := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "mytracker_locations_ingested_total") {
		t.Fatalf("missing metric: %s", body)
	}
	if strings.Contains(body, `result="unexpected"`) || !strings.Contains(body, `result="unknown"`) {
		t.Fatalf("unbounded OTS result label: %s", body)
	}
	for _, name := range []string{
		"mytracker_stamp_jobs", "mytracker_ingest_total", "mytracker_backups_total",
		"mytracker_mqtt_connected", "mytracker_retention_deleted_total",
		"mytracker_stamp_lease_events_total", "mytracker_proof_gc_total",
		"mytracker_http_requests_total", "mytracker_restore_total",
		"mytracker_proof_verifications_total", "mytracker_proof_verification_provider_attempts_total",
		"mytracker_proof_verification_stale",
		"mytracker_proof_verification_policy_stale", "mytracker_tsa_trust_checks_total",
		"mytracker_ots_anchor_checks_total", "mytracker_proof_verification_jobs",
		"mytracker_proof_verification_jobs_total",
		"mytracker_proof_verification_lease_events_total",
		"mytracker_proof_verification_oldest_pending_seconds",
		"mytracker_proof_verification_worker_healthy",
		"mytracker_maintenance_jobs", "mytracker_maintenance_jobs_total",
		"mytracker_maintenance_duration_seconds", "mytracker_maintenance_lease_events_total",
		"mytracker_maintenance_next_run_unixtime",
		"mytracker_maintenance_last_success_unixtime",
		"mytracker_maintenance_worker_healthy",
		"mytracker_sse_connections", "mytracker_sse_events_total",
		"mytracker_sse_connection_duration_seconds",
	} {
		if !strings.Contains(body, name) {
			t.Fatalf("missing %s", name)
		}
	}
}

func TestHTTPMethodLabelsAreBounded(t *testing.T) {
	methods := []string{"REVIEWCUSTOM", "reviewcustom", "get", "GET-OTHER", ""}
	for _, method := range methods {
		metrics.HTTP(method, http.StatusMethodNotAllowed, time.Millisecond)
	}
	metrics.HTTP(http.MethodGet, http.StatusOK, time.Millisecond)
	metrics.HTTP(http.MethodOptions, http.StatusMethodNotAllowed, time.Millisecond)
	rr := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()
	for _, method := range methods {
		if strings.Contains(body, `method="`+method+`"`) {
			t.Fatalf("unbounded method label %q", method)
		}
	}
	for _, sample := range []string{
		`mytracker_http_requests_total{method="OTHER",status="405"}`,
		`mytracker_http_request_duration_seconds_count{method="OTHER"}`,
		`mytracker_http_requests_total{method="GET",status="200"}`,
		`mytracker_http_requests_total{method="OPTIONS",status="405"}`,
	} {
		if !strings.Contains(body, sample) {
			t.Fatalf("missing %s", sample)
		}
	}
}
