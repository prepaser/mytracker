package track

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/prepaser/mytracker/internal/store"
)

func TestSampleDeterministicAndKeepsEndpoints(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "track.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var firstID, lastID int64
	for i := range 100 {
		loc := &store.Location{
			User: "u", Device: "d", Tst: int64(1000 + i),
			Lat: 37 + float64(i)/1000, Lon: 127,
			Payload: json.RawMessage(`{}`), Digest: make([]byte, 32),
		}
		if i == 50 {
			loc.Lon += 1
		}
		id, err := st.Insert(loc)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstID = id
		}
		lastID = id
	}

	run := func() Result {
		snapshot, err := st.BeginTrackSnapshot(store.TrackFilter{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = snapshot.Close() }()
		result, err := Sample(snapshot, WebMercator)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	a, b := run(), run()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("sampling is not deterministic")
	}
	if len(a.Locations) != 10 || !a.Metadata.Sampled ||
		a.Metadata.TotalPoints != 100 || a.Metadata.ReturnedPoints != 10 {
		t.Fatalf("result=%+v locations=%d", a.Metadata, len(a.Locations))
	}
	if a.Locations[0].ID != firstID || a.Locations[len(a.Locations)-1].ID != lastID {
		t.Fatalf("endpoints=%d,%d", a.Locations[0].ID, a.Locations[len(a.Locations)-1].ID)
	}
	foundTurn := false
	for _, loc := range a.Locations {
		if loc.Lon > 127.5 {
			foundTurn = true
		}
	}
	if !foundTurn {
		t.Fatal("sharp turn was not retained")
	}
}

func TestSampleUnwrapsDatelineLikeContinuousPath(t *testing.T) {
	raw := []float64{179, 179.2, 179.4, -180, -179.6, -179.2, -178.8, -178.4, -178, -177.6, -177.2, -176.8}
	continuous := []float64{179, 179.2, 179.4, 180, 180.4, 180.8, 181.2, 181.6, 182, 182.4, 182.8, 183.2}
	lats := []float64{-4.85, 0.05, 2.34, 4.84, 1.71, -2.67, 1.53, 4.49, -0.33, -0.88, -0.09, 3.85}
	run := func(t *testing.T, lons []float64) []int64 {
		st, err := store.Open(filepath.Join(t.TempDir(), "track.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		for i, lon := range lons {
			if _, err := st.Insert(&store.Location{
				User: "u", Device: "d", Tst: int64(i + 1), Lat: lats[i], Lon: lon,
				Payload: []byte(`{}`), Digest: []byte{byte(i + 1)},
			}); err != nil {
				t.Fatal(err)
			}
		}
		snapshot, err := st.BeginTrackSnapshot(store.TrackFilter{Limit: 6})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = snapshot.Close() }()
		result, err := Sample(snapshot, WebMercator)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]int64, len(result.Locations))
		for i, location := range result.Locations {
			out[i] = location.Tst
		}
		return out
	}

	if raw, want := run(t, raw), run(t, continuous); !reflect.DeepEqual(raw, want) {
		t.Fatalf("raw=%v continuous=%v", raw, want)
	}
}

func TestSampleReturnsExactSmallRange(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "small.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	snapshot, err := st.BeginTrackSnapshot(store.TrackFilter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	result, err := Sample(snapshot, WebMercator)
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata.Sampled || result.Metadata.TotalPoints != 0 || len(result.Locations) != 0 {
		t.Fatalf("result=%+v", result)
	}
}

func TestSampleSeparatesIdentityTrajectories(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "multi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var first, last map[string]int64
	first, last = map[string]int64{}, map[string]int64{}
	for i := range 10 {
		for _, identity := range []struct{ user, device string }{{"a", "phone"}, {"b", "watch"}} {
			loc := &store.Location{
				User: identity.user, Device: identity.device, Tst: int64(100 + i*2),
				Lat: float64(i), Lon: float64(i), Payload: []byte(`{}`), Digest: []byte{byte(i + 1)},
			}
			if identity.user == "b" {
				loc.Tst++
			}
			id, err := st.Insert(loc)
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				first[identity.user] = id
			}
			if i == 9 {
				last[identity.user] = id
			}
		}
	}
	final := &store.Location{User: "a", Device: "phone", Tst: 1000, Lat: 10, Lon: 10, Payload: []byte(`{}`), Digest: []byte{99}}
	finalID, err := st.Insert(final)
	if err != nil {
		t.Fatal(err)
	}
	last["a"] = finalID
	snapshot, err := st.BeginTrackSnapshot(store.TrackFilter{Limit: 6})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	result, err := Sample(snapshot, WebMercator)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Locations) != 6 || result.Metadata.TotalPoints != 21 {
		t.Fatalf("metadata=%+v locations=%d", result.Metadata, len(result.Locations))
	}
	seen := map[string]map[int64]bool{"a": {}, "b": {}}
	for _, loc := range result.Locations {
		seen[loc.User][loc.ID] = true
	}
	for _, user := range []string{"a", "b"} {
		if !seen[user][first[user]] || !seen[user][last[user]] {
			t.Fatalf("%s endpoints missing: %+v", user, seen[user])
		}
	}
}

func TestSampleIdentityOverflowUsesTemporalFallback(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "overflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var firstID, lastID int64
	for i := range 6 {
		id, err := st.Insert(&store.Location{
			User: "u", Device: string(rune('a' + i)), Tst: int64(i + 1),
			Lat: float64(i), Lon: float64(i), Payload: []byte(`{}`), Digest: []byte{byte(i + 1)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstID = id
		}
		lastID = id
	}
	snapshot, err := st.BeginTrackSnapshot(store.TrackFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	result, err := Sample(snapshot, WebMercator)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Locations) != 2 || result.Locations[0].ID != firstID || result.Locations[1].ID != lastID {
		t.Fatalf("locations=%+v", result.Locations)
	}
}

func TestSampleUsesRemainingQuotaAcrossIdentities(t *testing.T) {
	for _, tc := range []struct {
		name       string
		identities []string
		limit      int
	}{
		{"same-endpoint-identity", []string{"a", "b", "b", "a", "a", "a", "a"}, 3},
		{"remaining-slots", []string{"a", "b", "c", "a", "b", "c"}, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "track.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			var firstID, lastID int64
			identities := make(map[string]bool)
			for i, device := range tc.identities {
				id, err := st.Insert(&store.Location{
					User: "u", Device: device, Tst: int64(i + 1), Lat: 1, Lon: 2,
					Payload: []byte(`{}`), Digest: make([]byte, 32),
				})
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					firstID = id
				}
				lastID = id
				identities[device] = false
			}
			snapshot, err := st.BeginTrackSnapshot(store.TrackFilter{Limit: tc.limit})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = snapshot.Close() }()
			result, err := Sample(snapshot, WebMercator)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Locations) != tc.limit || result.Metadata.ReturnedPoints != tc.limit {
				t.Fatalf("returned=%d limit=%d", len(result.Locations), tc.limit)
			}
			if result.Locations[0].ID != firstID || result.Locations[len(result.Locations)-1].ID != lastID {
				t.Fatal("global endpoints missing")
			}
			for _, loc := range result.Locations {
				identities[loc.Device] = true
			}
			for device, seen := range identities {
				if !seen {
					t.Fatalf("identity %q missing", device)
				}
			}
		})
	}
}

func BenchmarkSampleMillion(b *testing.B) {
	path := filepath.Join(b.TempDir(), "million.db")
	st, err := store.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	if err := st.Close(); err != nil {
		b.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	_, err = db.Exec(`
		WITH RECURSIVE n(i) AS (
			VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<1000000
		)
		INSERT INTO locations(user,device,tst,lat,lon,payload,digest,created_at)
		SELECT 'u','d',i,37.0+(i%1000)/10000.0,127.0,CAST('{}' AS BLOB),zeroblob(32),i FROM n`)
	if err != nil {
		b.Fatal(err)
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	b.ResetTimer()
	for range b.N {
		snapshot, err := st.BeginTrackSnapshot(store.TrackFilter{Limit: 10000})
		if err != nil {
			b.Fatal(err)
		}
		result, err := Sample(snapshot, WebMercator)
		_ = snapshot.Close()
		if err != nil {
			b.Fatal(err)
		}
		if len(result.Locations) != 10000 {
			b.Fatalf("locations=%d", len(result.Locations))
		}
	}
}
