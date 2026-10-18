package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/realtime"
	"github.com/prepaser/mytracker/internal/store"
)

type sseTestWriter struct {
	mu          sync.Mutex
	header      http.Header
	body        bytes.Buffer
	status      int
	flushed     bool
	deadline    time.Time
	deadlineErr error
	writes      chan struct{}
}

func (w *sseTestWriter) Header() http.Header { return w.header }
func (w *sseTestWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = status
}
func (w *sseTestWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.body.Write(p)
	w.mu.Unlock()
	if w.writes != nil {
		select {
		case w.writes <- struct{}{}:
		default:
		}
	}
	return n, err
}
func (w *sseTestWriter) Flush() {
	w.mu.Lock()
	w.flushed = true
	w.mu.Unlock()
}

func (w *sseTestWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return w.deadlineErr
}

func (w *sseTestWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func TestSSEWireContract(t *testing.T) {
	w := &sseTestWriter{header: make(http.Header)}
	started := time.Now()
	if !startSSE(w) {
		t.Fatal("stream did not start")
	}
	if err := writeEvent(w, "change", adminEvent{
		Topics: []string{store.ChangeLocations}, LocationIDs: []int64{7}, All: false,
	}); err != nil {
		t.Fatal(err)
	}
	if got := w.header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type=%q", got)
	}
	if got := w.header.Get("Cache-Control"); got != "no-store, no-transform" {
		t.Fatalf("cache-control=%q", got)
	}
	want := "retry: 3000\n\nevent: change\ndata: {\"topics\":[\"locations\"],\"location_ids\":[7],\"all\":false}\n\n"
	if w.String() != want || !w.flushed {
		t.Fatalf("body=%q flushed=%v", w.String(), w.flushed)
	}
	if w.deadline.Sub(started) <= heartbeatEvery {
		t.Fatalf("write deadline %s does not outlive heartbeat %s", w.deadline.Sub(started), heartbeatEvery)
	}
	for _, private := range []string{"token", "latitude", "longitude", "user", "device", "tst"} {
		if strings.Contains(w.String(), private) {
			t.Fatalf("private field %q in payload", private)
		}
	}
}

func TestAdminSSEPublishesAndStops(t *testing.T) {
	hub := realtime.New()
	s := &Server{events: hub, shareByID: make(map[int64]int)}
	w := &sseTestWriter{header: make(http.Header), writes: make(chan struct{}, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	req := (&http.Request{}).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		s.apiEvents(w, req)
		close(done)
	}()
	waitWrite(t, w.writes)
	hub.Publish(store.Change{Topic: store.ChangeVerification, LocationID: 42, User: "private"})
	waitForBody(t, w, `"location_ids":[42]`)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("admin stream did not stop")
	}
	if admin, _ := s.eventCounts(); admin != 0 {
		t.Fatalf("admin connections=%d", admin)
	}
}

func TestAdminSSEHeadReturnsWithoutConnection(t *testing.T) {
	s := &Server{events: realtime.New(), shareByID: make(map[int64]int)}
	w := &sseTestWriter{header: make(http.Header)}
	s.apiEvents(w, (&http.Request{Method: http.MethodHead}).WithContext(context.Background()))
	if admin, share := s.eventCounts(); admin != 0 || share != 0 {
		t.Fatalf("event counts admin=%d share=%d", admin, share)
	}
	if !strings.Contains(w.String(), "retry: 3000") {
		t.Fatalf("body=%q", w.String())
	}
}

func TestAdminSSELimitAndHubShutdown(t *testing.T) {
	s := &Server{events: realtime.New(), adminEvents: maxAdminEvents, shareByID: make(map[int64]int)}
	rejected := &sseTestWriter{header: make(http.Header)}
	s.apiEvents(rejected, &http.Request{})
	if rejected.status != http.StatusServiceUnavailable || rejected.header.Get("Retry-After") != "10" {
		t.Fatalf("status=%d headers=%v", rejected.status, rejected.header)
	}

	s.adminEvents = 0
	s.events.Close()
	closed := &sseTestWriter{header: make(http.Header)}
	s.apiEvents(closed, (&http.Request{}).WithContext(context.Background()))
	if !strings.Contains(closed.String(), "retry: 3000") {
		t.Fatalf("body=%q", closed.String())
	}
}

func TestShareSSEPublishesPrivateTrackSignal(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	token := "share-event-test-token"
	if _, err := st.CreateShare(&store.ShareLink{Token: token, User: "alice", Device: "phone", Hours: 24}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	mgr := config.NewManager(cfg, st)
	hub := realtime.New()
	s := &Server{
		Cfg: mgr, Store: st, shareRL: shareLimiter(cfg), events: hub, shareByID: make(map[int64]int),
	}
	w := &sseTestWriter{header: make(http.Header), writes: make(chan struct{}, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/share/"+token+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetPathValue("token", token)
	done := make(chan struct{})
	go func() {
		s.handleShareEvents(w, req)
		close(done)
	}()
	waitWrite(t, w.writes)
	hub.Publish(store.Change{
		Topic: store.ChangeLocations, User: "bob", Device: "phone", Tst: time.Now().Add(-time.Minute).Unix(),
	})
	hub.Publish(store.Change{Topic: store.ChangeShares})
	hub.Publish(store.Change{
		Topic: store.ChangeLocations, LocationID: 42, User: "alice", Device: "phone",
		Tst: time.Now().Add(-time.Minute).Unix(),
	})
	waitForBody(t, w, "event: track\ndata: {}")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("share stream did not stop")
	}
	body := w.String()
	for _, private := range []string{token, "42", "alice", "phone"} {
		if strings.Contains(body, private) {
			t.Fatalf("private value %q in payload %q", private, body)
		}
	}
}

func TestSSEHeartbeatWireFormat(t *testing.T) {
	w := &sseTestWriter{header: make(http.Header)}
	if err := writeComment(w); err != nil {
		t.Fatal(err)
	}
	if w.String() != ": heartbeat\n\n" || !w.flushed {
		t.Fatalf("body=%q flushed=%v", w.String(), w.flushed)
	}
}

func TestSSEWriteFailures(t *testing.T) {
	want := errors.New("deadline failed")
	w := &sseTestWriter{header: make(http.Header), deadlineErr: want}
	if startSSE(w) {
		t.Fatal("stream started with a broken write deadline")
	}
	if err := writeComment(w); !errors.Is(err, want) {
		t.Fatalf("comment error=%v", err)
	}
	w.deadlineErr = nil
	if err := writeEvent(w, "change", make(chan int)); err == nil {
		t.Fatal("unencodable event accepted")
	}
}

func waitWrite(t *testing.T, writes <-chan struct{}) {
	t.Helper()
	select {
	case <-writes:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SSE write")
	}
}

func waitForBody(t *testing.T, w *sseTestWriter, text string) {
	t.Helper()
	deadline := time.After(time.Second)
	for !strings.Contains(w.String(), text) {
		select {
		case <-w.writes:
		case <-deadline:
			t.Fatalf("timed out waiting for %q in %q", text, w.String())
		}
	}
}

func TestSSEConnectionLimits(t *testing.T) {
	s := &Server{shareByID: make(map[int64]int)}
	for range maxAdminEvents {
		if !s.acquireAdminEvent() {
			t.Fatal("admin connection rejected below limit")
		}
	}
	if s.acquireAdminEvent() {
		t.Fatal("admin connection accepted above limit")
	}
	for range maxAdminEvents {
		s.releaseAdminEvent()
	}
	for range maxShareByID {
		if !s.acquireShareEvent(1) {
			t.Fatal("share connection rejected below per-link limit")
		}
	}
	if s.acquireShareEvent(1) {
		t.Fatal("share connection accepted above per-link limit")
	}
	for range maxShareByID {
		s.releaseShareEvent(1)
	}
	rec := &sseTestWriter{header: make(http.Header)}
	eventBusy(rec)
	if rec.status != http.StatusServiceUnavailable || rec.header.Get("Retry-After") != "10" {
		t.Fatalf("status=%d headers=%v", rec.status, rec.header)
	}
}

func TestShareScheduleFiltersAndDelays(t *testing.T) {
	now := time.Unix(1_800_000_030, 0)
	link := &store.ShareLink{User: "alice", Device: "phone", Hours: 24, DelayMinutes: 15}
	schedule := shareSchedule{due: make(map[int64]struct{})}
	batch := realtime.Batch{
		Topics: []string{store.ChangeLocations},
		Changes: []store.Change{
			{Topic: store.ChangeLocations, User: "bob", Device: "phone", Tst: now.Unix()},
			{Topic: store.ChangeLocations, User: "alice", Device: "watch", Tst: now.Unix()},
			{Topic: store.ChangeLocations, User: "alice", Device: "phone", Tst: now.Add(-25 * time.Hour).Unix()},
			{Topic: store.ChangeLocations, User: "alice", Device: "phone", Tst: now.Unix()},
		},
	}
	if schedule.add(batch, link, now) || len(schedule.due) != 1 {
		t.Fatalf("schedule=%+v", schedule)
	}
	due := time.Unix(ceilMinute(now.Add(15*time.Minute).Unix()), 0)
	if next, ok := schedule.next(); !ok || !next.Equal(due) {
		t.Fatalf("next=%v ok=%v", next, ok)
	}
	if schedule.ready(due.Add(-time.Second)) {
		t.Fatal("event released before privacy delay")
	}
	if !schedule.ready(due) || schedule.ready(due) {
		t.Fatal("event was not released exactly once")
	}
	if _, ok := schedule.next(); ok {
		t.Fatal("empty schedule still has a deadline")
	}
}

func TestShareScheduleUsesDelayedWindow(t *testing.T) {
	now := time.Unix(1_800_000_030, 0)
	link := &store.ShareLink{User: "alice", Device: "phone", Hours: 1, DelayMinutes: 120}
	schedule := shareSchedule{due: make(map[int64]struct{})}
	batch := realtime.Batch{
		Topics: []string{store.ChangeLocations},
		Changes: []store.Change{{
			Topic: store.ChangeLocations, User: "alice", Device: "phone",
			Tst: now.Add(-150 * time.Minute).Unix(),
		}},
	}
	if !schedule.add(batch, link, now) {
		t.Fatal("location in delayed window did not trigger an immediate refresh")
	}
	if len(schedule.due) != 0 {
		t.Fatalf("schedule=%+v", schedule)
	}

	schedule = shareSchedule{due: make(map[int64]struct{})}
	batch.Changes[0].Tst = now.Add(-190 * time.Minute).Unix()
	if schedule.add(batch, link, now) {
		t.Fatal("location outside delayed window triggered a refresh")
	}
}

func TestShareScheduleZeroDelayAndBulkUseMinuteBoundary(t *testing.T) {
	now := time.Unix(1_800_000_001, 0)
	link := &store.ShareLink{Hours: 24}
	for _, batch := range []realtime.Batch{
		{Topics: []string{store.ChangeLocations}, Changes: []store.Change{{Topic: store.ChangeLocations, Tst: now.Unix()}}},
		{Topics: []string{store.ChangeLocations}, All: true},
		{Topics: []string{store.ChangeLocations}, Changes: []store.Change{{Topic: store.ChangeLocations, Bulk: true}}},
	} {
		schedule := shareSchedule{due: make(map[int64]struct{})}
		if schedule.add(batch, link, now) {
			t.Fatal("event bypassed minute coalescing")
		}
		if schedule.ready(time.Unix(ceilMinute(now.Unix())-1, 0)) ||
			!schedule.ready(time.Unix(ceilMinute(now.Unix()), 0)) {
			t.Fatalf("schedule=%+v", schedule)
		}
	}
}

func TestShareScheduleCapacityPreservesDueTimes(t *testing.T) {
	now := time.Unix(1_800_000_001, 0)
	schedule := shareSchedule{due: make(map[int64]struct{})}
	first := ceilMinute(now.Add(15 * time.Minute).Unix())
	for i := range int64(2049) {
		schedule.schedule(first + i*60)
	}
	if len(schedule.due) != 2048 || schedule.ready(now.Add(time.Minute)) {
		t.Fatal("capacity changed publication times")
	}
	if next, ok := schedule.next(); !ok || next.Unix() != first {
		t.Fatalf("next=%v ok=%v", next, ok)
	}
	if !schedule.ready(time.Unix(first, 0)) || schedule.ready(time.Unix(first, 0)) {
		t.Fatal("due bucket was not released exactly once")
	}
}
