package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prepaser/mytracker/internal/store"
)

func TestPrivacyStatsVacuumAndOrphanRemoval(t *testing.T) {
	st := openTestDB(t)
	legacy := sampleLocation()
	legacy.Payload = []byte(`{"_type":"location","topic":"secret"}`)
	if _, err := st.Insert(legacy); err != nil {
		t.Fatal(err)
	}
	risky := &store.ShareLink{Hours: 24, PrecisionM: 0}
	if _, err := st.CreateShare(risky); err != nil {
		t.Fatal(err)
	}
	stats, err := st.PrivacyStats()
	if err != nil || stats.LegacyRawPayloads != 1 || stats.RiskyShares != 1 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	if err := st.Vacuum(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "1.tsr"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveOrphanProofs(dir, []string{"1.tsr", "missing.ots"}); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(dir, "blocked.tsr")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(blocked, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2.tsr"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveOrphanProofs(dir, []string{"2.tsr", "blocked.tsr"}); err == nil {
		t.Fatal("expected orphan removal error")
	}
	if _, err := os.Stat(filepath.Join(dir, "2.tsr")); !os.IsNotExist(err) {
		t.Fatalf("partial deletion was not retained: %v", err)
	}
}

func TestCountExportProofs(t *testing.T) {
	st := openTestDB(t)
	id, err := st.Insert(sampleLocation())
	if err != nil {
		t.Fatal(err)
	}
	ts := int64(1)
	_ = st.UpdateTSA(id, "ok", &ts)
	_ = st.UpdateOTS(id, "submitted")
	n, err := st.CountExportProofs(store.ExportFilter{})
	if err != nil || n != 2 {
		t.Fatalf("proofs=%d err=%v", n, err)
	}
}

func TestPrivacyStatsCacheInvalidatesOnLocationChange(t *testing.T) {
	st := openTestDB(t)
	if stats, err := st.PrivacyStats(); err != nil || stats.LegacyRawPayloads != 0 {
		t.Fatalf("initial stats=%+v err=%v", stats, err)
	}
	legacy := sampleLocation()
	legacy.Payload = []byte(`{"_type":"location","topic":"legacy"}`)
	if _, err := st.Insert(legacy); err != nil {
		t.Fatal(err)
	}
	if stats, err := st.PrivacyStats(); err != nil || stats.LegacyRawPayloads != 1 {
		t.Fatalf("updated stats=%+v err=%v", stats, err)
	}
}
