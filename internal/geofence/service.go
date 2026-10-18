package geofence

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/metrics"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/webhook"
	"github.com/prepaser/mytracker/internal/worker"
)

type Service struct {
	cfg       *config.Manager
	st        *store.Store
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	heartbeat *worker.Heartbeat
}

func New(cfg *config.Manager, st *store.Store) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{cfg: cfg, st: st, ctx: ctx, cancel: cancel, heartbeat: worker.NewHeartbeat(time.Now)}
	if err := st.RecoverGeofenceDeliveries(time.Now().Unix()); err != nil {
		slog.Error("recover geofence deliveries", "err", err)
	}
	s.wg.Add(1)
	go s.run()
	return s
}
func (s *Service) Healthy() bool { return s.heartbeat.Healthy(30 * time.Second) }
func (s *Service) ShutdownContext(ctx context.Context) error {
	s.cancel()
	return worker.Wait(ctx, &s.wg)
}
func (s *Service) run() {
	defer s.wg.Done()
	owner := time.Now().Format("150405.000000000")
	t := time.NewTicker(time.Second)
	defer t.Stop()
	nextCleanup := time.Time{}
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		s.heartbeat.Beat()
		if now := time.Now(); !now.Before(nextCleanup) {
			if _, err := s.st.CleanupGeofenceDeliveries(now.Add(-30 * 24 * time.Hour).Unix()); err != nil {
				slog.Error("cleanup geofence deliveries", "err", err)
			}
			nextCleanup = now.Add(time.Hour)
		}
		for {
			if s.ctx.Err() != nil {
				return
			}
			s.heartbeat.Beat()
			now := time.Now()
			d, err := s.st.ClaimGeofenceDelivery(owner, now.Unix(), now.Add(90*time.Second).Unix())
			if err != nil {
				slog.Error("claim geofence delivery", "err", err)
				break
			}
			if d == nil {
				break
			}
			err = func() error {
				now := time.Now()
				jobCtx, stopLease := worker.RenewLease(s.ctx, worker.LeaseOptions{
					Interval:      15 * time.Second,
					LeaseUntil:    time.Unix(now.Add(90*time.Second).Unix(), 0),
					LeaseDuration: 90 * time.Second,
					Renew: func(ctx context.Context, now time.Time) (bool, error) {
						return s.st.RenewGeofenceDeliveryContext(ctx, d.ID, d.LeaseOwner, now.Add(90*time.Second).Unix())
					},
					OnError: func(err error) {
						slog.Error("renew geofence lease", "delivery_id", d.ID, "err", err)
					},
					OnRenewed: func() { s.heartbeat.Beat() },
				})
				defer stopLease()
				return worker.Guard("geofence worker", func() error {
					cfg := s.cfg.Snapshot()
					data := map[string]any{"geofence_id": d.Event.GeofenceID, "name": d.Event.Name, "event": d.Event.Event, "tst": d.Event.Tst, "user": d.Event.User, "device": d.Event.Device}
					return webhook.Send(jobCtx, cfg.WebhookURL, cfg.WebhookSecret, "geofence."+d.Event.Event, data)
				})
			}()
			s.heartbeat.Beat()
			if d.Attempts == 1 {
				metrics.GeofenceEvent(d.Event.Event)
			}
			result := "ok"
			if err != nil {
				result = "error"
			}
			metrics.GeofenceDelivery(result)
			if finish := s.st.FinishGeofenceDelivery(d, err); finish != nil && finish != store.ErrLeaseLost {
				slog.Error("finish geofence delivery", "err", finish)
			}
		}
	}
}
