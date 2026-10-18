package ratelimit

import (
	"container/heap"
	"container/list"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

type Limiter struct {
	limit  int
	window time.Duration
	maxIPs int
	now    func() time.Time
	mu     sync.Mutex
	hits   map[string]*entry
	lru    list.List
	expiry expiryHeap
}

type entry struct {
	n       int
	reset   time.Time
	lru     *list.Element
	expires *expiryItem
}

type expiryItem struct {
	key   string
	entry *entry
	when  time.Time
	index int
}

type expiryHeap []*expiryItem

func (h expiryHeap) Len() int { return len(h) }

func (h expiryHeap) Less(i, j int) bool {
	return h[i].when.Before(h[j].when)
}

func (h expiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *expiryHeap) Push(x any) {
	item := x.(*expiryItem)
	item.index = len(*h)
	*h = append(*h, item)
}

func (h *expiryHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*h = old[:n-1]
	return item
}

const gcBudget = 16

func New(limit int, window time.Duration) *Limiter {
	return &Limiter{
		limit:  limit,
		window: window,
		now:    time.Now,
		hits:   make(map[string]*entry),
	}
}

func (l *Limiter) SetMaxIPs(n int) {
	l.mu.Lock()
	l.maxIPs = n
	l.trimToMaxLocked()
	l.mu.Unlock()
}

func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if key == "" || l.limit <= 0 || l.window <= 0 {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.gc(now)
	e := l.hits[key]
	reset := false
	if e == nil {
		e = &entry{}
		l.hits[key] = e
		e.lru = l.lru.PushBack(key)
		e.expires = &expiryItem{key: key, entry: e}
		heap.Push(&l.expiry, e.expires)
		reset = true
	} else if now.After(e.reset) {
		reset = true
	}
	if reset {
		updated := entry{reset: now.Add(l.window)}
		updated.lru = e.lru
		updated.expires = e.expires
		*e = updated
		e.expires.when = e.reset.Add(l.window)
		heap.Fix(&l.expiry, e.expires.index)
	}
	e.n++
	l.lru.MoveToBack(e.lru)
	l.trimToMaxLocked()
	if e.n <= l.limit {
		return true, 0
	}
	return false, e.reset.Sub(now)
}

func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	if e := l.hits[key]; e != nil {
		l.removeLocked(key, e)
	}
	l.mu.Unlock()
}

func (l *Limiter) gc(now time.Time) {
	for i := 0; i < gcBudget && l.expiry.Len() > 0; i++ {
		item := l.expiry[0]
		if !now.After(item.when) {
			return
		}
		heap.Pop(&l.expiry)
		if current := l.hits[item.key]; current == item.entry {
			delete(l.hits, item.key)
			l.lru.Remove(current.lru)
		}
	}
}

func (l *Limiter) trimToMaxLocked() {
	for l.maxIPs > 0 && len(l.hits) > l.maxIPs {
		oldest := l.lru.Front()
		if oldest == nil {
			return
		}
		key := oldest.Value.(string)
		if e := l.hits[key]; e != nil {
			l.removeLocked(key, e)
		} else {
			l.lru.Remove(oldest)
		}
	}
}

func (l *Limiter) removeLocked(key string, e *entry) {
	delete(l.hits, key)
	l.lru.Remove(e.lru)
	if e.expires.index >= 0 {
		heap.Remove(&l.expiry, e.expires.index)
	}
}

func ClientKey(r *http.Request, trusted []string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && IsTrustedProxy(ip, trusted) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			for _, part := range slices.Backward(parts) {
				forwarded := net.ParseIP(strings.TrimSpace(part))
				if forwarded == nil {
					continue
				}
				if !IsTrustedProxy(forwarded, trusted) {
					return forwarded.String()
				}
			}
			for _, raw := range parts {
				if forwarded := net.ParseIP(strings.TrimSpace(raw)); forwarded != nil {
					return forwarded.String()
				}
			}
		}
	}
	if ip != nil {
		return ip.String()
	}
	return host
}

func IngestKey(user, device string) string {
	if user == "" {
		user = "owntracks"
	}
	if device == "" {
		device = "phone"
	}
	return "ingest:" + user + "/" + device
}

func IsTrustedProxy(ip net.IP, trusted []string) bool {
	if ip.IsLoopback() {
		return true
	}
	for _, raw := range trusted {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(raw); err == nil {
			if n.Contains(ip) {
				return true
			}
			continue
		}
		if t := net.ParseIP(raw); t != nil && t.Equal(ip) {
			return true
		}
	}
	return false
}
