package timeline

import (
	"math"
	"testing"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
)

func TestBuildTripsAndStays(t *testing.T) {
	cfg := config.Default()
	points := []store.TrackPoint{{ID: 1, Tst: 0, Lat: 37, Lon: 127}, {ID: 2, Tst: 600, Lat: 37.0001, Lon: 127.0001}, {ID: 3, Tst: 660, Lat: 37.01, Lon: 127.01}, {ID: 4, Tst: 720, Lat: 37.02, Lon: 127.02}}
	got := Build("u", "d", points, cfg)
	if len(got) != 2 {
		t.Fatalf("segments=%+v", got)
	}
	if got[0].Kind != "stay" || got[0].PointCount != 2 {
		t.Fatalf("stay=%+v", got[0])
	}
	if got[1].Kind != "trip" || got[1].StartLocationID != 2 || got[1].PointCount != 3 || got[1].DistanceM <= 0 {
		t.Fatalf("trip=%+v", got[1])
	}
}

func TestBuildRetainsKnownMovementBetweenStays(t *testing.T) {
	cfg := config.Default()
	points := []store.TrackPoint{
		{ID: 1, Tst: 1, Lat: 0, Lon: 0},
		{ID: 2, Tst: 601, Lat: 0, Lon: 0},
		{ID: 3, Tst: 1801, Lat: 0, Lon: 0.1},
		{ID: 4, Tst: 2401, Lat: 0, Lon: 0.1},
	}
	got := Build("u", "d", points, cfg)
	if len(got) != 3 {
		t.Fatalf("segments=%+v", got)
	}
	trip := got[1]
	if trip.Kind != "trip" || trip.StartLocationID != 2 || trip.EndLocationID != 3 || trip.PointCount != 2 {
		t.Fatalf("trip=%+v", trip)
	}
	wantDistance := distance(points[1].Lat, points[1].Lon, points[2].Lat, points[2].Lon)
	if math.Abs(trip.DistanceM-wantDistance) > 1e-9 {
		t.Fatalf("trip distance=%f want=%f", trip.DistanceM, wantDistance)
	}
	if got[2].Kind != "stay" || got[2].StartLocationID != 3 || got[2].PointCount != 2 {
		t.Fatalf("stay=%+v", got[2])
	}
}

func TestBuildRetainsBothTripBoundaryEdges(t *testing.T) {
	cfg := config.Default()
	points := []store.TrackPoint{
		{ID: 1, Tst: 1, Lat: 0, Lon: 0},
		{ID: 2, Tst: 601, Lat: 0, Lon: 0},
		{ID: 3, Tst: 1201, Lat: 0, Lon: 0.1},
		{ID: 4, Tst: 1301, Lat: 0, Lon: 0.2},
		{ID: 5, Tst: 1901, Lat: 0, Lon: 0.3},
		{ID: 6, Tst: 2501, Lat: 0, Lon: 0.3},
	}
	got := Build("u", "d", points, cfg)
	if len(got) != 3 {
		t.Fatalf("segments=%+v", got)
	}
	trip := got[1]
	if trip.Kind != "trip" || trip.StartLocationID != 2 || trip.EndLocationID != 5 || trip.PointCount != 4 {
		t.Fatalf("trip=%+v", trip)
	}
	wantDistance := distance(0, 0, 0, 0.1) + distance(0, 0.1, 0, 0.2) + distance(0, 0.2, 0, 0.3)
	if math.Abs(trip.DistanceM-wantDistance) > 1e-9 {
		t.Fatalf("trip distance=%f want=%f", trip.DistanceM, wantDistance)
	}
	if got[2].Kind != "stay" || got[2].StartLocationID != 5 || got[2].PointCount != 2 {
		t.Fatalf("stay=%+v", got[2])
	}
}

func TestDistanceAntipodalIsFinite(t *testing.T) {
	got := distance(88.2, 0, -88.2, 180)
	want := math.Pi * 6371008.8
	if math.IsNaN(got) || math.IsInf(got, 0) || math.Abs(got-want) > 1e-6 {
		t.Fatalf("distance=%f want=%f", got, want)
	}
}

func TestBuildStayCenterHandlesDateline(t *testing.T) {
	cfg := config.Default()
	cfg.StayRadiusM = 500
	got := Build("u", "d", []store.TrackPoint{
		{ID: 1, Tst: 1, Lat: 0, Lon: 179.999},
		{ID: 2, Tst: 601, Lat: 0, Lon: -179.999},
	}, cfg)
	if len(got) != 1 || got[0].Kind != "stay" || !got[0].CenterLon.Valid {
		t.Fatalf("segments=%+v", got)
	}
	if math.Abs(math.Abs(got[0].CenterLon.Float64)-180) > 1e-9 {
		t.Fatalf("center_lon=%f", got[0].CenterLon.Float64)
	}
}

func TestBuildMarksGap(t *testing.T) {
	cfg := config.Default()
	got := Build("u", "d", []store.TrackPoint{{ID: 1, Tst: 1, Lat: 1, Lon: 1}, {ID: 2, Tst: 4000, Lat: 2, Lon: 2}}, cfg)
	if len(got) != 2 || !got[1].GapBefore {
		t.Fatalf("segments=%+v", got)
	}
}

func TestBuilderBoundsPendingWindow(t *testing.T) {
	cfg := config.Default()
	cfg.StayMinDuration = "24h"
	b := newBuilder("u", "d", cfg)
	for i := range 100000 {
		b.Add(store.TrackPoint{ID: int64(i + 1), Tst: int64(i), Lat: 37, Lon: 127})
	}
	if len(b.pending) > 1 {
		t.Fatalf("pending=%d", len(b.pending))
	}
	segments := b.Finish()
	if len(segments) != 1 || segments[0].Kind != "stay" || segments[0].PointCount != 100000 {
		t.Fatalf("segments=%+v", segments)
	}
}

func TestBuilderRetainsShiftedPendingAnchor(t *testing.T) {
	cfg := config.Default()
	cfg.StayMinDuration = "10m"
	b := newBuilder("u", "d", cfg)
	b.Add(store.TrackPoint{ID: 1, Tst: 0, Lat: 0, Lon: 0})
	b.Add(store.TrackPoint{ID: 2, Tst: 1, Lat: 0, Lon: 0})
	b.Add(store.TrackPoint{ID: 3, Tst: 2, Lat: 0, Lon: 0})
	b.Add(store.TrackPoint{ID: 4, Tst: 3, Lat: 0, Lon: 0.01})
	b.Add(store.TrackPoint{ID: 5, Tst: 603, Lat: 0, Lon: 0.01})
	got := b.Finish()
	if len(got) != 2 {
		t.Fatalf("segments=%+v", got)
	}
	if got[0].Kind != "trip" || got[0].PointCount != 4 || got[0].StartLocationID != 1 || got[0].EndLocationID != 4 {
		t.Fatalf("trip=%+v", got[0])
	}
	if got[1].Kind != "stay" || got[1].PointCount != 2 || got[1].StartLocationID != 4 {
		t.Fatalf("stay=%+v", got[1])
	}
}

func TestBuilderStreamsSegmentsWithoutAccumulatingOutput(t *testing.T) {
	cfg := config.Default()
	cfg.StayMinDuration = "24h"
	b := newBuilder("u", "d", cfg)
	count := 0
	b.setSink(func(store.TimelineSegment) error {
		count++
		return nil
	})
	for i := range 20000 {
		b.Add(store.TrackPoint{ID: int64(i + 1), Tst: int64(i) * 4000, Lat: 37, Lon: 127})
	}
	b.Finish()
	if err := b.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 20000 {
		t.Fatalf("streamed segments=%d", count)
	}
	if len(b.out) != 0 {
		t.Fatalf("builder retained %d output segments", len(b.out))
	}
}

func TestPendingBoundsMatchesLinearAcrossDateline(t *testing.T) {
	points := make([]store.TrackPoint, 0, 16)
	for i, lon := range []float64{-179.9999, 179.9999, -170, 170, -90, 90, -1, 1, 0, 180, -180, 45, -45, 135, -135, 179} {
		points = append(points, store.TrackPoint{ID: int64(i + 1), Lat: float64(i%5 - 2), Lon: lon})
	}
	anchors := []store.TrackPoint{
		{Lat: 0, Lon: 180},
		{Lat: 0, Lon: -180},
		{Lat: 0, Lon: 0},
		{Lat: 70, Lon: 179.5},
	}
	radii := []float64{1, 100, 10000, 1000000, 20000000}
	for _, anchor := range anchors {
		for _, radius := range radii {
			want := -1
			for i, point := range points {
				if distance(anchor.Lat, anchor.Lon, point.Lat, point.Lon) > radius {
					want = i
					break
				}
			}
			var bounds pendingBounds
			bounds.rebuild(points)
			if got := bounds.firstOutside(0, points, 0, anchor, radius, nil); got != want {
				t.Fatalf("anchor=%+v radius=%g got=%d want=%d", anchor, radius, got, want)
			}
		}
	}
}

func TestPendingBoundsKeepsFloatingDistanceResidue(t *testing.T) {
	anchor := store.TrackPoint{Lat: 90, Lon: 180}
	points := []store.TrackPoint{{Lat: 90, Lon: -180}}
	radius := distance(anchor.Lat, anchor.Lon, points[0].Lat, points[0].Lon) * 0.999
	var bounds pendingBounds
	bounds.rebuild(points)
	if got := bounds.firstOutside(0, points, 0, anchor, radius, nil); got != 0 {
		t.Fatalf("got=%d want=0 radius=%g", got, radius)
	}
}

func benchmarkPendingOutside(b *testing.B, n int, dateline bool) {
	cfg := config.Default()
	cfg.StayMinDuration = "24h"
	points := make([]store.TrackPoint, n+1)
	for i := range n {
		lon := 127.0001
		if i&1 == 1 {
			lon = 126.9999
		}
		if dateline {
			if i&1 == 0 {
				lon = 179.9999
			} else {
				lon = -179.9999
			}
		}
		points[i] = store.TrackPoint{ID: int64(i + 1), Tst: int64(i + 1), Lat: 0, Lon: lon}
	}
	lastLon := 127.003
	if dateline {
		lastLon = 179.997
	}
	points[n] = store.TrackPoint{ID: int64(n + 1), Tst: int64(n + 1), Lat: 0, Lon: lastLon}
	b.ResetTimer()
	for range b.N {
		_ = Build("u", "d", points, cfg)
	}
}

func BenchmarkPendingOutsideOrdinary8k(b *testing.B) { benchmarkPendingOutside(b, 8000, false) }
func BenchmarkPendingOutsideDateline8k(b *testing.B) { benchmarkPendingOutside(b, 8000, true) }
