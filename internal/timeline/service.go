package timeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/metrics"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/worker"
)

const lease = 90 * time.Second
const segmentBatchSize = 256
const finishTimeout = 15 * time.Second

type Service struct {
	cfg       *config.Manager
	st        *store.Store
	ctx       context.Context
	cancel    context.CancelFunc
	wake      *worker.Signal
	wg        sync.WaitGroup
	heartbeat *worker.Heartbeat
}

func New(cfg *config.Manager, st *store.Store) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{cfg: cfg, st: st, ctx: ctx, cancel: cancel, wake: worker.NewSignal(), heartbeat: worker.NewHeartbeat(time.Now)}
	if err := st.RecoverTimelineJobs(time.Now().Unix()); err != nil {
		slog.Error("recover timeline jobs", "err", err)
	}
	if err := st.EnqueueLegacyTimelineRepairs(); err != nil {
		slog.Error("enqueue legacy timeline repairs", "err", err)
	}
	s.wg.Add(1)
	go s.run()
	s.wake.Notify()
	return s
}

func (s *Service) Reload() error {
	if err := s.st.EnqueueAllTimelines(); err != nil {
		return err
	}
	s.wake.Notify()
	return nil
}
func (s *Service) Wake()         { s.wake.Notify() }
func (s *Service) Healthy() bool { return s.heartbeat.Healthy(30 * time.Second) }
func (s *Service) ShutdownContext(ctx context.Context) error {
	s.cancel()
	return worker.Wait(ctx, &s.wg)
}

func (s *Service) run() {
	defer s.wg.Done()
	owner := time.Now().Format("150405.000000000")
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if s.ctx.Err() != nil {
			return
		}
		s.heartbeat.Beat()
		worked, err := worker.GuardValue("timeline worker", func() (bool, error) { return s.work(owner), nil })
		if err != nil {
			slog.Error("timeline worker", "err", err)
		}
		if worked {
			continue
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake.C():
		case <-tick.C:
		}
	}
}

func (s *Service) work(owner string) (worked bool) {
	if s.ctx.Err() != nil {
		return false
	}
	started := time.Now()
	now := time.Now()
	j, err := s.st.ClaimTimelineJob(owner, now.Unix(), now.Add(lease).Unix())
	if err != nil {
		slog.Error("claim timeline job", "err", err)
		return false
	}
	if j == nil {
		return false
	}
	worked = true
	from := j.DirtyFrom
	ctx, stop := worker.RenewLease(s.ctx, worker.LeaseOptions{
		Interval:      15 * time.Second,
		LeaseUntil:    time.Unix(now.Unix(), 0).Add(lease),
		LeaseDuration: lease,
		Renew: func(ctx context.Context, now time.Time) (bool, error) {
			return s.st.RenewTimelineJobContext(ctx, j.ID, owner, now.Add(lease).Unix())
		},
		OnRenewed: func() {
			s.heartbeat.Beat()
		},
	})
	completed := false
	gapBefore := false
	var rebuild *store.TimelineRebuild
	defer func() {
		stop()
		if rebuild != nil {
			_ = rebuild.Close()
		}
		if recovered := recover(); recovered != nil && !completed {
			cause := fmt.Errorf("timeline worker panic: %v", recovered)
			if finishErr := s.st.FinishTimelineJob(j, from, nil, cause); finishErr != nil && finishErr != store.ErrLeaseLost {
				slog.Error("finish panicked timeline job", "job_id", j.ID, "err", finishErr)
			}
		}
	}()
	if err == nil {
		rebuild, err = s.st.BeginTimelineRebuild(ctx)
	}
	if err == nil {
		from, gapBefore, err = rebuild.RebuildBoundary(ctx, j.User, j.Device, j.DirtyFrom)
	}
	if err == nil {
		builder := newBuilderContext(ctx, j.User, j.Device, s.cfg.Snapshot())
		builder.pendingGap = gapBefore
		segments := make([]store.TimelineSegment, 0, segmentBatchSize)
		builder.setSink(func(segment store.TimelineSegment) error {
			segments = append(segments, segment)
			if len(segments) < segmentBatchSize {
				return nil
			}
			err := rebuild.Append(ctx, segments)
			segments = segments[:0]
			return err
		})
		err = rebuild.IteratePoints(ctx, j.User, j.Device, from, 1000, func(points []store.TrackPoint) error {
			for _, p := range points {
				builder.Add(p)
				if err := builder.Err(); err != nil {
					return err
				}
			}
			return ctx.Err()
		})
		if err == nil {
			builder.Finish()
			err = builder.Err()
		}
		if err == nil && len(segments) > 0 {
			err = rebuild.Append(ctx, segments)
		}
		if err == nil {
			leaseErr := ctx.Err()
			stop()
			if leaseErr != nil {
				err = leaseErr
			} else {
				finishCtx, finishCancel := context.WithTimeout(context.Background(), finishTimeout)
				err = rebuild.Finish(finishCtx, j, from, nil)
				finishCancel()
			}
			if err != nil && err != store.ErrLeaseLost {
				slog.Error("finish timeline job", "err", err)
			}
			completed = err == nil || errors.Is(err, store.ErrLeaseLost)
		}
	}
	if !completed && ctx.Err() != nil && err == nil {
		err = ctx.Err()
	}
	stop()
	s.heartbeat.Beat()
	if rebuild != nil {
		_ = rebuild.Close()
		rebuild = nil
	}
	if !completed && err != nil {
		if finishErr := s.st.FinishTimelineJob(j, from, nil, err); finishErr != nil && finishErr != store.ErrLeaseLost {
			slog.Error("finish timeline job", "err", finishErr)
		}
	}
	completed = true
	backlog, _ := s.st.TimelineJobCount()
	result := "ok"
	if err != nil {
		result = "error"
	}
	metrics.Timeline(result, time.Since(started), backlog)
	return true
}

func Build(user, device string, p []store.TrackPoint, cfg *config.Config) []store.TimelineSegment {
	b := newBuilder(user, device, cfg)
	for _, point := range p {
		b.Add(point)
	}
	return b.Finish()
}

type accumulator struct {
	first, last                   store.TrackPoint
	count                         int
	distance, lat, lonSin, lonCos float64
	gap                           bool
}

func (a *accumulator) add(p store.TrackPoint) {
	if a.count == 0 {
		a.first = p
	} else {
		a.distance += distance(a.last.Lat, a.last.Lon, p.Lat, p.Lon)
	}
	a.last = p
	a.count++
	a.lat += p.Lat
	lon := p.Lon * math.Pi / 180
	a.lonSin += math.Sin(lon)
	a.lonCos += math.Cos(lon)
}
func (a *accumulator) segment(user, device, kind string) store.TimelineSegment {
	v := store.TimelineSegment{User: user, Device: device, Kind: kind, StartLocationID: a.first.ID, EndLocationID: a.last.ID, StartTst: a.first.Tst, EndTst: a.last.Tst, DistanceM: a.distance, PointCount: a.count, GapBefore: a.gap}
	if kind == "stay" {
		v.CenterLat = store.NullableFloat64{Float64: a.lat / float64(a.count), Valid: true}
		v.CenterLon = store.NullableFloat64{Float64: math.Atan2(a.lonSin, a.lonCos) * 180 / math.Pi, Valid: true}
	}
	return v
}

type pendingBounds struct {
	size, length                      int
	valid                             []bool
	minLat, maxLat, lonStart, lonSpan []float64
}

func (t *pendingBounds) reset() { *t = pendingBounds{} }

func (t *pendingBounds) rebuild(points []store.TrackPoint) {
	t.reset()
	if len(points) == 0 {
		return
	}
	size := 1
	for size < len(points) {
		size <<= 1
	}
	t.size, t.length = size, len(points)
	t.valid = make([]bool, size*2)
	t.minLat, t.maxLat = make([]float64, size*2), make([]float64, size*2)
	t.lonStart, t.lonSpan = make([]float64, size*2), make([]float64, size*2)
	for i, p := range points {
		pos := size + i
		if finitePoint(p) {
			t.valid[pos] = true
			t.minLat[pos], t.maxLat[pos] = p.Lat, p.Lat
			t.lonStart[pos] = longitude360(p.Lon)
		}
	}
	for i := size - 1; i > 0; i-- {
		t.merge(i)
	}
}

func (t *pendingBounds) append(index int, p store.TrackPoint) {
	if index >= t.size {
		t.grow(index)
	}
	if index >= t.length {
		t.length = index + 1
	}
	pos := t.size + index
	if finitePoint(p) {
		t.valid[pos] = true
		t.minLat[pos], t.maxLat[pos] = p.Lat, p.Lat
		t.lonStart[pos] = longitude360(p.Lon)
		t.lonSpan[pos] = 0
	} else {
		t.valid[pos] = false
	}
	for pos >>= 1; pos > 0; pos >>= 1 {
		t.merge(pos)
	}
}

func (t *pendingBounds) grow(index int) {
	oldSize, oldLength := t.size, t.length
	newSize := 1
	for newSize <= index {
		newSize <<= 1
	}
	valid := make([]bool, newSize*2)
	minLat := make([]float64, newSize*2)
	maxLat := make([]float64, newSize*2)
	lonStart := make([]float64, newSize*2)
	lonSpan := make([]float64, newSize*2)
	for i := range oldLength {
		oldPos, newPos := oldSize+i, newSize+i
		valid[newPos] = t.valid[oldPos]
		minLat[newPos], maxLat[newPos] = t.minLat[oldPos], t.maxLat[oldPos]
		lonStart[newPos], lonSpan[newPos] = t.lonStart[oldPos], t.lonSpan[oldPos]
	}
	t.size = newSize
	t.valid, t.minLat, t.maxLat = valid, minLat, maxLat
	t.lonStart, t.lonSpan = lonStart, lonSpan
	t.length = oldLength
	for i := newSize - 1; i > 0; i-- {
		t.merge(i)
	}
}

func (t *pendingBounds) merge(pos int) {
	left, right := pos*2, pos*2+1
	if !t.valid[left] || !t.valid[right] {
		t.valid[pos] = false
		return
	}
	t.valid[pos] = true
	t.minLat[pos] = math.Min(t.minLat[left], t.minLat[right])
	t.maxLat[pos] = math.Max(t.maxLat[left], t.maxLat[right])
	t.lonStart[pos], t.lonSpan[pos] = mergeLongitude(
		t.lonStart[left], t.lonSpan[left], t.lonStart[right], t.lonSpan[right],
	)
}

func (t *pendingBounds) firstOutside(start int, points []store.TrackPoint, pointBase int, anchor store.TrackPoint, radius float64, canceled func() bool) int {
	if start >= t.length {
		return -1
	}
	nodes := 0
	index, stopped := t.findOutside(1, 0, t.size, start, points, pointBase, anchor, radius, &nodes, canceled)
	if stopped {
		return -2
	}
	return index
}

func (t *pendingBounds) findOutside(pos, lo, hi, start int, points []store.TrackPoint, pointBase int, anchor store.TrackPoint, radius float64, nodes *int, canceled func() bool) (int, bool) {
	*nodes = *nodes + 1
	if *nodes&255 == 0 && canceled != nil && canceled() {
		return -1, true
	}
	if hi <= start || lo >= t.length {
		return -1, false
	}
	if lo >= start && hi <= t.length && t.inside(pos, anchor, radius) {
		return -1, false
	}
	if hi-lo == 1 {
		if distance(anchor.Lat, anchor.Lon, points[lo-pointBase].Lat, points[lo-pointBase].Lon) > radius {
			return lo, false
		}
		return -1, false
	}
	mid := lo + (hi-lo)/2
	if index, stopped := t.findOutside(pos*2, lo, mid, start, points, pointBase, anchor, radius, nodes, canceled); stopped || index >= 0 {
		return index, stopped
	}
	return t.findOutside(pos*2+1, mid, hi, start, points, pointBase, anchor, radius, nodes, canceled)
}

func (t *pendingBounds) inside(pos int, anchor store.TrackPoint, radius float64) bool {
	if !t.valid[pos] || !finitePoint(anchor) {
		return false
	}
	maxLat := math.Max(math.Abs(anchor.Lat-t.minLat[pos]), math.Abs(anchor.Lat-t.maxLat[pos]))
	maxLat = math.Min(maxLat, 180)
	maxLon := maxLongitudeDistance(anchor.Lon, t.lonStart[pos], t.lonSpan[pos])
	lat := math.Sin(maxLat * math.Pi / 360)
	lon := math.Sin(maxLon * math.Pi / 360)
	x := lat*lat + lon*lon
	if x > 1 {
		x = 1
	}
	bound := 2 * 6371008.8 * math.Asin(math.Sqrt(x))
	return bound+pruningToleranceM <= radius*(1-1e-12)
}

const longitudeDegrees = 360.0
const pruningToleranceM = 1e-6

func longitude360(lon float64) float64 {
	lon += 180
	if lon >= longitudeDegrees {
		lon -= longitudeDegrees
	}
	return lon
}

func angularDistance(a, b float64) float64 {
	d := math.Abs(a - b)
	if d > 180 {
		d = longitudeDegrees - d
	}
	return d
}

func clockwiseDistance(from, to float64) float64 {
	d := to - from
	if d < 0 {
		d += longitudeDegrees
	}
	return d
}

func longitudeArcEnd(start, span float64) float64 {
	end := start + span
	if end >= longitudeDegrees {
		end -= longitudeDegrees
	}
	return end
}

type longitudeInterval struct{ start, end float64 }

func addLongitudeInterval(intervals *[4]longitudeInterval, count *int, start, span float64) {
	end := start + span
	if end <= longitudeDegrees {
		intervals[*count] = longitudeInterval{start: start, end: end}
		*count = *count + 1
		return
	}
	intervals[*count] = longitudeInterval{start: start, end: longitudeDegrees}
	*count = *count + 1
	intervals[*count] = longitudeInterval{start: 0, end: end - longitudeDegrees}
	*count = *count + 1
}

func mergeLongitude(leftStart, leftSpan, rightStart, rightSpan float64) (start, span float64) {
	if leftSpan >= longitudeDegrees || rightSpan >= longitudeDegrees {
		return 0, longitudeDegrees
	}
	var intervals [4]longitudeInterval
	count := 0
	addLongitudeInterval(&intervals, &count, leftStart, leftSpan)
	addLongitudeInterval(&intervals, &count, rightStart, rightSpan)
	for i := 1; i < count; i++ {
		v := intervals[i]
		j := i
		for j > 0 && intervals[j-1].start > v.start {
			intervals[j] = intervals[j-1]
			j--
		}
		intervals[j] = v
	}
	merged := intervals
	mergedCount := 0
	for i := 0; i < count; i++ {
		if mergedCount == 0 || intervals[i].start > merged[mergedCount-1].end {
			merged[mergedCount] = intervals[i]
			mergedCount++
			continue
		}
		if intervals[i].end > merged[mergedCount-1].end {
			merged[mergedCount-1].end = intervals[i].end
		}
	}
	maxGap := -1.0
	gapEnd := 0.0
	for i := 0; i < mergedCount; i++ {
		next := (i + 1) % mergedCount
		gap := merged[next].start - merged[i].end
		if next == 0 {
			gap += longitudeDegrees
		}
		if gap > maxGap {
			maxGap = gap
			gapEnd = merged[next].start
		}
	}
	if maxGap <= 0 {
		return 0, longitudeDegrees
	}
	span = longitudeDegrees - maxGap
	start = gapEnd
	if start >= longitudeDegrees {
		start -= longitudeDegrees
	}
	return start, span
}

func maxLongitudeDistance(anchor, start, span float64) float64 {
	if span >= longitudeDegrees {
		return 180
	}
	anchor = longitude360(anchor)
	end := longitudeArcEnd(start, span)
	max := math.Max(angularDistance(anchor, start), angularDistance(anchor, end))
	antipode := anchor + 180
	if antipode >= longitudeDegrees {
		antipode -= longitudeDegrees
	}
	if clockwiseDistance(start, antipode) <= span {
		return 180
	}
	return max
}

func finitePoint(p store.TrackPoint) bool {
	return p.Lat >= -90 && p.Lat <= 90 && p.Lon >= -180 && p.Lon <= 180 && !math.IsNaN(p.Lat) && !math.IsNaN(p.Lon) && !math.IsInf(p.Lat, 0) && !math.IsInf(p.Lon, 0)
}

type builder struct {
	user, device      string
	radius            float64
	staySecs, gapSecs int64
	ctx               context.Context
	pending           []store.TrackPoint
	pendingBase       int
	pendingBounds     pendingBounds
	pendingGap        bool
	trip, stay        accumulator
	staying           bool
	anchor            store.TrackPoint
	out               []store.TimelineSegment
	sink              func(store.TimelineSegment) error
	sinkErr           error
}

func newBuilder(user, device string, cfg *config.Config) *builder {
	return newBuilderContext(context.Background(), user, device, cfg)
}
func newBuilderContext(ctx context.Context, user, device string, cfg *config.Config) *builder {
	if ctx == nil {
		ctx = context.Background()
	}
	return &builder{user: user, device: device, radius: float64(cfg.StayRadiusM), staySecs: int64(cfg.StayDuration().Seconds()), gapSecs: int64(cfg.TripGapDuration().Seconds()), ctx: ctx}
}
func (b *builder) emit(kind string, a *accumulator) {
	if a.count == 0 || b.sinkErr != nil {
		return
	}
	segment := a.segment(b.user, b.device, kind)
	if b.sink != nil {
		b.sinkErr = b.sink(segment)
	} else {
		b.out = append(b.out, segment)
	}
	*a = accumulator{}
}

func (b *builder) setSink(sink func(store.TimelineSegment) error) { b.sink = sink }

func (b *builder) Err() error { return b.sinkErr }
func (b *builder) canceled() bool {
	if b.ctx == nil || b.ctx.Done() == nil {
		return false
	}
	select {
	case <-b.ctx.Done():
		b.sinkErr = b.ctx.Err()
		return true
	default:
		return false
	}
}
func (b *builder) addTripPoint(p store.TrackPoint) {
	if b.trip.count == 0 {
		b.trip.gap = b.pendingGap
	}
	b.trip.add(p)
}
func (b *builder) pendingLen() int { return len(b.pending) }
func (b *builder) appendPending(p store.TrackPoint) {
	b.pending = append(b.pending, p)
	b.pendingBounds.append(b.pendingBase+len(b.pending)-1, p)
}
func (b *builder) resetPending() {
	b.pending = nil
	b.pendingBase = 0
	b.pendingBounds.reset()
}
func (b *builder) compactPending() bool {
	if b.pendingBase < 1024 || b.pendingBase*2 < b.pendingBounds.length {
		return true
	}
	active := make([]store.TrackPoint, len(b.pending))
	for i, p := range b.pending {
		if i&255 == 0 && b.canceled() {
			return false
		}
		active[i] = p
	}
	b.pending = active
	b.pendingBase = 0
	b.pendingBounds.rebuild(active)
	return true
}

func (b *builder) Add(p store.TrackPoint) {
	if b.sinkErr != nil || b.canceled() {
		return
	}
	if b.staying {
		gap := p.Tst-b.stay.last.Tst > b.gapSecs
		if gap || distance(b.anchor.Lat, b.anchor.Lon, p.Lat, p.Lon) > b.radius {
			last := b.stay.last
			b.emit("stay", &b.stay)
			b.staying = false
			b.pendingGap = gap
			if !gap {
				b.addTripPoint(last)
			}
			b.Add(p)
			return
		}
		b.stay.add(p)
		return
	}
	if b.pendingLen() > 0 && p.Tst-b.pending[len(b.pending)-1].Tst > b.gapSecs {
		for i, q := range b.pending {
			if i&255 == 0 && b.canceled() {
				return
			}
			if b.trip.count == 0 {
				b.trip.gap = b.pendingGap
			}
			b.trip.add(q)
		}
		b.resetPending()
		b.emit("trip", &b.trip)
		b.pendingGap = true
	}
	if b.pendingLen() > 0 && distance(b.pending[0].Lat, b.pending[0].Lon, p.Lat, p.Lon) <= b.radius {
		b.appendPending(p)
		if p.Tst-b.pending[0].Tst >= b.staySecs {
			b.startStay(b.pendingLen())
		}
		return
	}
	b.appendPending(p)
	for b.pendingLen() > 0 {
		anchor := b.pending[0]
		outside := b.pendingBounds.firstOutside(b.pendingBase+1, b.pending, b.pendingBase, anchor, b.radius, b.canceled)
		if outside == -2 {
			return
		}
		end := b.pendingLen()
		if outside >= 0 {
			end = outside - b.pendingBase
		}
		if end > 1 && b.pending[end-1].Tst-anchor.Tst >= b.staySecs {
			b.startStay(end)
			return
		}
		if outside < 0 {
			return
		}
		if b.trip.count == 0 {
			b.trip.gap = b.pendingGap
		}
		b.trip.add(anchor)
		b.pending = b.pending[1:]
		b.pendingBase++
		b.pendingGap = false
		if b.pendingLen() == 0 {
			b.resetPending()
		} else if !b.compactPending() {
			return
		}
	}
}

func (b *builder) startStay(end int) {
	candidate := b.pending[:end]
	rest := b.pending[end:]
	if b.trip.count > 0 {
		b.addTripPoint(candidate[0])
	}
	b.emit("trip", &b.trip)
	b.resetPending()
	b.stay = accumulator{gap: b.pendingGap}
	for i, point := range candidate {
		if i&255 == 0 && b.canceled() {
			return
		}
		b.stay.add(point)
	}
	b.anchor = candidate[0]
	b.staying = true
	b.pendingGap = false
	for _, point := range rest {
		b.Add(point)
	}
}

func (b *builder) Finish() []store.TimelineSegment {
	if b.sinkErr != nil {
		return b.out
	}
	if b.staying {
		b.emit("stay", &b.stay)
	}
	for i, p := range b.pending {
		if i&255 == 0 && b.canceled() {
			return b.out
		}
		b.addTripPoint(p)
	}
	b.resetPending()
	b.emit("trip", &b.trip)
	return b.out
}

func distance(a, b, c, d float64) float64 {
	const r = 6371008.8
	p1 := a * math.Pi / 180
	p2 := c * math.Pi / 180
	dp := (c - a) * math.Pi / 180
	dl := (d - b) * math.Pi / 180
	x := math.Sin(dp/2)*math.Sin(dp/2) + math.Cos(p1)*math.Cos(p2)*math.Sin(dl/2)*math.Sin(dl/2)
	x = math.Max(0, math.Min(1, x))
	return 2 * r * math.Atan2(math.Sqrt(x), math.Sqrt(1-x))
}
