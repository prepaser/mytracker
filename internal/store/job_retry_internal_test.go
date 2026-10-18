package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRecoveryDoesNotRequeueExhaustedJobs(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := int64(100)
	if _, err := st.db.Exec(`
		INSERT INTO timeline_rebuild_jobs(user,device,dirty_from,state,claimed_generation,attempts,lease_owner,lease_until,created_at,updated_at)
		VALUES ('u','d',1,'running',1,5,'owner',200,?,?);
		INSERT INTO timeline_rebuild_jobs(user,device,dirty_from,state,generation,claimed_generation,attempts,lease_owner,lease_until,created_at,updated_at)
		VALUES ('new','d',1,'running',2,1,5,'owner',200,?,?)`, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`
		INSERT INTO locations(id,user,device,tst,lat,lon,payload,digest,tsa_status,ots_status,created_at)
		VALUES (1,'u','d',1,0,0,'{}',x'00','pending','pending',?);
		INSERT INTO geofences(name,lat,lon,radius_m,enabled,created_at,updated_at)
		VALUES ('f',0,0,100,1,?,?);
		INSERT INTO geofence_events(geofence_id,location_id,user,device,event,tst,created_at)
		VALUES (1,1,'u','d','enter',1,?);
		INSERT INTO geofence_deliveries(event_id,state,attempts,lease_owner,lease_until,created_at,updated_at)
		VALUES (1,'running',5,'owner',200,?,?)`, now, now, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RecoverTimelineJobs(now); err != nil {
		t.Fatal(err)
	}
	if err := st.RecoverGeofenceDeliveries(now); err != nil {
		t.Fatal(err)
	}
	var timelineState, geofenceState string
	if err := st.db.QueryRow(`SELECT state FROM timeline_rebuild_jobs WHERE user='u' AND device='d'`).Scan(&timelineState); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT state FROM geofence_deliveries WHERE event_id=1`).Scan(&geofenceState); err != nil {
		t.Fatal(err)
	}
	if timelineState != "failed" || geofenceState != "failed" {
		t.Fatalf("timeline=%q geofence=%q", timelineState, geofenceState)
	}
	var supersededState string
	var supersededAttempts int
	if err := st.db.QueryRow(`SELECT state,attempts FROM timeline_rebuild_jobs WHERE user='new' AND device='d'`).Scan(&supersededState, &supersededAttempts); err != nil {
		t.Fatal(err)
	}
	if supersededState != "queued" || supersededAttempts != 0 {
		t.Fatalf("superseded state=%q attempts=%d", supersededState, supersededAttempts)
	}
	job, err := st.ClaimTimelineJob("next", now, now+90)
	if err != nil || job == nil || job.User != "new" || job.Attempts != 1 {
		t.Fatalf("superseded claim=%+v err=%v", job, err)
	}
	if _, err := st.db.Exec(`DELETE FROM timeline_rebuild_jobs WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if job, err := st.ClaimTimelineJob("next", now, now+90); err != nil || job != nil {
		t.Fatalf("timeline claim=%+v err=%v", job, err)
	}
	if delivery, err := st.ClaimGeofenceDelivery("next", now, now+90); err != nil || delivery != nil {
		t.Fatalf("geofence claim=%+v err=%v", delivery, err)
	}
}

func TestClaimFailsExpiredExhaustedJobsWithoutRestart(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "expired.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.db.Exec(`
		INSERT INTO timeline_rebuild_jobs(user,device,dirty_from,state,claimed_generation,attempts,lease_owner,lease_until,created_at,updated_at)
		VALUES ('u','d',1,'running',1,5,'old',99,1,1);
		INSERT INTO locations(id,user,device,tst,lat,lon,payload,digest,tsa_status,ots_status,created_at)
		VALUES (1,'u','d',1,0,0,'{}',x'00','pending','pending',1);
		INSERT INTO geofences(name,lat,lon,radius_m,enabled,created_at,updated_at)
		VALUES ('f',0,0,100,1,1,1);
		INSERT INTO geofence_events(geofence_id,location_id,user,device,event,tst,created_at)
		VALUES (1,1,'u','d','enter',1,1);
		INSERT INTO geofence_deliveries(event_id,state,attempts,lease_owner,lease_until,created_at,updated_at)
		VALUES (1,'running',5,'old',99,1,1)`); err != nil {
		t.Fatal(err)
	}
	if job, err := st.ClaimTimelineJob("new", 100, 190); err != nil || job != nil {
		t.Fatalf("timeline claim=%+v err=%v", job, err)
	}
	if job, err := st.ClaimGeofenceDelivery("new", 100, 190); err != nil || job != nil {
		t.Fatalf("geofence claim=%+v err=%v", job, err)
	}
	for table, id := range map[string]int{"timeline_rebuild_jobs": 1, "geofence_deliveries": 1} {
		var state string
		if err := st.db.QueryRow(`SELECT state FROM `+table+` WHERE id=?`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "failed" {
			t.Fatalf("%s state=%q", table, state)
		}
	}
}

func TestRenewGeofenceDelivery(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "renew.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.db.Exec(`
		INSERT INTO locations(id,user,device,tst,lat,lon,payload,digest,tsa_status,ots_status,created_at)
		VALUES (1,'u','d',1,0,0,'{}',x'00','pending','pending',1);
		INSERT INTO geofences(name,lat,lon,radius_m,enabled,created_at,updated_at)
		VALUES ('f',0,0,100,1,1,1);
		INSERT INTO geofence_events(geofence_id,location_id,user,device,event,tst,created_at)
		VALUES (1,1,'u','d','enter',1,1);
		INSERT INTO geofence_deliveries(event_id,state,created_at,updated_at)
		VALUES (1,'queued',1,1)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	delivery, err := st.ClaimGeofenceDelivery("owner", now, now+90)
	if err != nil || delivery == nil {
		t.Fatalf("delivery=%+v err=%v", delivery, err)
	}
	if ok, err := st.RenewGeofenceDelivery(delivery.ID, "owner", now+120); err != nil || !ok {
		t.Fatalf("renew=%t err=%v", ok, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.RenewGeofenceDeliveryContext(ctx, delivery.ID, "owner", now+150); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled renew err=%v", err)
	}
}

func TestCleanupGeofenceDeliveries(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.db.Exec(`
		INSERT INTO locations(id,user,device,tst,lat,lon,payload,digest,tsa_status,ots_status,created_at)
		VALUES (1,'u','d',1,0,0,'{}',x'00','pending','pending',1);
		INSERT INTO geofences(name,lat,lon,radius_m,enabled,created_at,updated_at)
		VALUES ('f',0,0,100,1,1,1);
		INSERT INTO geofence_events(id,geofence_id,location_id,user,device,event,tst,created_at)
		VALUES (1,1,1,'u','d','enter',1,1),(2,1,1,'u','d','exit',2,2),(3,1,1,'u','d','enter',3,3);
		INSERT INTO geofence_deliveries(event_id,state,created_at,updated_at)
		VALUES (1,'done',1,1),(2,'failed',1,1),(3,'failed',100,100)`); err != nil {
		t.Fatal(err)
	}
	n, err := st.CleanupGeofenceDeliveries(50)
	if err != nil || n != 2 {
		t.Fatalf("cleaned=%d err=%v", n, err)
	}
	status, err := st.GeofenceDeliveryDiagnostics(200)
	if err != nil || status.Failed != 1 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}
