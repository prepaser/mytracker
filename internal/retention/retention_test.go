package retention_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/retention"
	"github.com/prepaser/mytracker/internal/store"
)

func TestRunDeletesOld(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.RetentionDays = 7
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	old := time.Now().Add(-10 * 24 * time.Hour).Unix()
	_, err = st.Insert(&store.Location{
		User: "a", Device: "p", Tst: old, Lat: 1, Lon: 1,
		Payload: []byte(`{}`), Digest: []byte{1, 2, 3, 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgMgr := config.NewManager(cfg, nil)
	if _, err := retention.Run(cfgMgr, st); err != nil {
		t.Fatal(err)
	}
	n, err := st.Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 locations, got %d", n)
	}
}

func TestDisabledRetentionDoesNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	mgr := config.NewManager(cfg, nil)
	if n, err := retention.Run(mgr, st); err != nil || n != 0 {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
}

func TestRunHonorsCanceledContext(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.RetentionDays = 1
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = retention.RunContext(ctx, config.NewManager(cfg, nil), st)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}

func TestRetentionUsesReceivedTime(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.RetentionDays = 7
	cfg.RetentionBy = "received"
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now().Unix()
	for _, loc := range []*store.Location{
		{User: "a", Device: "old", Tst: now, CreatedAt: now - 10*24*3600},
		{User: "a", Device: "new", Tst: now - 10*24*3600, CreatedAt: now},
	} {
		loc.Payload = []byte(`{}`)
		loc.Digest = []byte{1}
		loc.TsaStatus = "pending"
		loc.OtsStatus = "pending"
		if _, err := st.ImportLocation(loc); err != nil {
			t.Fatal(err)
		}
	}
	mgr := config.NewManager(cfg, nil)
	if n, err := retention.Run(mgr, st); err != nil || n != 1 {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
	if n, err := st.Count(); err != nil || n != 1 {
		t.Fatalf("remaining=%d err=%v", n, err)
	}
}

func TestRetentionKeepsDataWhenBackupFails(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.RetentionDays = 1
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.Insert(&store.Location{
		User: "a", Device: "p", Tst: time.Now().Add(-48 * time.Hour).Unix(),
		Payload: []byte(`{}`), Digest: []byte{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	stampTime := time.Now().Unix()
	if err := st.UpdateTSA(id, "ok", &stampTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cfg.ProofsDir(), "1.tsr")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	mgr := config.NewManager(cfg, nil)
	if n, err := retention.Run(mgr, st); err == nil || n != 0 {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
	if n, err := st.Count(); err != nil || n != 1 {
		t.Fatalf("remaining=%d err=%v", n, err)
	}
}
