package web

import (
	"context"
	"net/http"
	"time"

	"github.com/prepaser/mytracker/internal/geojson"
	"github.com/prepaser/mytracker/internal/metrics"
	"github.com/prepaser/mytracker/internal/store"
	trackview "github.com/prepaser/mytracker/internal/track"
)

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	cfg := s.Cfg.Snapshot()
	st, _ := s.Store.Stats()
	_, backupAt, _ := store.LatestBackup(cfg.BackupsDir())
	backupText := "never"
	if !backupAt.IsZero() {
		backupText = backupAt.UTC().Format(time.RFC3339)
	}
	page(w, "dashboard.html", map[string]any{
		"Page":      "dashboard",
		"Stats":     st,
		"WorkersOK": s.Stamp.Healthy(),
		"BackupAt":  backupText,
		"CSRF":      s.csrfToken(r),
	})
}

func (s *Server) handleMap(w http.ResponseWriter, r *http.Request) {
	cfg := s.Cfg.Snapshot()
	ids, _ := s.Store.ListIdentities()
	data := map[string]any{
		"Page":       "map",
		"Identities": ids,
		"TileURL":    cfg.TileURL,
		"TileAttr":   cfg.TileAttribution,
		"CSRF":       s.csrfToken(r),
	}
	if last, _ := s.Store.LastLocation(); last != nil {
		data["LastLat"] = last.Lat
		data["LastLon"] = last.Lon
	}
	page(w, "map.html", data)
}

func (s *Server) apiTrack(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	from, to, err := parseLocationRange(r.URL.Query(), started, true)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	limit, err := parseTrackLimit(r.URL.Query().Get("limit"))
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	f := store.TrackFilter{
		User:   r.URL.Query().Get("user"),
		Device: r.URL.Query().Get("device"),
		From:   from,
		To:     to,
		Limit:  limit,
	}
	result, err := s.sampleTrackContext(r.Context(), f, 0)
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	fc := geojson.FromLocations(result.Locations)
	fc.Metadata = result.Metadata
	metrics.ObserveData("track", result.Metadata.Sampled, time.Since(started), len(result.Locations))
	jsonOK(w, fc)
}

func (s *Server) sampleTrackContext(ctx context.Context, f store.TrackFilter, precision int) (trackview.Result, error) {
	snapshot, err := s.Store.BeginTrackSnapshotContext(ctx, f)
	if err != nil {
		return trackview.Result{}, err
	}
	defer func() { _ = snapshot.Close() }()
	project := trackview.WebMercator
	if precision > 0 {
		project = func(p store.TrackPoint) (float64, float64) {
			p.Lat, p.Lon = geojson.Quantize(p.Lat, p.Lon, precision)
			return trackview.WebMercator(p)
		}
	}
	return trackview.Sample(snapshot, project)
}

func (s *Server) apiOwnTracksURL(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]string{"url": s.Cfg.Snapshot().PubURLForRequest(r.Host, s.secureCookie(r))})
}

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.Stats()
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	jsonOK(w, st)
}

func (s *Server) apiIdentities(w http.ResponseWriter, r *http.Request) {
	ids, err := s.Store.ListIdentities()
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if ids == nil {
		ids = []struct{ User, Device string }{}
	}
	jsonOK(w, map[string]any{"data": ids})
}

func (s *Server) apiOTSUpgrade(w http.ResponseWriter, r *http.Request) {
	job, coalesced, err := s.Maintenance.Enqueue("ots_upgrade", "manual")
	if err != nil {
		jsonErr(w, err, http.StatusServiceUnavailable)
		return
	}
	jsonOKStatus(w, map[string]any{
		"data": map[string]any{"job": job, "coalesced": coalesced},
	}, http.StatusAccepted)
}
