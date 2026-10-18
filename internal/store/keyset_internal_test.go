package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestKeysetDefaultsAndCallbackCursor(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "keyset.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	seedKeysetLocations(t, st)

	t.Run("track snapshot locations", func(t *testing.T) {
		snapshot, err := st.BeginTrackSnapshot(TrackFilter{User: "u", Device: "d"})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = snapshot.Close() }()
		assertLocationKeyset(t, snapshot.Iterate)
	})

	t.Run("export snapshot locations", func(t *testing.T) {
		snapshot, err := st.BeginExportSnapshot(ExportFilter{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = snapshot.Close() }()
		assertLocationKeyset(t, snapshot.Iterate)
	})

	t.Run("track snapshot points", func(t *testing.T) {
		snapshot, err := st.BeginTrackSnapshot(TrackFilter{User: "u", Device: "d"})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = snapshot.Close() }()
		assertPointKeyset(t, snapshot.IteratePoints)
	})

	t.Run("timeline points", func(t *testing.T) {
		assertPointKeyset(t, func(batch int, fn func([]TrackPoint) error) error {
			return st.IterateTimelinePoints(context.Background(), "u", "d", 1, batch, fn)
		})
	})
}

func assertLocationKeyset(t *testing.T, iterate func(int, func([]Location) error) error) {
	t.Helper()
	var batches []int
	var ids []int64
	err := iterate(0, func(locs []Location) error {
		batches = append(batches, len(locs))
		if len(batches) == 1 {
			locs[len(locs)-1].Tst = 1000
		}
		for _, loc := range locs {
			ids = append(ids, loc.ID)
		}
		return nil
	})
	if err != nil || len(batches) != 2 || batches[0] != 500 || batches[1] != 2 || len(ids) != 502 || ids[500] != 1000 {
		t.Fatalf("batches=%v ids=%v err=%v", batches, ids, err)
	}
}

func assertPointKeyset(t *testing.T, iterate func(int, func([]TrackPoint) error) error) {
	t.Helper()
	var batches []int
	var ids []int64
	err := iterate(0, func(points []TrackPoint) error {
		batches = append(batches, len(points))
		if len(batches) == 1 {
			points[len(points)-1].Tst = 999
		}
		for _, point := range points {
			ids = append(ids, point.ID)
		}
		return nil
	})
	if err != nil || len(batches) != 2 || batches[0] != 1000 || batches[1] != 2 || len(ids) != 1002 || ids[999] != 1000 || ids[1000] != 1000 {
		t.Fatalf("batches=%v ids=%v err=%v", batches, ids, err)
	}
}

func seedKeysetLocations(t *testing.T, st *Store) {
	t.Helper()
	tx, err := st.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO locations(user,device,tst,lat,lon,payload,digest,created_at) VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	for i := 1; i <= 1001; i++ {
		if _, err := stmt.Exec("u", "d", i, float64(i), float64(i), []byte(`{}`), make([]byte, 32), i); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
