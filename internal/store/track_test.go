package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/prepaser/mytracker/internal/store"
)

func TestListTrack(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	for _, loc := range []*store.Location{
		{User: "a", Device: "p", Tst: 300, Lat: 3, Lon: 3, Payload: []byte("{}"), Digest: []byte{1}},
		{User: "a", Device: "p", Tst: 100, Lat: 1, Lon: 1, Payload: []byte("{}"), Digest: []byte{2}},
		{User: "b", Device: "x", Tst: 200, Lat: 2, Lon: 2, Payload: []byte("{}"), Digest: []byte{3}},
	} {
		if _, err := st.Insert(loc); err != nil {
			t.Fatal(err)
		}
	}

	locs, err := st.ListTrack(store.TrackFilter{User: "a", Device: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(locs) != 2 || locs[0].Tst != 100 || locs[1].Tst != 300 {
		t.Fatalf("order: %+v", locs)
	}

	from := int64(150)
	locs, err = st.ListTrack(store.TrackFilter{From: from})
	if err != nil || len(locs) != 2 {
		t.Fatalf("from filter: %d err=%v", len(locs), err)
	}

	ids, err := st.ListIdentities()
	if err != nil || len(ids) != 2 {
		t.Fatalf("identities: %v", ids)
	}
}

func TestTrackSnapshotKeysetAndIsolation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for i, device := range []string{"a", "b", "c"} {
		if _, err := st.Insert(&store.Location{
			User: "u", Device: device, Tst: 100,
			Lat: float64(i), Lon: float64(i), Payload: []byte("{}"), Digest: []byte{byte(i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := st.BeginTrackSnapshot(store.TrackFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	if _, err := st.Insert(&store.Location{
		User: "u", Device: "d", Tst: 101,
		Lat: 4, Lon: 4, Payload: []byte("{}"), Digest: []byte{4},
	}); err != nil {
		t.Fatal(err)
	}
	var got []int64
	if err := snapshot.IteratePoints(1, func(points []store.TrackPoint) error {
		got = append(got, points[0].ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] >= got[1] || got[1] >= got[2] {
		t.Fatalf("ids=%v", got)
	}
	locs, err := snapshot.LoadLocations(got)
	if err != nil || len(locs) != 3 || locs[0].ID != got[0] {
		t.Fatalf("locations=%v err=%v", locs, err)
	}
}

func TestExportSnapshotExcludesConcurrentInsert(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "export-snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for i := range 2 {
		if _, err := st.Insert(&store.Location{
			User: "u", Device: "d", Tst: int64(i + 1),
			Lat: 1, Lon: 1, Payload: []byte("{}"), Digest: []byte{byte(i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := st.BeginExportSnapshot(store.ExportFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	if _, err := st.Insert(&store.Location{
		User: "u", Device: "d", Tst: 3,
		Lat: 1, Lon: 1, Payload: []byte("{}"), Digest: []byte{3},
	}); err != nil {
		t.Fatal(err)
	}
	count, err := snapshot.Count()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	if err := snapshot.Iterate(1, func(locs []store.Location) error {
		seen += len(locs)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 2 || seen != 2 {
		t.Fatalf("count=%d seen=%d", count, seen)
	}
}

func TestStorageStatsIndexesAndFailedIteration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "storage.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for i, status := range []string{"fail", "pending", "fail"} {
		id, err := st.Insert(&store.Location{
			User: "u", Device: "d", Tst: int64(i + 1),
			Lat: 1, Lon: 1, Payload: []byte("{}"), Digest: []byte{byte(i)},
			TsaStatus: status, OtsStatus: "pending",
		})
		if err != nil {
			t.Fatal(err)
		}
		if status == "fail" {
			if err := st.UpdateTSA(id, "fail", nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	stats, err := st.StorageStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Locations != 3 || stats.LogicalBytes <= 0 ||
		stats.OldestAt == "" || stats.NewestAt == "" {
		t.Fatalf("stats=%+v", stats)
	}
	if missing, err := st.MissingRangeIndexes(); err != nil || len(missing) != 0 {
		t.Fatalf("missing=%v err=%v", missing, err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP INDEX idx_locations_device_tst_id`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	missing, err := st.MissingRangeIndexes()
	if err != nil || len(missing) != 1 || missing[0] != "idx_locations_device_tst_id" {
		t.Fatalf("missing=%v err=%v", missing, err)
	}
	seen := 0
	if err := st.IterateFailed(1, func(locs []store.Location) error {
		seen += len(locs)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Fatalf("failed=%d", seen)
	}
	snapshot, err := st.BeginTrackSnapshot(store.TrackFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Limit() != 5000 {
		t.Fatalf("limit=%d", snapshot.Limit())
	}
	seen = 0
	if err := snapshot.Iterate(2, func(locs []store.Location) error {
		seen += len(locs)
		return nil
	}); err != nil || seen != 3 {
		t.Fatalf("track iterate=%d err=%v", seen, err)
	}
	_ = snapshot.Close()
	snapshot, err = st.BeginTrackSnapshot(store.TrackFilter{})
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("stop")
	if err := snapshot.Iterate(1, func([]store.Location) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("callback error=%v", err)
	}
	_ = snapshot.Close()
}
