package testutil

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/maintenance"
	"github.com/prepaser/mytracker/internal/owntracks"
	"github.com/prepaser/mytracker/internal/realtime"
	"github.com/prepaser/mytracker/internal/stamp"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/web"
	"golang.org/x/crypto/bcrypt"
)

const AdminPassword = "test-admin-pass"

func Setup(t *testing.T) (*config.Config, *store.Store, *stamp.Service, *owntracks.Handler, *web.Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.ListenAddr = ":8080"
	hash, err := bcrypt.GenerateFromPassword([]byte(AdminPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AdminHash = string(hash)
	cfg.PubUser = "owntracks"
	cfg.PubPass = "pub-secret"
	cfg.TSAURL = "http://127.0.0.1:1/nope"
	cfg.OTSCalendars = []string{"http://127.0.0.1:1/nope"}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	data, err := json.Marshal(cfg.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSettings(data, false); err != nil {
		t.Fatal(err)
	}
	cfgMgr := config.NewManager(cfg, st)
	events := realtime.New()
	st.SetChangeSink(events)
	t.Cleanup(events.Close)
	stampSvc := stamp.New(cfgMgr, st)
	t.Cleanup(stampSvc.Shutdown)
	maintenanceSvc := maintenance.New(cfgMgr, st, stampSvc)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = maintenanceSvc.ShutdownContext(ctx)
	})
	ot := owntracks.New(st, stampSvc, cfgMgr)
	webSrv := web.New(web.Dependencies{
		Config: cfgMgr, Store: st, Stamp: stampSvc, Maintenance: maintenanceSvc, Events: events,
	})
	return cfg, st, stampSvc, ot, webSrv, cfgPath
}
