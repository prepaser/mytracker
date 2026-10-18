package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestPrivacyCacheTracksCommittedInserts(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.PrivacyStats(); err != nil {
		t.Fatal(err)
	}
	for i, payload := range []string{`{"tst":1,"lat":1,"lon":2}`, `{"topic":"legacy"}`} {
		if _, err := st.Insert(&Location{User: "u", Device: "d", Tst: int64(i + 1), Payload: []byte(payload), Digest: []byte{1}}); err != nil {
			t.Fatal(err)
		}
		if st.privacy.cachedGeneration != st.privacy.generation || st.privacy.legacy != i {
			t.Fatalf("cache=%+v", st.privacy)
		}
	}
	generation := st.privacy.generation
	_, err = st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{"topic":"duplicate"}`), Digest: []byte{1}})
	if !errors.Is(err, ErrDuplicate) || st.privacy.generation != generation {
		t.Fatalf("duplicate err=%v cache=%+v", err, st.privacy)
	}
	if _, err := st.DeleteRange(t.TempDir(), ExportFilter{From: 2}); err != nil {
		t.Fatal(err)
	}
	stats, err := st.PrivacyStats()
	if err != nil || stats.LegacyRawPayloads != 0 {
		t.Fatalf("after delete stats=%+v err=%v", stats, err)
	}
}

func TestPrivacyScanConcurrentInserts(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			if _, err := st.PrivacyStats(); err != nil {
				t.Error(err)
				return
			}
		}
	})
	for i := range 100 {
		if _, err := st.Insert(&Location{User: "u", Device: "d", Tst: int64(i + 1), Payload: []byte(`{"topic":"legacy"}`), Digest: []byte{1}}); err != nil {
			t.Error(err)
			break
		}
	}
	wg.Wait()
	stats, err := st.PrivacyStats()
	if err != nil || stats.LegacyRawPayloads != 100 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}
