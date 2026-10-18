package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWorkerHealthMetricsExpireAtScrape(t *testing.T) {
	verificationPrevious := verificationWorkerLastHeartbeat.Load()
	maintenancePrevious := maintenanceWorkerLastHeartbeat.Load()
	t.Cleanup(func() {
		verificationWorkerLastHeartbeat.Store(verificationPrevious)
		maintenanceWorkerLastHeartbeat.Store(maintenancePrevious)
	})
	expired := time.Now().Add(-workerHeartbeatMaxAge - time.Second).UnixNano()
	verificationWorkerLastHeartbeat.Store(expired)
	maintenanceWorkerLastHeartbeat.Store(expired)

	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	for _, metric := range []string{
		"mytracker_proof_verification_worker_healthy",
		"mytracker_maintenance_worker_healthy",
	} {
		if !strings.Contains(body, metric+" 0\n") {
			t.Fatalf("expired %s metric missing zero sample", metric)
		}
	}
}

func TestPendingAgeUsesCurrentTime(t *testing.T) {
	now := time.Unix(200, 0)
	if got := pendingAge(now, 100); got != 100 {
		t.Fatalf("age=%v", got)
	}
	if got := pendingAge(now, 201); got != 0 {
		t.Fatalf("future age=%v", got)
	}
	if got := pendingAge(now, 0); got != 0 {
		t.Fatalf("empty age=%v", got)
	}
}
