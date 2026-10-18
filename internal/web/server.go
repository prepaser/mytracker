package web

import (
	"net/http"
	"sync"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/ratelimit"
	"github.com/prepaser/mytracker/internal/realtime"
	"github.com/prepaser/mytracker/internal/store"
)

type Server struct {
	Cfg            ConfigManager
	Store          *store.Store
	Stamp          StampService
	Verification   VerificationService
	Maintenance    MaintenanceService
	mux            *http.ServeMux
	loginRL        *ratelimit.Limiter
	shareRL        *ratelimit.Limiter
	loginMu        sync.RWMutex
	shareMu        sync.RWMutex
	loginSem       chan struct{}
	events         *realtime.Hub
	Timeline       TimelineService
	Geofence       GeofenceService
	eventMu        sync.Mutex
	adminEvents    int
	shareEvents    int
	shareByID      map[int64]int
	shareQueryMu   sync.Mutex
	shareQueries   int
	shareQueryByID map[int64]int
	backupMu       sync.Mutex
	backupBusy     chan struct{}
	backupCheck    backupCheck
	deleteMu       sync.Mutex
	exportSem      chan struct{}
}

type backupCheck struct {
	path      string
	size      int64
	mtimeNano int64
	state     string
	checkedAt time.Time
}

func New(deps Dependencies) *Server {
	snap := deps.Config.Snapshot()
	verification := deps.Verification
	if verification == nil {
		verification, _ = deps.Stamp.(VerificationService)
	}
	s := &Server{
		Cfg:            deps.Config,
		Store:          deps.Store,
		Stamp:          deps.Stamp,
		Verification:   verification,
		Maintenance:    deps.Maintenance,
		mux:            http.NewServeMux(),
		loginRL:        newLoginRL(snap),
		shareRL:        shareLimiter(snap),
		loginSem:       make(chan struct{}, 2),
		events:         deps.Events,
		Timeline:       deps.Timeline,
		Geofence:       deps.Geofence,
		shareByID:      make(map[int64]int),
		shareQueryByID: make(map[int64]int),
		exportSem:      make(chan struct{}, 1),
	}
	if s.events == nil {
		s.events = realtime.New()
	}
	if err := s.routes(); err != nil {
		panic(err)
	}
	return s
}

func shareLimiter(cfg *config.Config) *ratelimit.Limiter {
	window, err := cfg.ShareRateDuration()
	if err != nil {
		window = time.Minute
	}
	rl := ratelimit.New(cfg.ShareRateLimit, window)
	rl.SetMaxIPs(cfg.RateLimitMaxIPs)
	return rl
}

func newLoginRL(cfg *config.Config) *ratelimit.Limiter {
	window, err := cfg.LoginRateDuration()
	if err != nil {
		window = 5 * time.Minute
	}
	rl := ratelimit.New(cfg.LoginRateLimit, window)
	rl.SetMaxIPs(cfg.RateLimitMaxIPs)
	return rl
}
