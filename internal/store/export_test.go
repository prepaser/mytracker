package store_test

import (
	"path/filepath"
	"testing"

	"github.com/prepaser/mytracker/internal/store"
)

func TestListAll(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	for _, loc := range []*store.Location{
		{User: "a", Device: "p", Tst: 300, Lat: 3, Lon: 3, Payload: []byte("{}"), Digest: []byte{1}},
		{User: "a", Device: "p", Tst: 100, Lat: 1, Lon: 1, Payload: []byte("{}"), Digest: []byte{2}},
	} {
		if _, err := st.Insert(loc); err != nil {
			t.Fatal(err)
		}
	}
	locs, err := st.ListAll()
	if err != nil || len(locs) != 2 || locs[0].Tst != 100 {
		t.Fatalf("got %+v err=%v", locs, err)
	}
	stampTime := int64(300)
	if err := st.UpdateTSA(1, "ok", &stampTime); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateOTS(1, "submitted"); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CountExportProofs(store.ExportFilter{From: 200, To: 400}); err != nil || n != 2 {
		t.Fatalf("proofs=%d err=%v", n, err)
	}
	if locs, err := st.List(0, 0); err != nil || len(locs) != 2 {
		t.Fatalf("default list=%d err=%v", len(locs), err)
	}
	if locs, err := st.PendingOTSLimit(0); err != nil || len(locs) != 2 {
		t.Fatalf("pending OTS=%d err=%v", len(locs), err)
	}
}
