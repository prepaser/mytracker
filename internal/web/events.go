package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/prepaser/mytracker/internal/metrics"
	"github.com/prepaser/mytracker/internal/realtime"
	"github.com/prepaser/mytracker/internal/store"
)

const (
	maxAdminEvents  = 16
	maxShareEvents  = 64
	maxShareByID    = 5
	heartbeatEvery  = 20 * time.Second
	revalidateEvery = 60 * time.Second
	sseWriteTimeout = 2 * heartbeatEvery
)

type adminEvent struct {
	Topics      []string `json:"topics"`
	LocationIDs []int64  `json:"location_ids,omitempty"`
	All         bool     `json:"all"`
}

type shareSchedule struct {
	due map[int64]struct{}
}

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		_ = startSSE(w)
		return
	}
	if !s.acquireAdminEvent() {
		metrics.SSEEvent("admin", "connection", "rejected")
		eventBusy(w)
		return
	}
	defer s.releaseAdminEvent()
	started := time.Now()
	metrics.SSEConnection("admin", 1)
	defer func() {
		metrics.SSEConnection("admin", -1)
		metrics.SSEDuration("admin", time.Since(started))
	}()
	sub := s.events.Subscribe()
	defer sub.Close()
	if !startSSE(w) {
		return
	}
	heartbeat := time.NewTicker(heartbeatEvery)
	defer heartbeat.Stop()
	validate := time.NewTicker(revalidateEvery)
	defer validate.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.Done():
			return
		case <-sub.Signal():
			batch := sub.Drain()
			if batch.Coalesced {
				metrics.SSEEvent("admin", "change", "coalesced")
			}
			if err := writeEvent(w, "change", adminEvent{
				Topics: batch.Topics, LocationIDs: batch.LocationIDs, All: batch.All,
			}); err != nil {
				metrics.SSEEvent("admin", "change", "error")
				return
			}
			metrics.SSEEvent("admin", "change", "sent")
		case <-heartbeat.C:
			if err := writeComment(w); err != nil {
				return
			}
		case <-validate.C:
			if !s.currentSessionValid(r) {
				return
			}
		}
	}
}

func (s *Server) handleShareEvents(w http.ResponseWriter, r *http.Request) {
	s.withShare(w, r, func(link *store.ShareLink) {
		if r.Method == http.MethodHead {
			_ = startSSE(w)
			return
		}
		if !s.acquireShareEvent(link.ID) {
			metrics.SSEEvent("share", "connection", "rejected")
			eventBusy(w)
			return
		}
		defer s.releaseShareEvent(link.ID)
		started := time.Now()
		metrics.SSEConnection("share", 1)
		defer func() {
			metrics.SSEConnection("share", -1)
			metrics.SSEDuration("share", time.Since(started))
		}()
		sub := s.events.Subscribe()
		defer sub.Close()
		if !startSSE(w) {
			return
		}
		heartbeat := time.NewTicker(heartbeatEvery)
		defer heartbeat.Stop()
		validate := time.NewTicker(revalidateEvery)
		defer validate.Stop()
		schedule := shareSchedule{due: make(map[int64]struct{})}
		dueTimer := time.NewTimer(time.Hour)
		dueTimer.Stop()
		defer dueTimer.Stop()
		var dueC <-chan time.Time
		armDue := func(now time.Time) {
			due, ok := schedule.next()
			if !ok {
				dueTimer.Stop()
				dueC = nil
				return
			}
			dueTimer.Stop()
			var wait time.Duration
			if now.Before(due) {
				wait = due.Sub(now)
			} else {
				wait = 0
			}
			dueTimer.Reset(wait)
			dueC = dueTimer.C
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-sub.Done():
				return
			case <-sub.Signal():
				batch := sub.Drain()
				if batch.Coalesced {
					metrics.SSEEvent("share", "locations", "coalesced")
				}
				if containsTopic(batch.Topics, store.ChangeShares) {
					current, err := s.loadShare(r.PathValue("token"))
					if err != nil || current.ID != link.ID {
						return
					}
				}
				now := time.Now()
				immediate := schedule.add(batch, link, now)
				armDue(now)
				if immediate {
					if err := writeEvent(w, "track", struct{}{}); err != nil {
						metrics.SSEEvent("share", "locations", "error")
						return
					}
					metrics.SSEEvent("share", "locations", "sent")
				}
			case <-heartbeat.C:
				if err := writeComment(w); err != nil {
					return
				}
			case now := <-dueC:
				if schedule.ready(now) {
					if err := writeEvent(w, "track", struct{}{}); err != nil {
						metrics.SSEEvent("share", "locations", "error")
						return
					}
					metrics.SSEEvent("share", "locations", "sent")
				}
				armDue(now)
			case now := <-validate.C:
				current, err := s.loadShare(r.PathValue("token"))
				if err != nil || current.ID != link.ID {
					return
				}
				if schedule.ready(now) {
					if err := writeEvent(w, "track", struct{}{}); err != nil {
						metrics.SSEEvent("share", "locations", "error")
						return
					}
					metrics.SSEEvent("share", "locations", "sent")
				}
				armDue(now)
			}
		}
	})
}

func (q *shareSchedule) add(batch realtime.Batch, link *store.ShareLink, now time.Time) bool {
	if !containsTopic(batch.Topics, store.ChangeLocations) {
		return false
	}
	if batch.All {
		q.schedule(ceilMinute(now.Add(time.Duration(link.DelayMinutes) * time.Minute).Unix()))
		return false
	}
	immediate := false
	for _, change := range batch.Changes {
		if change.Bulk {
			q.schedule(ceilMinute(now.Add(time.Duration(link.DelayMinutes) * time.Minute).Unix()))
			continue
		}
		if link.User != "" && link.User != change.User || link.Device != "" && link.Device != change.Device {
			continue
		}
		if link.Hours > 0 {
			from, _ := shareTrackWindow(now, link)
			if change.Tst < from {
				continue
			}
		}
		due := ceilMinute(change.Tst + int64(link.DelayMinutes*60))
		if due <= now.Unix() {
			immediate = true
			continue
		}
		q.schedule(due)
	}
	return immediate
}

func (q *shareSchedule) schedule(due int64) {
	if _, ok := q.due[due]; ok {
		return
	}
	if len(q.due) < 2048 {
		q.due[due] = struct{}{}
	}
}

func (q *shareSchedule) ready(now time.Time) bool {
	ready := false
	for due := range q.due {
		if due <= now.Unix() {
			delete(q.due, due)
			ready = true
		}
	}
	return ready
}

func (q *shareSchedule) next() (time.Time, bool) {
	var next int64
	for due := range q.due {
		if next == 0 || due < next {
			next = due
		}
	}
	if next == 0 {
		return time.Time{}, false
	}
	return time.Unix(next, 0), true
}

func ceilMinute(unix int64) int64 { return ((unix + 59) / 60) * 60 }

func containsTopic(topics []string, target string) bool {
	i := sort.SearchStrings(topics, target)
	return i < len(topics) && topics[i] == target
}

func startSSE(w http.ResponseWriter) bool {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	if err := setWriteDeadline(w); err != nil {
		return false
	}
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return false
	}
	return flushSSE(w) == nil
}

func writeEvent(w http.ResponseWriter, event string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := setWriteDeadline(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	return flushSSE(w)
}

func writeComment(w http.ResponseWriter) error {
	if err := setWriteDeadline(w); err != nil {
		return err
	}
	if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
		return err
	}
	return flushSSE(w)
}

func setWriteDeadline(w http.ResponseWriter) error {
	return http.NewResponseController(w).SetWriteDeadline(time.Now().Add(sseWriteTimeout))
}

func flushSSE(w http.ResponseWriter) error { return http.NewResponseController(w).Flush() }

func eventBusy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "10")
	jsonErr(w, fmt.Errorf("event connection limit reached"), http.StatusServiceUnavailable)
}

func (s *Server) acquireAdminEvent() bool {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.adminEvents >= maxAdminEvents {
		return false
	}
	s.adminEvents++
	return true
}

func (s *Server) releaseAdminEvent() {
	s.eventMu.Lock()
	s.adminEvents--
	s.eventMu.Unlock()
}

func (s *Server) acquireShareEvent(id int64) bool {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.shareEvents >= maxShareEvents || s.shareByID[id] >= maxShareByID {
		return false
	}
	s.shareEvents++
	s.shareByID[id]++
	return true
}

func (s *Server) releaseShareEvent(id int64) {
	s.eventMu.Lock()
	s.shareEvents--
	s.shareByID[id]--
	if s.shareByID[id] == 0 {
		delete(s.shareByID, id)
	}
	s.eventMu.Unlock()
}

func (s *Server) eventCounts() (int, int) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	return s.adminEvents, s.shareEvents
}
