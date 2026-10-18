package metrics

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const workerHeartbeatMaxAge = 30 * time.Second

var (
	verificationWorkerLastHeartbeat atomic.Int64
	maintenanceWorkerLastHeartbeat  atomic.Int64
	stampOldestPending              atomic.Int64
	verificationOldestPending       atomic.Int64
)

func heartbeatHealthy(last int64) float64 {
	if last == 0 || time.Since(time.Unix(0, last)) >= workerHeartbeatMaxAge {
		return 0
	}
	return 1
}

func pendingAge(now time.Time, oldest int64) float64 {
	if oldest <= 0 {
		return 0
	}
	age := now.Sub(time.Unix(oldest, 0)).Seconds()
	if age < 0 {
		return 0
	}
	return age
}

var (
	locationsIngested = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mytracker_locations_ingested_total",
		Help: "Total locations ingested",
	})
	stampFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_stamp_failures_total",
		Help: "Stamp failures by kind",
	}, []string{"kind"})
	exportTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mytracker_exports_total",
		Help: "Total export downloads",
	})
	stampQueue = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mytracker_stamp_queue_depth",
		Help: "Current stamp job queue depth",
	})
	stampJobStates = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mytracker_stamp_jobs",
		Help: "Current durable stamp jobs by state",
	}, []string{"state"})
	stampOldest = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "mytracker_stamp_oldest_pending_seconds",
		Help: "Age of the oldest pending stamp job",
	}, func() float64 {
		return pendingAge(time.Now(), stampOldestPending.Load())
	})
	stampDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mytracker_stamp_duration_seconds",
		Help:    "Stamp job duration by kind",
		Buckets: prometheus.DefBuckets,
	}, []string{"kind"})
	retentionDeleted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mytracker_retention_deleted_total",
		Help: "Locations deleted by retention",
	})
	stampJobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_stamp_jobs_total",
		Help: "Stamp jobs by result and kind",
	}, []string{"result", "kind"})
	lastStampSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mytracker_stamp_last_success_unixtime",
		Help: "Unix time of the last successful stamp",
	}, []string{"kind"})
	ingestResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_ingest_total",
		Help: "Ingest attempts by transport and result",
	}, []string{"transport", "result"})
	mqttConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mytracker_mqtt_connected",
		Help: "Whether MQTT is currently connected",
	})
	backupResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_backups_total",
		Help: "Backup attempts by result",
	}, []string{"result"})
	lastBackup = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mytracker_backup_last_success_unixtime",
		Help: "Unix time of the last successful backup",
	})
	stampLeaseEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_stamp_lease_events_total",
		Help: "Stamp lease events by result",
	}, []string{"result"})
	proofGCEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_proof_gc_total",
		Help: "Proof garbage collection outcomes",
	}, []string{"result"})
	httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_http_requests_total",
		Help: "HTTP requests by method and status",
	}, []string{"method", "status"})
	httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mytracker_http_request_duration_seconds",
		Help:    "HTTP request duration by method",
		Buckets: prometheus.DefBuckets,
	}, []string{"method"})
	restoreEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_restore_total",
		Help: "Restore outcomes",
	}, []string{"result"})
	verificationResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_proof_verifications_total",
		Help: "Proof verification outcomes",
	}, []string{"kind", "result", "trigger"})
	verificationDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mytracker_proof_verification_duration_seconds",
		Help:    "Proof verification duration by kind",
		Buckets: prometheus.DefBuckets,
	}, []string{"kind"})
	verificationAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_proof_verification_provider_attempts_total",
		Help: "Proof verification provider attempts",
	}, []string{"provider", "result"})
	verificationStale = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mytracker_proof_verification_stale",
		Help: "Locations with missing or stale proof verification",
	})
	verificationLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mytracker_proof_verification_last_success_unixtime",
		Help: "Last successful proof verification",
	}, []string{"kind"})
	verificationPolicyStale = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "mytracker_proof_verification_policy_stale",
		Help: "Locations whose latest proof result used an older trust policy",
	})
	verificationJobStates = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mytracker_proof_verification_jobs",
		Help: "Current durable proof verification jobs by state",
	}, []string{"state"})
	verificationJobEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_proof_verification_jobs_total",
		Help: "Proof verification job events by result and kind",
	}, []string{"result", "kind"})
	verificationJobOldest = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "mytracker_proof_verification_oldest_pending_seconds",
		Help: "Age of the oldest pending proof verification job",
	}, func() float64 {
		return pendingAge(time.Now(), verificationOldestPending.Load())
	})
	verificationLeaseEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_proof_verification_lease_events_total",
		Help: "Proof verification lease events by result",
	}, []string{"result"})
	verificationWorkerHealthy = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "mytracker_proof_verification_worker_healthy",
		Help: "Whether proof verification workers have a recent heartbeat",
	}, func() float64 {
		return heartbeatHealthy(verificationWorkerLastHeartbeat.Load())
	})
	verificationHistoryCleanup = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_proof_verification_history_cleanup_total",
		Help: "Proof verification history cleanup outcomes",
	}, []string{"result"})
	tsaTrustChecks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_tsa_trust_checks_total",
		Help: "TSA trust checks by fixed check and result",
	}, []string{"check", "result"})
	otsAnchorChecks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_ots_anchor_checks_total",
		Help: "OTS Bitcoin anchor checks by fixed check and result",
	}, []string{"check", "result"})
	maintenanceJobStates = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mytracker_maintenance_jobs",
		Help: "Current durable maintenance jobs by kind and state",
	}, []string{"kind", "state"})
	maintenanceEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_maintenance_jobs_total",
		Help: "Maintenance job events by kind and event",
	}, []string{"kind", "event"})
	maintenanceDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mytracker_maintenance_duration_seconds",
		Help:    "Maintenance run duration by kind and result",
		Buckets: prometheus.DefBuckets,
	}, []string{"kind", "result"})
	maintenanceLeaseEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_maintenance_lease_events_total",
		Help: "Maintenance lease events",
	}, []string{"event"})
	maintenanceNextRun = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mytracker_maintenance_next_run_unixtime",
		Help: "Next scheduled maintenance run by kind",
	}, []string{"kind"})
	maintenanceLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mytracker_maintenance_last_success_unixtime",
		Help: "Last successful maintenance run by kind",
	}, []string{"kind"})
	maintenanceWorkerHealthy = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "mytracker_maintenance_worker_healthy",
		Help: "Whether the maintenance worker has a recent heartbeat",
	}, func() float64 {
		return heartbeatHealthy(maintenanceWorkerLastHeartbeat.Load())
	})
	dataDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mytracker_data_operation_duration_seconds",
		Help:    "Track and export processing duration",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation", "sampled"})
	dataPoints = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mytracker_data_operation_points",
		Help:    "Points returned by track and export operations",
		Buckets: []float64{10, 100, 1000, 5000, 10000, 20000, 100000},
	}, []string{"operation", "sampled"})
	sseConnections = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mytracker_sse_connections",
		Help: "Active SSE connections by audience",
	}, []string{"audience"})
	sseEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mytracker_sse_events_total",
		Help: "SSE outcomes by audience, topic, and result",
	}, []string{"audience", "topic", "result"})
	sseDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mytracker_sse_connection_duration_seconds",
		Help:    "SSE connection duration by audience",
		Buckets: []float64{10, 60, 300, 1800, 7200, 21600, 86400},
	}, []string{"audience"})
	timelineRebuilds   = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mytracker_timeline_rebuilds_total", Help: "Timeline rebuild outcomes"}, []string{"result"})
	timelineDuration   = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "mytracker_timeline_rebuild_duration_seconds", Help: "Timeline rebuild duration", Buckets: prometheus.DefBuckets})
	timelineBacklog    = prometheus.NewGauge(prometheus.GaugeOpts{Name: "mytracker_timeline_rebuild_backlog", Help: "Pending timeline rebuild identities"})
	geofenceEvents     = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mytracker_geofence_events_total", Help: "Geofence transitions"}, []string{"event"})
	geofenceDeliveries = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mytracker_geofence_deliveries_total", Help: "Geofence webhook delivery outcomes"}, []string{"result"})
	gpxOperations      = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mytracker_gpx_operations_total", Help: "GPX operations"}, []string{"operation", "result"})
)

func init() {
	prometheus.MustRegister(locationsIngested, stampFailures, exportTotal, stampQueue, stampJobStates, stampOldest, stampDuration,
		retentionDeleted, stampJobs, lastStampSuccess, ingestResults, mqttConnected, backupResults, lastBackup,
		stampLeaseEvents, proofGCEvents, httpRequests, httpDuration, restoreEvents,
		verificationResults, verificationDuration, verificationAttempts, verificationStale,
		verificationLastSuccess, verificationPolicyStale, verificationJobStates,
		verificationJobEvents, verificationJobOldest, verificationLeaseEvents,
		verificationWorkerHealthy, verificationHistoryCleanup, tsaTrustChecks, otsAnchorChecks,
		maintenanceJobStates, maintenanceEvents, maintenanceDuration,
		maintenanceLeaseEvents, maintenanceNextRun, maintenanceLastSuccess,
		maintenanceWorkerHealthy, dataDuration, dataPoints,
		sseConnections, sseEvents, sseDuration, timelineRebuilds, timelineDuration, timelineBacklog,
		geofenceEvents, geofenceDeliveries, gpxOperations)
}

func Handler() http.Handler { return promhttp.Handler() }

func IncLocation() { locationsIngested.Inc() }

func VerificationHistoryCleanup(result string) {
	verificationHistoryCleanup.WithLabelValues(result).Inc()
}

func Timeline(result string, d time.Duration, backlog int) {
	timelineRebuilds.WithLabelValues(result).Inc()
	timelineDuration.Observe(d.Seconds())
	timelineBacklog.Set(float64(backlog))
}
func GeofenceEvent(event string)     { geofenceEvents.WithLabelValues(event).Inc() }
func GeofenceDelivery(result string) { geofenceDeliveries.WithLabelValues(result).Inc() }
func GPX(operation, result string)   { gpxOperations.WithLabelValues(operation, result).Inc() }

func IncStampFail(kind string) { stampFailures.WithLabelValues(kind).Inc() }

func IncExport() { exportTotal.Inc() }

func SetStampQueue(n int) { stampQueue.Set(float64(n)) }

func SetStampJobs(queued, running, retrying, failed int, oldest int64) {
	stampJobStates.WithLabelValues("queued").Set(float64(queued))
	stampJobStates.WithLabelValues("running").Set(float64(running))
	stampJobStates.WithLabelValues("retry").Set(float64(retrying))
	stampJobStates.WithLabelValues("failed").Set(float64(failed))
	SetStampQueue(queued + running + retrying)
	stampOldestPending.Store(oldest)
}

func ObserveStamp(kind string, d time.Duration) {
	stampDuration.WithLabelValues(kind).Observe(d.Seconds())
}

func AddRetentionDeleted(n int64) { retentionDeleted.Add(float64(n)) }

func StampJob(result, kind string) { stampJobs.WithLabelValues(result, kind).Inc() }

func LastStampSuccess(kind string) { lastStampSuccess.WithLabelValues(kind).SetToCurrentTime() }

func Ingest(transport, result string) { ingestResults.WithLabelValues(transport, result).Inc() }

func SetMQTTConnected(ok bool) {
	if ok {
		mqttConnected.Set(1)
	} else {
		mqttConnected.Set(0)
	}
}

func Backup(result string) {
	backupResults.WithLabelValues(result).Inc()
	if result == "ok" {
		lastBackup.SetToCurrentTime()
	}
}

func StampLease(result string, n int) {
	stampLeaseEvents.WithLabelValues(result).Add(float64(n))
}

func ProofGC(result string, n int) {
	proofGCEvents.WithLabelValues(result).Add(float64(n))
}

func HTTP(method string, status int, duration time.Duration, stream ...bool) {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
	default:
		method = "OTHER"
	}
	httpRequests.WithLabelValues(method, strconv.Itoa(status)).Inc()
	if len(stream) == 0 || !stream[0] {
		httpDuration.WithLabelValues(method).Observe(duration.Seconds())
	}
}

func SSEConnection(audience string, delta float64) {
	sseConnections.WithLabelValues(audience).Add(delta)
}

func SSEEvent(audience, topic, result string) {
	sseEvents.WithLabelValues(audience, topic, result).Inc()
}

func SSEDuration(audience string, duration time.Duration) {
	sseDuration.WithLabelValues(audience).Observe(duration.Seconds())
}

func Restore(result string) {
	restoreEvents.WithLabelValues(result).Inc()
}

func Verification(kind, result, trigger string, duration time.Duration) {
	verificationResults.WithLabelValues(kind, result, trigger).Inc()
	verificationDuration.WithLabelValues(kind).Observe(duration.Seconds())
	if result == "verified" || result == "degraded" {
		verificationLastSuccess.WithLabelValues(kind).SetToCurrentTime()
	}
}

func VerificationAttempt(provider, result string) {
	verificationAttempts.WithLabelValues(provider, result).Inc()
}

func SetVerificationStale(n int) {
	verificationStale.Set(float64(n))
}

func SetVerificationPolicyStale(n int) {
	verificationPolicyStale.Set(float64(n))
}

func VerificationJob(result, kind string) {
	verificationJobEvents.WithLabelValues(result, kind).Inc()
}

func VerificationLease(result string, n int) {
	verificationLeaseEvents.WithLabelValues(result).Add(float64(n))
}

func SetVerificationJobs(queued, running, retrying, failed int, oldest int64) {
	verificationJobStates.WithLabelValues("queued").Set(float64(queued))
	verificationJobStates.WithLabelValues("running").Set(float64(running))
	verificationJobStates.WithLabelValues("retry").Set(float64(retrying))
	verificationJobStates.WithLabelValues("failed").Set(float64(failed))
	verificationOldestPending.Store(oldest)
}

func SetVerificationWorkerHealthy(ok bool) {
	if ok {
		verificationWorkerLastHeartbeat.Store(time.Now().UnixNano())
	} else {
		verificationWorkerLastHeartbeat.Store(0)
	}
}

func TSATrust(check, result string) {
	tsaTrustChecks.WithLabelValues(check, result).Inc()
}

func OTSAnchor(check, result string) {
	if check != "header" && check != "pow" && check != "merkle" && check != "confirmations" {
		return
	}
	switch result {
	case "valid", "invalid", "unknown", "pending", "mature", "unavailable":
	default:
		result = "unknown"
	}
	otsAnchorChecks.WithLabelValues(check, result).Inc()
}

func MaintenanceEvent(kind, event string) {
	maintenanceEvents.WithLabelValues(kind, event).Inc()
}

func MaintenanceRun(kind, result string, duration time.Duration) {
	maintenanceDuration.WithLabelValues(kind, result).Observe(duration.Seconds())
}

func MaintenanceLease(event string, n int) {
	maintenanceLeaseEvents.WithLabelValues(event).Add(float64(n))
}

func SetMaintenanceJob(kind, state string, nextRun, lastSuccess int64) {
	for _, candidate := range []string{"scheduled", "queued", "running", "retry", "failed", "disabled"} {
		value := float64(0)
		if candidate == state {
			value = 1
		}
		maintenanceJobStates.WithLabelValues(kind, candidate).Set(value)
	}
	maintenanceNextRun.WithLabelValues(kind).Set(float64(nextRun))
	maintenanceLastSuccess.WithLabelValues(kind).Set(float64(lastSuccess))
}

func SetMaintenanceWorkerHealthy(ok bool) {
	if ok {
		maintenanceWorkerLastHeartbeat.Store(time.Now().UnixNano())
	} else {
		maintenanceWorkerLastHeartbeat.Store(0)
	}
}

func ObserveData(operation string, sampled bool, duration time.Duration, points int) {
	if operation != "track" && operation != "export" {
		return
	}
	sampledLabel := strconv.FormatBool(sampled)
	dataDuration.WithLabelValues(operation, sampledLabel).Observe(duration.Seconds())
	dataPoints.WithLabelValues(operation, sampledLabel).Observe(float64(points))
}
