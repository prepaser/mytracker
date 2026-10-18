package store

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestGeoDistanceAntipodalIsFinite(t *testing.T) {
	got := geoDistance(88.2, 0, -88.2, 180)
	want := math.Pi * 6371008.8
	if math.IsNaN(got) || math.IsInf(got, 0) || math.Abs(got-want) > 1e-6 {
		t.Fatalf("distance=%f want=%f", got, want)
	}
}

func TestGeofenceWriteRollsBackWhenStateRebuildFails(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err = st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.Exec(`CREATE TRIGGER reject_geofence_state BEFORE INSERT ON geofence_states BEGIN SELECT RAISE(ABORT,'state failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = st.CreateGeofence(&Geofence{Name: "Home", Lat: 0, Lon: 0, RadiusM: 100, Enabled: true}); err == nil {
		t.Fatal("create succeeded")
	}
	if fences, err := st.ListGeofences(); err != nil || len(fences) != 0 {
		t.Fatalf("fences=%+v err=%v", fences, err)
	}
	if _, err = st.db.Exec(`DROP TRIGGER reject_geofence_state`); err != nil {
		t.Fatal(err)
	}
	fence, err := st.CreateGeofence(&Geofence{Name: "Home", Lat: 0, Lon: 0, RadiusM: 100, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.db.Exec(`CREATE TRIGGER reject_geofence_state BEFORE INSERT ON geofence_states BEGIN SELECT RAISE(ABORT,'state failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = st.UpdateGeofence(fence.ID, &Geofence{Name: "Changed", Lat: 0, Lon: 0, RadiusM: 100, Enabled: true}); err == nil {
		t.Fatal("update succeeded")
	}
	fences, err := st.ListGeofences()
	if err != nil || len(fences) != 1 || fences[0].Name != "Home" {
		t.Fatalf("fences=%+v err=%v", fences, err)
	}
}

func TestRebuildGeofenceStateBatchesLatestLocations(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	const identities = 257
	for i := range identities {
		user := fmt.Sprintf("user-%03d", i)
		device := "phone"
		if i == 0 {
			user, device = "", ""
		}
		for j, lat := range []float64{1, 0} {
			if _, err := st.Insert(&Location{
				User: user, Device: device, Tst: int64(100 + j), Lat: lat, Lon: 0,
				Payload: []byte(`{"_type":"location"}`), Digest: make([]byte, 32),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	if _, err := st.CreateGeofence(&Geofence{Name: "Home", Lat: 0, Lon: 0, RadiusM: 25, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var states, inside int
	if err := st.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(inside),0) FROM geofence_states`).Scan(&states, &inside); err != nil {
		t.Fatal(err)
	}
	if states != identities || inside != identities {
		t.Fatalf("states=%d inside=%d, want %d latest states inside", states, inside, identities)
	}
}

func TestBulkDeleteRebuildsGeofenceStateAcrossBatchBoundary(t *testing.T) {
	root := t.TempDir()
	proofs := filepath.Join(root, "proofs")
	if err := os.MkdirAll(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.CreateGeofence(&Geofence{Name: "Home", Lat: 0, Lon: 0, RadiusM: 100, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	const locations = 129
	for i := range locations {
		if _, err := st.Insert(&Location{
			User: fmt.Sprintf("user-%03d", i), Device: "phone", Tst: int64(100 + i),
			Payload: []byte(`{"_type":"location"}`), Digest: make([]byte, 32),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var states int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM geofence_states`).Scan(&states); err != nil {
		t.Fatal(err)
	}
	if states != locations {
		t.Fatalf("states before delete=%d", states)
	}
	if n, err := st.DeleteRange(proofs, ExportFilter{From: 100, To: 300}); err != nil || n != locations {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM geofence_states`).Scan(&states); err != nil {
		t.Fatal(err)
	}
	if states != 0 {
		t.Fatalf("states after delete=%d", states)
	}
}
