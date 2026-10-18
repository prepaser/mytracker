package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/store"
)

func TestGeofenceInitialStateAndExit(t *testing.T) {
	st := openTestDB(t)
	g, err := st.CreateGeofence(&store.Geofence{Name: "Home", Lat: 37, Lon: 127, RadiusM: 200, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	first := sampleLocation()
	first.Lat = 37
	first.Lon = 127
	if _, err = st.Insert(first); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListGeofenceEvents(10)
	if err != nil || len(events) != 0 {
		t.Fatalf("initial events=%+v err=%v", events, err)
	}
	second := sampleLocation()
	second.Tst++
	second.Lat = 37.01
	second.Lon = 127.01
	if _, err = st.Insert(second); err != nil {
		t.Fatal(err)
	}
	events, err = st.ListGeofenceEvents(10)
	if err != nil || len(events) != 1 || events[0].Event != "exit" || events[0].GeofenceID != g.ID {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	now := time.Now().Unix()
	delivery, err := st.ClaimGeofenceDelivery("worker", now, now+90)
	if err != nil || delivery == nil {
		t.Fatalf("delivery=%+v err=%v", delivery, err)
	}
	if err = st.FinishGeofenceDelivery(delivery, nil); err != nil {
		t.Fatal(err)
	}
	if delivery, err := st.ClaimGeofenceDelivery("worker", now, now+90); err != nil || delivery != nil {
		t.Fatalf("completed delivery remains: %+v err=%v", delivery, err)
	}
	if failures, err := st.GeofenceDeliveryFailures(); err != nil || failures != 0 {
		t.Fatalf("failures=%d err=%v", failures, err)
	}
	g.Name = "Work"
	g.RadiusM = 300
	if _, err = st.UpdateGeofence(g.ID, g); err != nil {
		t.Fatal(err)
	}
	if values, err := st.ListGeofences(); err != nil || len(values) != 1 || values[0].Name != "Work" {
		t.Fatalf("values=%+v err=%v", values, err)
	}
	if value, err := st.UpdateGeofenceEnabled(g.ID, false); err != nil || value.Name != "Work" || value.Enabled {
		t.Fatalf("enabled update=%+v err=%v", value, err)
	}
	if err = st.DeleteGeofence(g.ID); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateGeofenceReturnsPersistedMetadata(t *testing.T) {
	st := openTestDB(t)
	created, err := st.CreateGeofence(&store.Geofence{Name: "Home", Lat: 37, Lon: 127, RadiusM: 200, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := st.UpdateGeofence(created.ID, &store.Geofence{
		ID: 999, Name: "Office", Lat: 37.1, Lon: 127.1, RadiusM: 300, Enabled: false,
		CreatedAt: 1, UpdatedAt: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID || updated.CreatedAt != created.CreatedAt || updated.UpdatedAt == 2 {
		t.Fatalf("updated=%+v created=%+v", updated, created)
	}
	values, err := st.ListGeofences()
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0] != *updated {
		t.Fatalf("values=%+v updated=%+v", values, updated)
	}
}

func TestExpiredTimelineAndGeofenceLeasesCannotFinish(t *testing.T) {
	st := openTestDB(t)
	loc := sampleLocation()
	if _, err := st.Insert(loc); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	timeline, err := st.ClaimTimelineJob("timeline", now, now-1)
	if err != nil || timeline == nil {
		t.Fatalf("timeline=%+v err=%v", timeline, err)
	}
	if status, err := st.TimelineJobDiagnostics(now); err != nil || status.Running != 1 || status.ExpiredLeases != 1 {
		t.Fatalf("timeline diagnostics=%+v err=%v", status, err)
	}
	if ok, err := st.RenewTimelineJob(timeline.ID, "timeline", now+90); err != nil || ok {
		t.Fatalf("timeline renew=%t err=%v", ok, err)
	}
	if err := st.FinishTimelineJob(timeline, timeline.DirtyFrom, nil, nil); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("timeline lease err=%v", err)
	}
	g, err := st.CreateGeofence(&store.Geofence{Name: "Home", Lat: loc.Lat, Lon: loc.Lon, RadiusM: 100, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	exit := sampleLocation()
	exit.Tst++
	exit.Lat++
	if _, err := st.Insert(exit); err != nil {
		t.Fatal(err)
	}
	delivery, err := st.ClaimGeofenceDelivery("geofence", now, now-1)
	if err != nil || delivery == nil || delivery.Event.GeofenceID != g.ID {
		t.Fatalf("delivery=%+v err=%v", delivery, err)
	}
	if status, err := st.GeofenceDeliveryDiagnostics(now); err != nil || status.Running != 1 || status.ExpiredLeases != 1 {
		t.Fatalf("geofence diagnostics=%+v err=%v", status, err)
	}
	if err := st.FinishGeofenceDelivery(delivery, nil); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("geofence lease err=%v", err)
	}
}

func TestTimelineJobLifecycle(t *testing.T) {
	st := openTestDB(t)
	loc := sampleLocation()
	if _, err := st.Insert(loc); err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimTimelineJob("owner", time.Now().Unix(), time.Now().Add(time.Minute).Unix())
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if ok, err := st.RenewTimelineJob(job.ID, "owner", time.Now().Add(time.Minute).Unix()); err != nil || !ok {
		t.Fatalf("renew=%v err=%v", ok, err)
	}
	from, err := st.TimelineRebuildFrom(job.User, job.Device, job.DirtyFrom)
	if err != nil {
		t.Fatal(err)
	}
	points, err := st.TimelinePoints(job.User, job.Device, from)
	if err != nil || len(points) != 1 {
		t.Fatalf("points=%+v err=%v", points, err)
	}
	segment := store.TimelineSegment{User: job.User, Device: job.Device, Kind: "trip", StartLocationID: points[0].ID, EndLocationID: points[0].ID, StartTst: points[0].Tst, EndTst: points[0].Tst, PointCount: 1}
	if err = st.FinishTimelineJob(job, from, []store.TimelineSegment{segment}, nil); err != nil {
		t.Fatal(err)
	}
	segments, err := st.ListTimeline(job.User, job.Device, 0, 0, 10)
	if err != nil || len(segments) != 1 {
		t.Fatalf("segments=%+v err=%v", segments, err)
	}
	if n, err := st.TimelineJobCount(); err != nil || n != 0 {
		t.Fatalf("jobs=%d err=%v", n, err)
	}
	if err = st.EnqueueAllTimelines(); err != nil {
		t.Fatal(err)
	}
	if err = st.RecoverTimelineJobs(time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
}

func TestTimelineFollowUpResetsAttempts(t *testing.T) {
	st := openTestDB(t)
	loc := sampleLocation()
	if _, err := st.Insert(loc); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	first, err := st.ClaimTimelineJob("first", now.Unix(), now.Add(time.Minute).Unix())
	if err != nil || first == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if err := st.EnqueueTimeline(loc.User, loc.Device, loc.Tst); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishTimelineJob(first, loc.Tst, nil, nil); err != nil {
		t.Fatal(err)
	}
	second, err := st.ClaimTimelineJob("second", time.Now().Unix(), time.Now().Add(time.Minute).Unix())
	if err != nil || second == nil || second.Attempts != 1 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func TestGeofenceValidationAndDeliveryFailure(t *testing.T) {
	st := openTestDB(t)
	for _, v := range []*store.Geofence{
		{Name: "", Lat: 0, Lon: 0, RadiusM: 25},
		{Name: "x", Lat: 91, Lon: 0, RadiusM: 25},
		{Name: "x", Lat: 0, Lon: 0, RadiusM: 1},
		{Name: "x", Lat: 0, Lon: 0, RadiusM: 25, User: strings.Repeat("u", 129)},
		{Name: "bad\nname", Lat: 0, Lon: 0, RadiusM: 25},
	} {
		if _, err := st.CreateGeofence(v); err == nil {
			t.Fatalf("accepted %+v", v)
		}
	}
	if _, err := st.CreateGeofence(&store.Geofence{Name: "x", Lat: 0, Lon: 0, RadiusM: 25, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for i, lat := range []float64{0, .01} {
		loc := sampleLocation()
		loc.Tst = int64(100 + i)
		loc.Lat = lat
		loc.Lon = 0
		if _, err := st.Insert(loc); err != nil {
			t.Fatal(err)
		}
	}
	for attempt := 1; attempt <= 5; attempt++ {
		now := time.Now().Add(time.Hour).Unix()
		d, err := st.ClaimGeofenceDelivery("owner", now, now+90)
		if err != nil || d == nil {
			t.Fatalf("attempt=%d delivery=%+v err=%v", attempt, d, err)
		}
		if err = st.FinishGeofenceDelivery(d, errors.New("network")); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.GeofenceDeliveryFailures(); err != nil || n != 1 {
		t.Fatalf("failed=%d err=%v", n, err)
	}
	if status, err := st.GeofenceDeliveryDiagnostics(time.Now().Unix()); err != nil || status.Failed != 1 {
		t.Fatalf("diagnostics=%+v err=%v", status, err)
	}
}

func TestDeleteLatestLocationRebuildsGeofenceState(t *testing.T) {
	st := openTestDB(t)
	if _, err := st.CreateGeofence(&store.Geofence{Name: "Home", Lat: 0, Lon: 0, RadiusM: 100, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	insert := func(tst int64, lat float64) int64 {
		loc := sampleLocation()
		loc.Tst = tst
		loc.Lat = lat
		loc.Lon = 0
		id, err := st.Insert(loc)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	insert(100, 0)
	latest := insert(101, .01)
	if err := st.DeleteIDs([]int64{latest}); err != nil {
		t.Fatal(err)
	}
	insert(102, .01)
	events, err := st.ListGeofenceEvents(10)
	if err != nil || len(events) != 1 || events[0].Event != "exit" || events[0].Tst != 102 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestTimelineSummaryAndCursor(t *testing.T) {
	st := openTestDB(t)
	var points []store.Location
	for i := range 3 {
		loc := sampleLocation()
		loc.Tst = int64(100 + i)
		loc.Device = "timeline"
		id, err := st.Insert(loc)
		if err != nil {
			t.Fatal(err)
		}
		loc.ID = id
		points = append(points, *loc)
	}
	job, err := st.ClaimTimelineJob("owner", time.Now().Unix(), time.Now().Add(time.Minute).Unix())
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	segments := make([]store.TimelineSegment, 3)
	for i, p := range points {
		segments[i] = store.TimelineSegment{User: p.User, Device: p.Device, Kind: "trip", StartLocationID: p.ID, EndLocationID: p.ID, StartTst: p.Tst, EndTst: p.Tst, DistanceM: 10, PointCount: 1}
	}
	if err = st.FinishTimelineJob(job, 100, segments, nil); err != nil {
		t.Fatal(err)
	}
	f := store.TimelineFilter{User: points[0].User, Device: "timeline", Limit: 2}
	combinedPage, combinedSummary, err := st.TimelinePageSummaryContext(context.Background(), f)
	if err != nil || len(combinedPage) != 2 || combinedSummary.TotalSegments != 3 || combinedSummary.Trips != 3 {
		t.Fatalf("combined page=%+v summary=%+v err=%v", combinedPage, combinedSummary, err)
	}
	page, err := st.ListTimelinePage(f)
	if err != nil || len(page) != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	f.BeforeTst, f.BeforeID = page[1].StartTst, page[1].ID
	next, err := st.ListTimelinePage(f)
	if err != nil || len(next) != 1 {
		t.Fatalf("next=%+v err=%v", next, err)
	}
	summary, err := st.TimelineSummary(f)
	if err != nil || summary.TotalSegments != 3 || summary.Trips != 3 || summary.DistanceM != 30 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	if err = st.EnqueueTimeline(points[0].User, "timeline", 100); err != nil {
		t.Fatal(err)
	}
	job, err = st.ClaimTimelineJob("owner-2", time.Now().Unix(), time.Now().Add(time.Minute).Unix())
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	clipped := []store.TimelineSegment{{User: job.User, Device: job.Device, Kind: "stay", StartLocationID: points[0].ID, EndLocationID: points[1].ID, StartTst: 100, EndTst: 200, DistanceM: 100, PointCount: 2}, {User: job.User, Device: job.Device, Kind: "trip", StartLocationID: points[1].ID, EndLocationID: points[2].ID, StartTst: 200, EndTst: 300, DistanceM: 100, PointCount: 2}}
	if err = st.FinishTimelineJob(job, 100, clipped, nil); err != nil {
		t.Fatal(err)
	}
	summary, err = st.TimelineSummary(store.TimelineFilter{User: job.User, Device: job.Device, From: 150, To: 250})
	if err != nil || summary.StaySeconds != 50 || summary.DistanceM != 100 {
		t.Fatalf("clipped summary=%+v err=%v", summary, err)
	}
	b, err := json.Marshal(clipped[1])
	if err != nil || !strings.Contains(string(b), `"center_lat":null`) {
		t.Fatalf("json=%s err=%v", b, err)
	}
	clipped[0].CenterLat = store.NullableFloat64{Float64: 1.5, Valid: true}
	b, _ = json.Marshal(clipped[0])
	if !strings.Contains(string(b), `"center_lat":1.5`) {
		t.Fatalf("json=%s", b)
	}
}

func TestTimelinePageSummaryContextHonorsCancellation(t *testing.T) {
	st := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := st.TimelinePageSummaryContext(ctx, store.TimelineFilter{Limit: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func TestImportTimestampSet(t *testing.T) {
	st := openTestDB(t)
	err := st.ImportAtomic(func(tx *store.ImportTx) error {
		fresh, err := tx.MarkImportTimestamp(10)
		if err != nil || !fresh {
			return fmt.Errorf("first timestamp: fresh=%t err=%v", fresh, err)
		}
		fresh, err = tx.MarkImportTimestamp(10)
		if err != nil || fresh {
			return fmt.Errorf("duplicate timestamp: fresh=%t err=%v", fresh, err)
		}
		loc := sampleLocation()
		loc.Device = "import-attempts"
		loc.Tst = 11
		_, err = tx.InsertLocation(loc, 7)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
