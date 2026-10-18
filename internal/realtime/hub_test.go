package realtime

import (
	"sync"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/store"
)

func TestHubCoalescesTopicsAndLocations(t *testing.T) {
	hub := New()
	sub := hub.Subscribe()
	defer sub.Close()

	hub.Publish(store.Change{Topic: store.ChangeVerification, LocationID: 2})
	hub.Publish(store.Change{Topic: store.ChangeLocations, LocationID: 1, User: "private"})
	hub.Publish(store.Change{Topic: store.ChangeVerification, LocationID: 2})
	<-sub.Signal()
	batch := sub.Drain()
	if batch.All || len(batch.Topics) != 2 || batch.Topics[0] != store.ChangeLocations ||
		len(batch.LocationIDs) != 2 || batch.LocationIDs[0] != 1 || batch.LocationIDs[1] != 2 || !batch.Coalesced {
		t.Fatalf("batch=%+v", batch)
	}
	select {
	case <-sub.Signal():
		t.Fatal("duplicate wake signal")
	default:
	}
}

func TestHubLocationCapAndSlowSubscriber(t *testing.T) {
	hub := New()
	sub := hub.Subscribe()
	defer sub.Close()
	done := make(chan struct{})
	go func() {
		for i := int64(1); i <= maxLocationIDs+10; i++ {
			hub.Publish(store.Change{Topic: store.ChangeLocations, LocationID: i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("slow subscriber blocked publisher")
	}
	<-sub.Signal()
	batch := sub.Drain()
	if !batch.All || len(batch.LocationIDs) != 0 || len(batch.Changes) != 0 {
		t.Fatalf("batch=%+v", batch)
	}
}

func TestHubConcurrentClose(t *testing.T) {
	hub := New()
	var wg sync.WaitGroup
	for range 20 {
		sub := hub.Subscribe()
		wg.Go(func() {
			hub.Publish(store.Change{Topic: store.ChangeMQTT})
			sub.Close()
		})
	}
	wg.Wait()
	hub.Close()
	if stats := hub.Stats(); !stats.Closed || stats.Subscribers != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	sub := hub.Subscribe()
	select {
	case <-sub.Done():
	default:
		t.Fatal("subscription remained open after shutdown")
	}
}
