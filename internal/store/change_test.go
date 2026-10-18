package store_test

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/store"
)

type changeCollector struct {
	mu      sync.Mutex
	changes []store.Change
}

func (c *changeCollector) Publish(change store.Change) {
	c.mu.Lock()
	c.changes = append(c.changes, change)
	c.mu.Unlock()
}

func (c *changeCollector) take() []store.Change {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]store.Change(nil), c.changes...)
	c.changes = nil
	return out
}

func TestChangesPublishOnlyAfterSuccessfulMutation(t *testing.T) {
	st := openTestDB(t)
	sink := &changeCollector{}
	st.SetChangeSink(sink)
	loc := sampleLocation()
	if _, err := st.Insert(loc); err != nil {
		t.Fatal(err)
	}
	changes := sink.take()
	if len(changes) != 3 || changes[0].Topic != store.ChangeLocations ||
		changes[0].User != loc.User || changes[0].Device != loc.Device || changes[0].Tst != loc.Tst ||
		changes[1].Topic != store.ChangeStampJobs || changes[2].Topic != store.ChangeTimeline {
		t.Fatalf("changes=%+v", changes)
	}
	if _, err := st.Insert(loc); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("duplicate err=%v", err)
	}
	if got := sink.take(); len(got) != 0 {
		t.Fatalf("duplicate published %+v", got)
	}
	if err := st.ImportAtomic(func(*store.ImportTx) error { return errors.New("rollback") }); err == nil {
		t.Fatal("expected rollback")
	}
	if got := sink.take(); len(got) != 0 {
		t.Fatalf("rollback published %+v", got)
	}
}

func TestLeaseRenewalDoesNotPublishChange(t *testing.T) {
	st := openTestDB(t)
	sink := &changeCollector{}
	st.SetChangeSink(sink)
	if _, err := st.Insert(sampleLocation()); err != nil {
		t.Fatal(err)
	}
	sink.take()
	now := time.Now().Unix()
	job, err := st.ClaimStampJob(now, "worker", now+90)
	if err != nil || job == nil {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	sink.take()
	if ok, err := st.RenewStampJob(job.ID, "worker", now+100); err != nil || !ok {
		t.Fatalf("renew ok=%v err=%v", ok, err)
	}
	if got := sink.take(); len(got) != 0 {
		t.Fatalf("renewal published %+v", got)
	}
}

func TestMissingLocationUpdateDoesNotPublishChange(t *testing.T) {
	st := openTestDB(t)
	sink := &changeCollector{}
	st.SetChangeSink(sink)
	if err := st.UpdateTSA(999, "ok", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateOTS(999, "confirmed"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if got := sink.take(); len(got) != 0 {
		t.Fatalf("missing location published %+v", got)
	}
}
