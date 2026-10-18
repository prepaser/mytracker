package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/timeline"
)

func TestTimelineDeletionInvalidatesInteriorPointAndRunningRebuild(t *testing.T) {
	st := openTestDB(t)
	proofs := t.TempDir()
	points := []store.TrackPoint{{Tst: 100, Lat: 10}, {Tst: 200, Lat: 10.001}, {Tst: 900, Lat: 10}}
	for i, point := range points {
		id, err := st.Insert(&store.Location{User: "u", Device: "d", Tst: point.Tst, Lat: point.Lat, Payload: []byte(`{}`), Digest: make([]byte, 32)})
		if err != nil {
			t.Fatal(err)
		}
		points[i].ID = id
	}
	cfg := config.Default()
	segments := timeline.Build("u", "d", points, cfg)
	if len(segments) != 1 || segments[0].Kind != "stay" || segments[0].PointCount != 3 {
		t.Fatalf("segments=%+v", segments)
	}
	now := time.Now().Unix()
	job, err := st.ClaimTimelineJob("initial", now, now+90)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if err := st.FinishTimelineJob(job, 100, segments, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueTimeline("u", "d", 100); err != nil {
		t.Fatal(err)
	}
	old, err := st.ClaimTimelineJob("old", now, now+90)
	if err != nil || old == nil {
		t.Fatalf("job=%+v err=%v", old, err)
	}
	if n, err := st.DeleteRange(proofs, store.ExportFilter{From: 200, To: 200}); err != nil || n != 1 {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
	if err := st.FinishTimelineJob(old, 100, segments, nil); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("deleted generation retained its lease: %v", err)
	}
	if got, err := st.ListTimeline("u", "d", 0, 0, 10); err != nil || len(got) != 0 {
		t.Fatalf("deleted point remains in derived segments: %+v err=%v", got, err)
	}
	job, err = st.ClaimTimelineJob("current", now, now+90)
	if err != nil || job == nil {
		t.Fatalf("follow-up=%+v err=%v", job, err)
	}
	remaining := []store.TrackPoint{points[0], points[2]}
	if err := st.FinishTimelineJob(job, 100, timeline.Build("u", "d", remaining, cfg), nil); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListTimeline("u", "d", 0, 0, 10)
	if err != nil || len(got) != 1 || got[0].PointCount != 2 || got[0].CenterLat.Float64 != 10 {
		t.Fatalf("rebuilt=%+v err=%v", got, err)
	}
	if err := st.ResetTimelines(); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ListTimeline("u", "d", 0, 0, 10); err != nil || len(got) != 0 {
		t.Fatalf("scrubbed=%+v err=%v", got, err)
	}
	if count, err := st.Count(); err != nil || count != 2 {
		t.Fatalf("scrub changed raw locations: count=%d err=%v", count, err)
	}
}

func TestTimelineRebuildStagesUntilAtomicFinish(t *testing.T) {
	st := openTestDB(t)
	loc := sampleLocation()
	id, err := st.Insert(loc)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	job, err := st.ClaimTimelineJob("owner", now, now+90)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	rebuild, err := st.BeginTimelineRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rebuild.Close() }()
	segment := store.TimelineSegment{
		User: loc.User, Device: loc.Device, Kind: "trip",
		StartLocationID: id, EndLocationID: id,
		StartTst: loc.Tst, EndTst: loc.Tst, PointCount: 1,
	}
	if err := rebuild.Append(context.Background(), []store.TimelineSegment{segment}); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ListTimeline(loc.User, loc.Device, 0, 0, 10); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Fatalf("staged segments became visible: %+v", got)
	}
	if err := rebuild.Finish(context.Background(), job, job.DirtyFrom, nil); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListTimeline(loc.User, loc.Device, 0, 0, 10)
	if err != nil || len(got) != 1 || got[0].StartLocationID != id {
		t.Fatalf("segments=%+v err=%v", got, err)
	}
}

func TestTimelineRebuildCloseDropsConnectionLocalStage(t *testing.T) {
	st := openTestDB(t)
	for range 3 {
		rebuild, err := st.BeginTimelineRebuild(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := rebuild.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTimelineRebuildLeaseLossDoesNotReplaceResults(t *testing.T) {
	st := openTestDB(t)
	loc := sampleLocation()
	id, err := st.Insert(loc)
	if err != nil {
		t.Fatal(err)
	}
	old := store.TimelineSegment{
		User: loc.User, Device: loc.Device, Kind: "trip",
		StartLocationID: id, EndLocationID: id,
		StartTst: loc.Tst, EndTst: loc.Tst, PointCount: 1,
	}
	job, err := st.ClaimTimelineJob("first", time.Now().Unix(), time.Now().Add(time.Minute).Unix())
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if err := st.FinishTimelineJob(job, job.DirtyFrom, []store.TimelineSegment{old}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueTimeline(loc.User, loc.Device, loc.Tst); err != nil {
		t.Fatal(err)
	}
	job, err = st.ClaimTimelineJob("second", time.Now().Unix(), time.Now().Add(time.Minute).Unix())
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	rebuild, err := st.BeginTimelineRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rebuild.Close() }()
	updated := old
	updated.Kind = "stay"
	if err := rebuild.Append(context.Background(), []store.TimelineSegment{updated}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimTimelineJob("replacement", time.Now().Add(2*time.Minute).Unix(), time.Now().Add(3*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := rebuild.Finish(context.Background(), job, job.DirtyFrom, nil); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("finish err=%v", err)
	}
	got, err := st.ListTimeline(loc.User, loc.Device, 0, 0, 10)
	if err != nil || len(got) != 1 || got[0].Kind != "trip" {
		t.Fatalf("segments=%+v err=%v", got, err)
	}
}

func TestTimelineRebuildKeepsSharedPrefixAtBoundary(t *testing.T) {
	st := openTestDB(t)
	points := []int64{1, 601, 1801, 2401, 3001, 3601}
	ids := make([]int64, len(points))
	for i, tst := range points {
		id, err := st.Insert(&store.Location{User: "u", Device: "d", Tst: tst, Lat: float64(i), Payload: []byte(`{}`), Digest: []byte{byte(i + 1)}})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	segments := []store.TimelineSegment{
		{User: "u", Device: "d", Kind: "stay", StartLocationID: ids[0], EndLocationID: ids[1], StartTst: points[0], EndTst: points[1], PointCount: 2},
		{User: "u", Device: "d", Kind: "trip", StartLocationID: ids[1], EndLocationID: ids[2], StartTst: points[1], EndTst: points[2], PointCount: 2},
		{User: "u", Device: "d", Kind: "stay", StartLocationID: ids[2], EndLocationID: ids[3], StartTst: points[2], EndTst: points[3], PointCount: 2},
		{User: "u", Device: "d", Kind: "trip", StartLocationID: ids[3], EndLocationID: ids[4], StartTst: points[3], EndTst: points[4], PointCount: 2},
		{User: "u", Device: "d", Kind: "stay", StartLocationID: ids[4], EndLocationID: ids[5], StartTst: points[4], EndTst: points[5], PointCount: 2},
	}
	now := time.Now().Unix()
	job, err := st.ClaimTimelineJob("first", now, now+90)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if err := st.FinishTimelineJob(job, points[0], segments, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueTimeline("u", "d", points[4]); err != nil {
		t.Fatal(err)
	}
	job, err = st.ClaimTimelineJob("second", now, now+90)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	rebuild, err := st.BeginTimelineRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rebuild.Close() }()
	from, err := rebuild.RebuildFrom(context.Background(), "u", "d", points[4])
	if err != nil {
		t.Fatal(err)
	}
	if from != points[2] {
		t.Fatalf("from=%d", from)
	}
	if err := rebuild.Append(context.Background(), segments[2:]); err != nil {
		t.Fatal(err)
	}
	if err := rebuild.Finish(context.Background(), job, from, nil); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListTimeline("u", "d", 0, 0, 10)
	if err != nil || len(got) != len(segments) {
		t.Fatalf("segments=%+v err=%v", got, err)
	}
	var prefix bool
	for _, segment := range got {
		if segment.Kind == "trip" && segment.StartLocationID == ids[1] && segment.EndLocationID == ids[2] {
			prefix = true
		}
	}
	if !prefix {
		t.Fatalf("prefix trip was lost: %+v", got)
	}
}

func TestEnqueueLegacyTimelineRepairsStopsAfterRebuild(t *testing.T) {
	st := openTestDB(t)
	points := []int64{1, 601, 1801}
	ids := make([]int64, len(points))
	for i, tst := range points {
		id, err := st.Insert(&store.Location{User: "u", Device: "d", Tst: tst, Lat: float64(i), Payload: []byte(`{}`), Digest: []byte{byte(i + 1)}})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	old := []store.TimelineSegment{
		{User: "u", Device: "d", Kind: "stay", StartLocationID: ids[0], EndLocationID: ids[1], StartTst: points[0], EndTst: points[1], PointCount: 2},
		{User: "u", Device: "d", Kind: "trip", StartLocationID: ids[2], EndLocationID: ids[2], StartTst: points[2], EndTst: points[2], PointCount: 1},
	}
	now := time.Now().Unix()
	job, err := st.ClaimTimelineJob("initial", now, now+90)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if err := st.FinishTimelineJob(job, points[0], old, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueLegacyTimelineRepairs(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueLegacyTimelineRepairs(); err != nil {
		t.Fatal(err)
	}
	job, err = st.ClaimTimelineJob("repair", now, now+90)
	if err != nil || job == nil || job.DirtyFrom != points[0] {
		t.Fatalf("repair job=%+v err=%v", job, err)
	}
	rebuild, err := st.BeginTimelineRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rebuild.Close() }()
	fixed := []store.TimelineSegment{
		old[0],
		{User: "u", Device: "d", Kind: "trip", StartLocationID: ids[1], EndLocationID: ids[2], StartTst: points[1], EndTst: points[2], PointCount: 2},
	}
	if err := rebuild.Append(context.Background(), fixed); err != nil {
		t.Fatal(err)
	}
	if err := rebuild.Finish(context.Background(), job, points[0], nil); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueLegacyTimelineRepairs(); err != nil {
		t.Fatal(err)
	}
	if job, err := st.ClaimTimelineJob("again", now+1, now+91); err != nil || job != nil {
		t.Fatalf("unexpected repeat repair job=%+v err=%v", job, err)
	}
}
