package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestTouchShareThrottlesConcurrentUpdates(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "shares.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	link := &ShareLink{Hours: 24}
	id, err := st.CreateShare(link)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-shareAccessTouchInterval - time.Second).Unix()
	if _, err := st.db.Exec(`UPDATE share_links SET last_access_at=? WHERE id=?`, old, id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TABLE touch_count (n INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO touch_count DEFAULT VALUES`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER count_share_touch AFTER UPDATE OF last_access_at ON share_links BEGIN UPDATE touch_count SET n=n+1; END`); err != nil {
		t.Fatal(err)
	}

	const callers = 32
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			errs <- st.TouchShare(id)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	var count int
	if err := st.db.QueryRow(`SELECT n FROM touch_count`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("touch updates=%d, want 1", count)
	}

	st.db.SetMaxOpenConns(1)
	if _, err := st.db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchShare(id); err != nil {
		t.Fatalf("throttled touch accessed database: %v", err)
	}
	if err := st.db.QueryRow(`SELECT n FROM touch_count`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("fresh touch updates=%d, want 1", count)
	}
}
