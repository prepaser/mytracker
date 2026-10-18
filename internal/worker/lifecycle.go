package worker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type Signal struct {
	ch chan struct{}
}

func NewSignal() *Signal {
	return &Signal{ch: make(chan struct{}, 1)}
}

func (s *Signal) Notify() {
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

func (s *Signal) C() <-chan struct{} {
	return s.ch
}

type Heartbeat struct {
	last atomic.Int64
	now  func() time.Time
}

func NewHeartbeat(now func() time.Time) *Heartbeat {
	if now == nil {
		now = time.Now
	}
	return &Heartbeat{now: now}
}

func (h *Heartbeat) Beat() {
	if h == nil {
		return
	}
	h.last.Store(h.now().Unix())
}

func (h *Heartbeat) Healthy(maxAge time.Duration) bool {
	if h == nil {
		return false
	}
	last := h.last.Load()
	return last > 0 && h.now().Sub(time.Unix(last, 0)) < maxAge
}

type LeaseOptions struct {
	Interval      time.Duration
	Now           func() time.Time
	LeaseUntil    time.Time
	LeaseDuration time.Duration
	Renew         func(context.Context, time.Time) (bool, error)
	OnError       func(error)
	OnRenewed     func()
	OnLost        func()
}

func RenewLease(parent context.Context, opt LeaseOptions) (context.Context, func()) {
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		interval := opt.Interval
		if interval <= 0 {
			interval = time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		deadline := opt.LeaseUntil
		if deadline.IsZero() && opt.LeaseDuration > 0 {
			deadline = time.Unix(now().Add(opt.LeaseDuration).Unix(), 0)
		}
		hasDeadline := !deadline.IsZero()
		var timer *time.Timer
		if hasDeadline {
			timer = time.NewTimer(deadline.Sub(now()))
			defer timer.Stop()
		}
		var lost sync.Once
		lose := func() {
			lost.Do(func() {
				cancel()
				if opt.OnLost != nil {
					opt.OnLost()
				}
			})
		}
		resetTimer := func(d time.Duration) {
			if timer == nil {
				return
			}
			timer.Stop()
			timer.Reset(d)
		}
		for {
			var expiry <-chan time.Time
			if timer != nil {
				expiry = timer.C
			}
			select {
			case <-ctx.Done():
				return
			case <-expiry:
				lose()
				return
			case <-ticker.C:
				current := now()
				if hasDeadline && !current.Before(deadline) {
					lose()
					return
				}
				type renewResult struct {
					ok  bool
					err error
				}
				result := make(chan renewResult, 1)
				go func() {
					ok, err := opt.Renew(ctx, current)
					result <- renewResult{ok: ok, err: err}
				}()
				var renewed renewResult
				select {
				case <-ctx.Done():
					return
				case <-expiry:
					lose()
					return
				case renewed = <-result:
				}
				if hasDeadline && !now().Before(deadline) {
					lose()
					return
				}
				ok, err := renewed.ok, renewed.err
				if err != nil {
					if opt.OnError != nil {
						opt.OnError(err)
					}
					if hasDeadline && !now().Before(deadline) {
						lose()
						return
					}
					continue
				}
				if !ok {
					lose()
					return
				}
				if opt.LeaseDuration > 0 {
					deadline = time.Unix(current.Add(opt.LeaseDuration).Unix(), 0)
					hasDeadline = true
					resetTimer(deadline.Sub(now()))
				}
				if opt.OnRenewed != nil {
					opt.OnRenewed()
				}
			}
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

func Wait(ctx context.Context, wg *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func Guard(label string, fn func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%s panic: %v", label, recovered)
		}
	}()
	return fn()
}

func GuardValue[T any](label string, fn func() (T, error)) (value T, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%s panic: %v", label, recovered)
		}
	}()
	return fn()
}
