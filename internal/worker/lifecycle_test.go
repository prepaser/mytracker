package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestSignalCoalesces(t *testing.T) {
	s := NewSignal()
	s.Notify()
	s.Notify()
	select {
	case <-s.C():
	default:
		t.Fatal("signal missing")
	}
	select {
	case <-s.C():
		t.Fatal("duplicate signal was not coalesced")
	default:
	}
}

func TestHeartbeat(t *testing.T) {
	now := time.Unix(100, 0)
	h := NewHeartbeat(func() time.Time { return now })
	if h.Healthy(10 * time.Second) {
		t.Fatal("zero heartbeat is healthy")
	}
	h.Beat()
	if !h.Healthy(10 * time.Second) {
		t.Fatal("fresh heartbeat is unhealthy")
	}
	now = now.Add(10 * time.Second)
	if h.Healthy(10 * time.Second) {
		t.Fatal("expired heartbeat is healthy")
	}
}

func TestRenewLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var renewals atomic.Int32
		var errorsSeen atomic.Int32
		var renewed atomic.Int32
		var lost atomic.Int32
		now := time.Unix(123, 0)
		ctx, stop := RenewLease(context.Background(), LeaseOptions{
			Interval: time.Millisecond,
			Now:      func() time.Time { return now },
			Renew: func(_ context.Context, got time.Time) (bool, error) {
				if got != now {
					t.Errorf("now=%v", got)
				}
				switch renewals.Add(1) {
				case 1:
					return false, errors.New("temporary")
				case 2:
					return true, nil
				default:
					return false, nil
				}
			},
			OnError:   func(error) { errorsSeen.Add(1) },
			OnRenewed: func() { renewed.Add(1) },
			OnLost:    func() { lost.Add(1) },
		})
		defer stop()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("lease loss did not cancel context")
		}
		stop()
		if renewals.Load() < 3 || errorsSeen.Load() != 1 || renewed.Load() != 1 || lost.Load() != 1 {
			t.Fatalf("renewals=%d errors=%d renewed=%d lost=%d",
				renewals.Load(), errorsSeen.Load(), renewed.Load(), lost.Load())
		}
		stop()
	})
}

func TestRenewLeaseCancelsAtKnownExpiryAfterTransientErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var errorsSeen atomic.Int32
		var lost atomic.Int32
		started := time.Now()
		ctx, stop := RenewLease(context.Background(), LeaseOptions{
			Interval:   5 * time.Millisecond,
			LeaseUntil: started.Add(40 * time.Millisecond),
			Renew: func(context.Context, time.Time) (bool, error) {
				return false, errors.New("temporary")
			},
			OnError: func(error) { errorsSeen.Add(1) },
			OnLost:  func() { lost.Add(1) },
		})
		defer stop()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("lease expiry did not cancel context")
		}
		stop()
		if elapsed := time.Since(started); elapsed < 25*time.Millisecond || elapsed > 500*time.Millisecond {
			t.Fatalf("expiry elapsed=%s", elapsed)
		}
		if errorsSeen.Load() == 0 || lost.Load() != 1 {
			t.Fatalf("errors=%d lost=%d", errorsSeen.Load(), lost.Load())
		}
	})
}

func TestRenewLeaseExpiryDoesNotWaitForBlockedRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		blocked := make(chan struct{})
		started := make(chan struct{})
		finished := make(chan struct{})
		ctx, stop := RenewLease(context.Background(), LeaseOptions{
			Interval:   time.Millisecond,
			LeaseUntil: time.Now().Add(30 * time.Millisecond),
			Renew: func(ctx context.Context, _ time.Time) (bool, error) {
				defer close(finished)
				close(started)
				select {
				case <-blocked:
					return true, nil
				case <-ctx.Done():
					return false, ctx.Err()
				}
			},
		})
		defer close(blocked)
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("renewal did not start")
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("blocked renewal prevented lease expiry")
		}
		done := make(chan struct{})
		go func() {
			stop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("stop waited for blocked renewal")
		}
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("renewal callback survived cancellation")
		}
	})
}

func TestRenewLeaseDefaultsAndImmediateExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var nilHeartbeat *Heartbeat
		nilHeartbeat.Beat()
		if nilHeartbeat.Healthy(time.Second) {
			t.Fatal("nil heartbeat is healthy")
		}
		hb := NewHeartbeat(nil)
		if hb.Healthy(time.Second) {
			t.Fatal("new heartbeat is healthy before first beat")
		}
		hb.Beat()
		if !hb.Healthy(time.Second) {
			t.Fatal("heartbeat did not become healthy")
		}

		lost := make(chan struct{})
		ctx, stop := RenewLease(context.Background(), LeaseOptions{
			LeaseUntil: time.Now().Add(-time.Second),
			Renew:      func(context.Context, time.Time) (bool, error) { return true, nil },
			OnLost:     func() { close(lost) },
		})
		defer stop()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("expired lease remained active")
		}
		select {
		case <-lost:
		case <-time.After(time.Second):
			t.Fatal("lease loss callback was not called")
		}
	})
}

func TestRenewLeaseDerivesDeadlineFromDuration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		renewed := make(chan struct{}, 1)
		ctx, stop := RenewLease(context.Background(), LeaseOptions{
			Interval:      time.Millisecond,
			LeaseDuration: time.Second,
			Renew:         func(context.Context, time.Time) (bool, error) { return true, nil },
			OnRenewed:     func() { renewed <- struct{}{} },
		})
		defer stop()
		select {
		case <-renewed:
		case <-ctx.Done():
			t.Fatal("lease expired before renewal")
		case <-time.After(time.Second):
			t.Fatal("lease was not renewed")
		}
	})
}

func TestWait(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := Wait(ctx, &wg); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	wg.Done()
	if err := Wait(context.Background(), &wg); err != nil {
		t.Fatal(err)
	}
}

func TestGuards(t *testing.T) {
	if err := Guard("worker", func() error { panic("boom") }); err == nil ||
		!strings.Contains(err.Error(), "worker panic: boom") {
		t.Fatalf("err=%v", err)
	}
	value, err := GuardValue("worker", func() (int, error) { return 7, nil })
	if err != nil || value != 7 {
		t.Fatalf("value=%d err=%v", value, err)
	}
	_, err = GuardValue("worker", func() (int, error) { panic("boom") })
	if err == nil {
		t.Fatal("value panic was not converted")
	}
}
