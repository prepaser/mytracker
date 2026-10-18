package realtime

import (
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/prepaser/mytracker/internal/store"
)

const (
	maxLocationIDs = 100
	maxChanges     = 2048
)

type Batch struct {
	Topics      []string
	LocationIDs []int64
	Changes     []store.Change
	All         bool
	Coalesced   bool
}

type Hub struct {
	mu          sync.RWMutex
	subscribers map[*Subscription]struct{}
	closed      bool
	published   atomic.Uint64
	coalesced   atomic.Uint64
}

type Stats struct {
	Subscribers int    `json:"subscribers"`
	Published   uint64 `json:"published"`
	Coalesced   uint64 `json:"coalesced"`
	Closed      bool   `json:"closed"`
}

type Subscription struct {
	hub     *Hub
	mu      sync.Mutex
	topics  map[string]struct{}
	ids     map[int64]struct{}
	changes []store.Change
	all     bool
	merged  bool
	signal  chan struct{}
	done    chan struct{}
	closed  bool
}

func New() *Hub {
	return &Hub{subscribers: make(map[*Subscription]struct{})}
}

func (h *Hub) Subscribe() *Subscription {
	s := &Subscription{
		hub: h, topics: make(map[string]struct{}), ids: make(map[int64]struct{}),
		signal: make(chan struct{}, 1), done: make(chan struct{}),
	}
	h.mu.Lock()
	if h.closed {
		s.closed = true
		close(s.done)
	} else {
		h.subscribers[s] = struct{}{}
	}
	h.mu.Unlock()
	return s
}

func (h *Hub) Publish(change store.Change) {
	h.published.Add(1)
	h.mu.RLock()
	subs := make([]*Subscription, 0, len(h.subscribers))
	for sub := range h.subscribers {
		subs = append(subs, sub)
	}
	h.mu.RUnlock()
	for _, sub := range subs {
		if sub.push(change) {
			h.coalesced.Add(1)
		}
	}
}

func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	subs := make([]*Subscription, 0, len(h.subscribers))
	for sub := range h.subscribers {
		subs = append(subs, sub)
	}
	h.subscribers = nil
	h.mu.Unlock()
	for _, sub := range subs {
		sub.close(false)
	}
}

func (h *Hub) Stats() Stats {
	h.mu.RLock()
	n := len(h.subscribers)
	closed := h.closed
	h.mu.RUnlock()
	return Stats{Subscribers: n, Published: h.published.Load(), Coalesced: h.coalesced.Load(), Closed: closed}
}

func (s *Subscription) Signal() <-chan struct{} { return s.signal }
func (s *Subscription) Done() <-chan struct{}   { return s.done }

func (s *Subscription) Drain() Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := Batch{All: s.all, Coalesced: s.merged, Changes: append([]store.Change(nil), s.changes...)}
	for topic := range s.topics {
		batch.Topics = append(batch.Topics, topic)
	}
	for id := range s.ids {
		batch.LocationIDs = append(batch.LocationIDs, id)
	}
	sort.Strings(batch.Topics)
	slices.Sort(batch.LocationIDs)
	clear(s.topics)
	clear(s.ids)
	s.changes = nil
	s.all = false
	s.merged = false
	return batch
}

func (s *Subscription) Close() { s.close(true) }

func (s *Subscription) push(change store.Change) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	_, merged := s.topics[change.Topic]
	s.topics[change.Topic] = struct{}{}
	if change.LocationID > 0 && !s.all {
		if len(s.ids) < maxLocationIDs {
			s.ids[change.LocationID] = struct{}{}
		} else {
			s.ids = make(map[int64]struct{})
			s.changes = nil
			s.all = true
		}
	}
	if change.Topic == store.ChangeLocations && !s.all {
		if len(s.changes) < maxChanges {
			s.changes = append(s.changes, change)
		} else {
			s.changes = nil
			s.all = true
		}
	}
	select {
	case s.signal <- struct{}{}:
	default:
		merged = true
	}
	s.merged = s.merged || merged
	return merged
}

func (s *Subscription) close(remove bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	s.mu.Unlock()
	if remove && s.hub != nil {
		s.hub.mu.Lock()
		delete(s.hub.subscribers, s)
		s.hub.mu.Unlock()
	}
}
