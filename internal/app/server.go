package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/geofence"
	"github.com/prepaser/mytracker/internal/maintenance"
	"github.com/prepaser/mytracker/internal/metrics"
	"github.com/prepaser/mytracker/internal/mqtt"
	"github.com/prepaser/mytracker/internal/owntracks"
	"github.com/prepaser/mytracker/internal/realtime"
	"github.com/prepaser/mytracker/internal/stamp"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/timeline"
	"github.com/prepaser/mytracker/internal/web"
	"github.com/prepaser/mytracker/internal/worker"
)

func serve(ctx context.Context, cfg *config.Config, cfgMgr *config.Manager, st *store.Store) int {
	code, _ := serveManaged(ctx, cfg, cfgMgr, st)
	return code
}

func shutdownExitCode(exitCode int, stopped bool) int {
	if !stopped && exitCode == 0 {
		return 1
	}
	return exitCode
}

func serveManaged(ctx context.Context, cfg *config.Config, cfgMgr *config.Manager, st *store.Store) (int, bool) {
	https, err := configureHTTPS(cfg)
	if err != nil {
		slog.Error("https setup", "err", err)
		return 1, true
	}
	events := realtime.New()
	st.SetChangeSink(events)
	stampSvc := stamp.New(cfgMgr, st)
	maintenanceSvc := maintenance.New(cfgMgr, st, stampSvc)
	timelineSvc := timeline.New(cfgMgr, st)
	geofenceSvc := geofence.New(cfgMgr, st)
	otHandler := owntracks.New(st, stampSvc, cfgMgr)
	webSrv := web.New(web.Dependencies{
		Config: cfgMgr, Store: st, Stamp: stampSvc, Verification: stampSvc, Maintenance: maintenanceSvc, Events: events,
		Timeline: timelineSvc, Geofence: geofenceSvc,
	})

	mux := http.NewServeMux()
	mux.Handle("/pub", otHandler)
	mux.Handle("/", webSrv.Handler())

	backgroundCtx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()
	var background sync.WaitGroup
	background.Go(func() {
		mqtt.Run(backgroundCtx, cfgMgr, func(user, device string, body []byte) mqtt.IngestResult {
			if len(body) > owntracks.MaxPayloadBytes {
				metrics.Ingest("mqtt", "invalid")
				slog.Warn("mqtt payload rejected", "reason", "too_large")
				return mqtt.IngestInvalid
			}
			if !otHandler.AllowIngest(user, device) {
				metrics.Ingest("mqtt", "rate_limited")
				return mqtt.IngestRetry
			}
			id, err := owntracks.Ingest(st, stampSvc, user, device, body, cfgMgr.Snapshot().StampRetries)
			if err != nil {
				if errors.Is(err, owntracks.ErrInvalidPayload) {
					metrics.Ingest("mqtt", "invalid")
					slog.Warn("mqtt payload rejected")
					return mqtt.IngestInvalid
				} else {
					metrics.Ingest("mqtt", "error")
					slog.Error("mqtt ingest", "err", err)
					return mqtt.IngestRetry
				}
			} else if id > 0 {
				metrics.IncLocation()
				metrics.Ingest("mqtt", "stored")
				return mqtt.IngestStored
			} else {
				metrics.Ingest("mqtt", "ignored")
				return mqtt.IngestDuplicate
			}
		}, func(bool) { events.Publish(store.Change{Topic: store.ChangeMQTT}) })
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       60 * time.Second,
		TLSConfig:         https.tlsConfig,
	}
	if https.tlsConfig != nil {
		srv.Addr = cfg.HTTPSListenAddr
		if https.httpServer == nil && cfg.ListenAddr != "" {
			https.httpServer = &http.Server{
				Addr: cfg.ListenAddr, Handler: mux,
				ReadHeaderTimeout: srv.ReadHeaderTimeout, ReadTimeout: srv.ReadTimeout,
				WriteTimeout: srv.WriteTimeout, IdleTimeout: srv.IdleTimeout,
			}
		}
	}
	errCh := make(chan error, 2)
	go func() {
		slog.Info("listening", "addr", srv.Addr, "mode", https.mode)
		var err error
		if https.tlsConfig == nil {
			err = srv.ListenAndServe()
		} else {
			err = srv.ListenAndServeTLS("", "")
		}
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	if https.httpServer != nil {
		go func() {
			slog.Info("HTTP listening", "addr", https.httpServer.Addr)
			if err := https.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		}()
	}

	exitCode := 0
	select {
	case err := <-errCh:
		slog.Error("http serve", "err", err)
		exitCode = 1
	case <-ctx.Done():
	}
	shutdown, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	var stopped atomic.Bool
	stopped.Store(true)
	recordShutdown := func(label string, err error) {
		if err != nil {
			stopped.Store(false)
			slog.Warn(label, "err", err)
		}
	}
	events.Close()
	cancelBackground()
	var shutdownWG sync.WaitGroup
	shutdownWG.Go(func() {
		recordShutdown("http shutdown", srv.Shutdown(shutdown))
	})
	shutdownWG.Go(func() {
		recordShutdown("timeline shutdown", timelineSvc.ShutdownContext(shutdown))
	})
	shutdownWG.Go(func() {
		recordShutdown("geofence shutdown", geofenceSvc.ShutdownContext(shutdown))
	})
	if https.httpServer != nil {
		shutdownWG.Go(func() {
			recordShutdown("HTTP shutdown", https.httpServer.Shutdown(shutdown))
		})
	}
	shutdownWG.Go(func() {
		recordShutdown("maintenance shutdown", maintenanceSvc.ShutdownContext(shutdown))
	})
	shutdownWG.Go(func() {
		recordShutdown("stamp shutdown", stampSvc.ShutdownContext(shutdown))
	})
	shutdownWG.Wait()
	recordShutdown("background shutdown", worker.Wait(shutdown, &background))
	stoppedOK := stopped.Load()
	return shutdownExitCode(exitCode, stoppedOK), stoppedOK
}
