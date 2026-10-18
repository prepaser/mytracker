package timeline

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/worker"
	_ "modernc.org/sqlite"
)

func TestServiceBuildsQueuedTimeline(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	mgr := config.NewManager(cfg, nil)
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for i := range 3 {
		if _, err = st.Insert(&store.Location{User: "u", Device: "d", Tst: int64(i*60 + 1), Lat: float64(i), Lon: 0, Payload: []byte(`{}`), Digest: make([]byte, 32)}); err != nil {
			t.Fatal(err)
		}
	}
	svc := New(mgr, st)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		segments, _ := st.ListTimeline("u", "d", 0, 0, 10)
		if len(segments) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if segments, _ := st.ListTimeline("u", "d", 0, 0, 10); len(segments) == 0 {
		t.Fatal("timeline not built")
	}
	if err := svc.Reload(); err != nil {
		t.Fatal(err)
	}
	svc.Wake()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = svc.ShutdownContext(ctx); err != nil {
		t.Fatal(err)
	}
	if !svc.Healthy() {
		t.Fatal("unhealthy heartbeat")
	}
}

func TestServiceRebuildRetainsBoundaryTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	mgr := config.NewManager(cfg, nil)
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for i, point := range []struct {
		tst      int64
		lat, lon float64
	}{
		{1, 0, 0}, {601, 0, 0}, {1801, 0, 0.1}, {2401, 0, 0.1},
	} {
		if _, err := st.Insert(&store.Location{User: "u", Device: "d", Tst: point.tst, Lat: point.lat, Lon: point.lon, Payload: []byte(`{}`), Digest: []byte{byte(i + 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	svc := &Service{cfg: mgr, st: st, ctx: context.Background(), heartbeat: worker.NewHeartbeat(time.Now)}
	if !svc.work("first") {
		t.Fatal("initial timeline was not built")
	}
	if err := st.EnqueueTimeline("u", "d", 1801); err != nil {
		t.Fatal(err)
	}
	if !svc.work("second") {
		t.Fatal("incremental timeline was not rebuilt")
	}
	segments, err := st.ListTimeline("u", "d", 0, 0, 10)
	if err != nil || len(segments) != 3 || segments[1].Kind != "trip" || segments[1].StartLocationID != 2 || segments[1].EndLocationID != 3 {
		t.Fatalf("segments=%+v err=%v", segments, err)
	}
}

func TestIncrementalRebuildMatchesFullTimeline(t *testing.T) {
	for _, tc := range []struct {
		name string
		gap  int64
	}{{"continuous", 0}, {"gap", 4000}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.DataDir = t.TempDir()
			if err := cfg.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(cfg.DBPath())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			svc := &Service{cfg: config.NewManager(cfg, nil), st: st, ctx: context.Background(), heartbeat: worker.NewHeartbeat(time.Now)}
			points := []store.TrackPoint{
				{Tst: 1, Lat: 37}, {Tst: 601, Lat: 37.0016},
				{Tst: 661 + tc.gap, Lat: 37.0028}, {Tst: 1261 + tc.gap, Lat: 37.0028},
				{Tst: 1321 + tc.gap, Lat: 37.0048}, {Tst: 1381 + tc.gap, Lat: 37.0054},
			}
			insert := func(p store.TrackPoint) {
				t.Helper()
				if _, err := st.Insert(&store.Location{User: "u", Device: "d", Tst: p.Tst, Lat: p.Lat, Lon: 127, Payload: []byte(`{}`), Digest: []byte{1}}); err != nil {
					t.Fatal(err)
				}
			}
			check := func() {
				t.Helper()
				if !svc.work("owner") {
					t.Fatal("rebuild job absent")
				}
				all, err := st.TimelinePoints("u", "d", 0)
				if err != nil {
					t.Fatal(err)
				}
				got, err := st.ListTimeline("u", "d", 0, 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				slices.Reverse(got)
				for i := range got {
					got[i].ID = 0
				}
				if want := Build("u", "d", all, cfg); !reflect.DeepEqual(got, want) {
					t.Fatalf("incremental=%+v\nfull=%+v", got, want)
				}
			}
			for _, p := range points[:5] {
				insert(p)
			}
			check()
			insert(points[5])
			check()
			insert(store.TrackPoint{Tst: points[2].Tst + 30, Lat: 37.0019})
			check()
			if _, err := st.DeleteRange(cfg.ProofsDir(), store.ExportFilter{From: points[2].Tst, To: points[2].Tst}); err != nil {
				t.Fatal(err)
			}
			check()
		})
	}
}

func TestServiceRepairsLegacyTimelineOnceAtStartup(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	mgr := config.NewManager(cfg, nil)
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	points := []struct {
		tst      int64
		lat, lon float64
	}{
		{1, 0, 0}, {601, 0, 0}, {1801, 0.1, 0},
	}
	ids := make([]int64, len(points))
	for i, point := range points {
		ids[i], err = st.Insert(&store.Location{User: "u", Device: "d", Tst: point.tst, Lat: point.lat, Lon: point.lon, Payload: []byte(`{}`), Digest: []byte{byte(i + 1)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Unix()
	job, err := st.ClaimTimelineJob("seed", now, now+90)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	legacy := []store.TimelineSegment{
		{User: "u", Device: "d", Kind: "stay", StartLocationID: ids[0], EndLocationID: ids[1], StartTst: points[0].tst, EndTst: points[1].tst, PointCount: 2},
		{User: "u", Device: "d", Kind: "trip", StartLocationID: ids[2], EndLocationID: ids[2], StartTst: points[2].tst, EndTst: points[2].tst, PointCount: 1},
	}
	if err := st.FinishTimelineJob(job, points[0].tst, legacy, nil); err != nil {
		t.Fatal(err)
	}
	svc := New(mgr, st)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		segments, listErr := st.ListTimeline("u", "d", 0, 0, 10)
		if listErr == nil && len(segments) == 2 && segments[0].Kind == "trip" && segments[0].StartLocationID == ids[1] {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	segments, err := st.ListTimeline("u", "d", 0, 0, 10)
	if err != nil || len(segments) != 2 || segments[0].Kind != "trip" || segments[0].StartLocationID != ids[1] {
		t.Fatalf("repaired segments=%+v err=%v", segments, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := svc.ShutdownContext(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	svc = New(mgr, st)
	time.Sleep(50 * time.Millisecond)
	if n, err := st.TimelineJobCount(); err != nil || n != 0 {
		t.Fatalf("repeat startup jobs=%d err=%v", n, err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	if err := svc.ShutdownContext(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
}

func TestCanceledServiceDoesNotClaim(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	mgr := config.NewManager(cfg, nil)
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err = st.Insert(&store.Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc := &Service{cfg: mgr, st: st, ctx: ctx}
	svc.wg.Add(1)
	svc.run()
	if svc.work("owner") {
		t.Fatal("canceled service claimed work")
	}
	if n, err := st.TimelineJobCount(); err != nil || n != 1 {
		t.Fatalf("jobs=%d err=%v", n, err)
	}
}

func TestReloadReturnsEnqueueError(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	svc := &Service{st: st, wake: worker.NewSignal()}
	if err := svc.Reload(); err == nil {
		t.Fatal("reload hid enqueue error")
	}
}

func TestPanicAfterClaimFinishesAsRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	_, err = db.Exec(`
		INSERT INTO locations(id,user,device,tst,lat,lon,payload,digest,tsa_status,ots_status,created_at)
		VALUES (1,'u','d',1,0,0,'{}',x'00','pending','pending',?);
		INSERT INTO timeline_rebuild_jobs(user,device,dirty_from,state,created_at,updated_at)
		VALUES ('u','d',1,'queued',?,?)`, now, now, now)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	svc := &Service{
		st: st, ctx: context.Background(), wake: worker.NewSignal(),
		heartbeat: worker.NewHeartbeat(time.Now),
	}
	if !svc.work("panic-owner") {
		t.Fatal("panic job was not claimed")
	}
	job, err := st.ClaimTimelineJob("next-owner", now+3, now+93)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || job.Attempts != 2 {
		t.Fatalf("panic job was not returned to retry: %+v", job)
	}
	if err := st.FinishTimelineJob(job, job.DirtyFrom, nil, nil); err != nil {
		t.Fatal(err)
	}
}
