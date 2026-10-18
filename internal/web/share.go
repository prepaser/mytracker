package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prepaser/mytracker/internal/geojson"
	"github.com/prepaser/mytracker/internal/metrics"
	"github.com/prepaser/mytracker/internal/store"
)

type shareView struct {
	ID        int64  `json:"id"`
	Label     string `json:"label"`
	Filter    string `json:"filter"`
	Hours     int    `json:"hours"`
	Delay     int    `json:"delay_minutes"`
	Precision int    `json:"precision_m"`
	Expires   string `json:"expires"`
	Active    bool   `json:"active"`
	Created   string `json:"created,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
}

func shareViews(links []store.ShareLink) []shareView {
	out := make([]shareView, 0, len(links))
	for _, link := range links {
		lastSeen := ""
		if link.LastAccessAt.Valid {
			lastSeen = time.Unix(link.LastAccessAt.Int64, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, shareView{
			ID:        link.ID,
			Label:     link.Label,
			Filter:    link.FilterDesc(),
			Hours:     link.Hours,
			Delay:     link.DelayMinutes,
			Precision: link.PrecisionM,
			Expires:   store.FormatShareExpiry(link.ExpiresAt),
			Active:    link.Active(),
			Created:   time.Unix(link.CreatedAt, 0).UTC().Format(time.RFC3339),
			LastSeen:  lastSeen,
		})
	}
	return out
}

func (s *Server) withShare(w http.ResponseWriter, r *http.Request, fn func(*store.ShareLink)) {
	s.shareMu.RLock()
	rl := s.shareRL
	s.shareMu.RUnlock()
	if ok, retry := rl.Allow(s.clientKey(r)); !ok {
		if strings.Contains(r.URL.Path, "/api/") {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retry.Seconds()))))
			jsonErr(w, fmt.Errorf("too many requests"), http.StatusTooManyRequests)
		} else {
			rateLimited(w, retry)
		}
		return
	}
	link, err := s.loadShare(r.PathValue("token"))
	if err != nil {
		if errors.Is(err, store.ErrShareNotFound) || errors.Is(err, store.ErrShareExpired) {
			if strings.Contains(r.URL.Path, "/api/") {
				jsonErr(w, fmt.Errorf("share not found"), http.StatusNotFound)
			} else {
				http.NotFound(w, r)
			}
			return
		}
		if strings.Contains(r.URL.Path, "/api/") {
			jsonErr(w, err, http.StatusServiceUnavailable)
		} else {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	_ = s.Store.TouchShare(link.ID)
	fn(link)
}

func (s *Server) handleSharePage(w http.ResponseWriter, r *http.Request) {
	s.withShare(w, r, func(link *store.ShareLink) {
		title := link.Label
		if title == "" {
			title = "Location share"
		}
		data := map[string]any{
			"Title":    title,
			"TileURL":  s.Cfg.Snapshot().TileURL,
			"TileAttr": s.Cfg.Snapshot().TileAttribution,
		}
		page(w, "share.html", data)
	})
}

func (s *Server) handleShareTrack(w http.ResponseWriter, r *http.Request) {
	s.withShare(w, r, func(link *store.ShareLink) {
		ctx, done, ok := s.beginShareQuery(w, r, link.ID)
		if !ok {
			return
		}
		defer done()
		started := time.Now()
		from, to := shareTrackWindow(started, link)
		result, err := s.sampleTrackContext(ctx, store.TrackFilter{
			User:   link.User,
			Device: link.Device,
			From:   from,
			To:     to,
			Limit:  10000,
		}, link.PrecisionM)
		if err != nil {
			jsonErr(w, err, 500)
			return
		}
		fc := geojson.FromShareLocations(result.Locations, link.PrecisionM)
		fc.Metadata = result.Metadata
		metrics.ObserveData("track", result.Metadata.Sampled, time.Since(started), len(result.Locations))
		jsonOK(w, fc)
	})
}

func shareTrackWindow(now time.Time, link *store.ShareLink) (from, to int64) {
	delay := time.Duration(link.DelayMinutes) * time.Minute
	return now.Add(-delay - time.Duration(link.Hours)*time.Hour).Unix(), now.Add(-delay).Unix()
}

func (s *Server) apiSharesList(w http.ResponseWriter, r *http.Request) {
	links, err := s.Store.ListShares()
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	jsonOK(w, shareViews(links))
}

func (s *Server) apiShareCreate(w http.ResponseWriter, r *http.Request) {
	link, risky, confirmed, err := parseShareRequest(r)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if risky && !confirmed {
		jsonErr(w, fmt.Errorf("confirm_risk is required for exact or non-expiring shares"), http.StatusBadRequest)
		return
	}
	id, err := s.Store.CreateShare(link)
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	cfg := s.Cfg.Snapshot()
	slog.Info("security share created", "share_id", id)
	jsonOK(w, map[string]any{
		"id":  id,
		"url": cfg.ShareURLForRequest(link.Token, r.Host, s.secureCookie(r)),
	})
}

type shareJSONRequest struct {
	Label        string `json:"label"`
	User         string `json:"user"`
	Device       string `json:"device"`
	Hours        *int   `json:"hours"`
	ExpiresDays  *int   `json:"expires_days"`
	DelayMinutes *int   `json:"delay_minutes"`
	PrecisionM   *int   `json:"precision_m"`
	ConfirmRisk  bool   `json:"confirm_risk"`
}

func parseShareRequest(r *http.Request) (*store.ShareLink, bool, bool, error) {
	if !mediaTypeIs(r.Header.Get("Content-Type"), "application/json") {
		link, risky, err := parseShareForm(r)
		return link, risky, r.FormValue("confirm_risk") == "1", err
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var input shareJSONRequest
	if err := dec.Decode(&input); err != nil {
		return nil, false, false, fmt.Errorf("invalid JSON share request")
	}
	if err := ensureJSONEnd(dec); err != nil {
		return nil, false, false, err
	}
	hours := intValue(input.Hours, 24)
	expires := intValue(input.ExpiresDays, 7)
	delay := intValue(input.DelayMinutes, 15)
	precision := intValue(input.PrecisionM, 100)
	if err := validateShareValues(hours, expires, delay, precision); err != nil {
		return nil, false, false, err
	}
	label, user, device := strings.TrimSpace(input.Label), strings.TrimSpace(input.User), strings.TrimSpace(input.Device)
	if err := validateShareText(label, user, device); err != nil {
		return nil, false, false, err
	}
	return &store.ShareLink{
		Label:        label,
		User:         user,
		Device:       device,
		Hours:        hours,
		ExpiresAt:    store.ShareExpiresAt(expires),
		DelayMinutes: delay,
		PrecisionM:   precision,
	}, expires == 0 || precision == 0, input.ConfirmRisk, nil
}

func ensureJSONEnd(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("invalid JSON share request")
	}
	return nil
}

func intValue(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func validateShareValues(hours, expires, delay, precision int) error {
	for _, value := range []struct {
		name     string
		value    int
		min, max int
	}{
		{"hours", hours, 1, 24 * 3650},
		{"expires_days", expires, 0, 3650},
		{"delay_minutes", delay, 0, 1440},
		{"precision_m", precision, 0, 1000},
	} {
		if value.value < value.min || value.value > value.max {
			return fmt.Errorf("%s must be between %d and %d", value.name, value.min, value.max)
		}
	}
	if precision != 0 && precision != 100 && precision != 500 && precision != 1000 {
		return fmt.Errorf("precision_m is invalid")
	}
	return nil
}

func validateShareText(label, user, device string) error {
	for _, value := range []struct {
		name  string
		value string
		max   int
	}{{"label", label, 80}, {"user", user, 128}, {"device", device, 128}} {
		if len(value.value) > value.max || !utf8.ValidString(value.value) {
			return fmt.Errorf("%s must be at most %d bytes", value.name, value.max)
		}
		for _, r := range value.value {
			if unicode.IsControl(r) {
				return fmt.Errorf("%s must not contain control characters", value.name)
			}
		}
	}
	return nil
}

func (s *Server) apiShareRotate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonErr(w, fmt.Errorf("invalid share id"), http.StatusBadRequest)
		return
	}
	token, err := s.Store.RotateShare(id)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, store.ErrShareNotFound) {
			code = http.StatusNotFound
		} else if errors.Is(err, store.ErrShareExpired) {
			code = http.StatusConflict
		}
		jsonErr(w, err, code)
		return
	}
	slog.Info("security share rotated", "share_id", id)
	jsonOK(w, map[string]any{"id": id, "url": s.Cfg.Snapshot().ShareURLForRequest(token, r.Host, s.secureCookie(r))})
}

func (s *Server) apiShareDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonErr(w, err, 400)
		return
	}
	if err := s.Store.DeleteShare(id); err != nil {
		if errors.Is(err, store.ErrShareNotFound) {
			jsonErr(w, err, 404)
			return
		}
		jsonErr(w, err, 500)
		return
	}
	slog.Info("security share revoked", "share_id", id)
	jsonOK(w, map[string]bool{"ok": true})
}

func parseShareForm(r *http.Request) (*store.ShareLink, bool, error) {
	hours, err := shareInt(r, "hours", 24, 1, 24*3650)
	if err != nil {
		return nil, false, err
	}
	expDays, err := shareInt(r, "expires_days", 7, 0, 3650)
	if err != nil {
		return nil, false, err
	}
	delay, err := shareInt(r, "delay_minutes", 15, 0, 1440)
	if err != nil {
		return nil, false, err
	}
	precision, err := shareInt(r, "precision_m", 100, 0, 1000)
	if err != nil {
		return nil, false, err
	}
	if err := validateShareValues(hours, expDays, delay, precision); err != nil {
		return nil, false, err
	}
	label, user, device := strings.TrimSpace(r.FormValue("label")), strings.TrimSpace(r.FormValue("user")), strings.TrimSpace(r.FormValue("device"))
	if err := validateShareText(label, user, device); err != nil {
		return nil, false, err
	}
	return &store.ShareLink{
		Label:        label,
		User:         user,
		Device:       device,
		Hours:        hours,
		ExpiresAt:    store.ShareExpiresAt(expDays),
		DelayMinutes: delay,
		PrecisionM:   precision,
	}, expDays == 0 || precision == 0, nil
}

func shareInt(r *http.Request, key string, fallback, minimum, maximum int) (int, error) {
	raw := strings.TrimSpace(r.FormValue(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", key, minimum, maximum)
	}
	return value, nil
}

func (s *Server) loadShare(token string) (*store.ShareLink, error) {
	link, err := s.Store.GetShareByToken(token)
	if err != nil {
		return nil, err
	}
	if !link.Active() {
		return nil, store.ErrShareExpired
	}
	link.Token = token
	return link, nil
}
