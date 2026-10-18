package owntracks

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/metrics"
	"github.com/prepaser/mytracker/internal/ratelimit"
	"github.com/prepaser/mytracker/internal/store"
)

type Handler struct {
	Store *store.Store
	Stamp Stamper
	cfg   *config.Manager
	rl    *ratelimit.Limiter
	rlMu  sync.RWMutex
}

type Stamper interface {
	Process(id int64, digest [32]byte)
}

func New(st *store.Store, stampSvc Stamper, cfg *config.Manager) *Handler {
	h := &Handler{Store: st, Stamp: stampSvc, cfg: cfg, rl: pubLimiter(cfg.Snapshot())}
	h.rl.SetMaxIPs(cfg.Snapshot().RateLimitMaxIPs)
	return h
}

func pubLimiter(cfg *config.Config) *ratelimit.Limiter {
	window, err := cfg.PubRateDuration()
	if err != nil {
		window = time.Minute
	}
	rl := ratelimit.New(cfg.PubRateLimit, window)
	rl.SetMaxIPs(cfg.RateLimitMaxIPs)
	return rl
}

func (h *Handler) ReloadLimits(cfg *config.Config) {
	h.rlMu.Lock()
	defer h.rlMu.Unlock()
	h.rl = pubLimiter(cfg)
}

func (h *Handler) creds() (user, pass string) {
	cfg := h.cfg.Snapshot()
	return cfg.PubUser, cfg.PubPass
}

func (h *Handler) AllowIngest(user, device string) bool {
	h.rlMu.RLock()
	rl := h.rl
	h.rlMu.RUnlock()
	user, device = normalizeIdentity(user, device)
	ok, _ := rl.Allow(ratelimit.IngestKey(user, device))
	return ok
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.rlMu.RLock()
	rl := h.rl
	h.rlMu.RUnlock()
	if ok, retry := rl.Allow(ratelimit.ClientKey(r, h.cfg.Snapshot().TrustedProxies)); !ok {
		metrics.Ingest("http", "rate_limited")
		if retry < time.Second {
			retry = time.Second
		}
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	user, pass := h.creds()
	u, p, ok := r.BasicAuth()
	if user == "" || pass == "" || !ok || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
		metrics.Ingest("http", "unauthorized")
		w.Header().Set("WWW-Authenticate", `Basic realm="mytracker"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxPayloadBytes+1))
	if len(body) > MaxPayloadBytes {
		metrics.Ingest("http", "invalid")
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		metrics.Ingest("http", "invalid")
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		metrics.Ingest("http", "invalid")
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	identUser, device := h.ident(r, raw)
	id, err := ingestDecoded(h.Store, h.Stamp, identUser, device, raw, h.cfg.Snapshot().StampRetries)
	if err != nil {
		if errors.Is(err, ErrInvalidPayload) {
			metrics.Ingest("http", "invalid")
			http.Error(w, "bad location", http.StatusBadRequest)
			return
		}
		metrics.Ingest("http", "error")
		slog.Error("ingest failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if id > 0 {
		metrics.IncLocation()
		metrics.Ingest("http", "stored")
	} else {
		metrics.Ingest("http", "ignored")
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("[]"))
}

func (h *Handler) ident(r *http.Request, raw map[string]any) (user, device string) {
	user = r.URL.Query().Get("u")
	device = r.URL.Query().Get("d")
	if user == "" {
		user = r.Header.Get("X-Limit-U")
	}
	if device == "" {
		device = r.Header.Get("X-Limit-D")
	}
	return identFromTopic(raw, user, device)
}
