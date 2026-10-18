package web

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/prepaser/mytracker/internal/store"
)

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	ids, _ := s.Store.ListIdentities()
	page(w, "timeline.html", map[string]any{"Page": "timeline", "Identities": ids, "CSRF": s.csrfToken(r)})
}
func (s *Server) apiTimeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err := parseOptionalUnix(q.Get("from"))
	if err != nil {
		jsonErr(w, fmt.Errorf("from is invalid"), 400)
		return
	}
	to, err := parseOptionalUnix(q.Get("to"))
	if err != nil || (from > 0 && to > 0 && from > to) {
		jsonErr(w, fmt.Errorf("to is invalid"), 400)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	beforeTst, beforeID, err := decodeTimelineCursor(q.Get("cursor"))
	if err != nil {
		jsonErr(w, fmt.Errorf("cursor is invalid"), 400)
		return
	}
	f := store.TimelineFilter{User: q.Get("user"), Device: q.Get("device"), From: from, To: to, BeforeTst: beforeTst, BeforeID: beforeID, Limit: limit + 1}
	v, summary, err := s.Store.TimelinePageSummaryContext(r.Context(), f)
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	next := ""
	if len(v) > limit {
		v = v[:limit]
		last := v[len(v)-1]
		next = encodeTimelineCursor(last.StartTst, last.ID)
	}
	jsonOK(w, map[string]any{"segments": v, "summary": summary, "next_cursor": next})
}

func parseOptionalUnix(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid timestamp")
	}
	return v, nil
}
func encodeTimelineCursor(tst, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d:%d", tst, id)))
}
func decodeTimelineCursor(raw string) (int64, int64, error) {
	if raw == "" {
		return 0, 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Split(string(b), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid cursor")
	}
	tst, e1 := strconv.ParseInt(parts[0], 10, 64)
	id, e2 := strconv.ParseInt(parts[1], 10, 64)
	if e1 != nil || e2 != nil || tst < 1 || id < 1 {
		return 0, 0, fmt.Errorf("invalid cursor")
	}
	return tst, id, nil
}

func (s *Server) apiGeofences(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.ListGeofences()
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	jsonOK(w, v)
}
func readGeofenceBody(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	const maxBody = 64 << 10
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("invalid geofence")
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("invalid geofence")
	}
	return body, nil
}
func decodeGeofenceBody(body []byte) (*store.Geofence, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("invalid geofence")
	}
	for _, key := range []string{"lat", "lon"} {
		value, ok := fields[key]
		if !ok || string(value) == "null" {
			return nil, fmt.Errorf("%s is required", key)
		}
	}
	var v store.Geofence
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("invalid geofence")
	}
	return &v, nil
}
func decodeGeofence(r *http.Request) (*store.Geofence, error) {
	body, err := readGeofenceBody(r)
	if err != nil {
		return nil, err
	}
	return decodeGeofenceBody(body)
}
func decodeGeofenceUpdate(r *http.Request) (*store.Geofence, *bool, error) {
	body, err := readGeofenceBody(r)
	if err != nil {
		return nil, nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, nil, fmt.Errorf("invalid geofence")
	}
	if len(fields) == 1 {
		if raw, ok := fields["enabled"]; ok {
			if strings.TrimSpace(string(raw)) == "null" {
				return nil, nil, fmt.Errorf("enabled is invalid")
			}
			var enabled bool
			if err := json.Unmarshal(raw, &enabled); err != nil {
				return nil, nil, fmt.Errorf("enabled is invalid")
			}
			return nil, &enabled, nil
		}
	}
	v, err := decodeGeofenceBody(body)
	return v, nil, err
}
func geofenceErrorStatus(err error) int {
	var input store.GeofenceInputError
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return http.StatusNotFound
	case errors.As(err, &input):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
func (s *Server) apiGeofenceCreate(w http.ResponseWriter, r *http.Request) {
	v, err := decodeGeofence(r)
	if err != nil {
		jsonErr(w, err, 400)
		return
	}
	v, err = s.Store.CreateGeofence(v)
	if err != nil {
		jsonErr(w, err, geofenceErrorStatus(err))
		return
	}
	jsonOKStatus(w, v, http.StatusCreated)
}
func (s *Server) apiGeofenceUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonErr(w, fmt.Errorf("invalid geofence id"), 400)
		return
	}
	v, enabled, err := decodeGeofenceUpdate(r)
	if err != nil {
		jsonErr(w, err, 400)
		return
	}
	if enabled != nil {
		v, err = s.Store.UpdateGeofenceEnabled(id, *enabled)
	} else {
		v, err = s.Store.UpdateGeofence(id, v)
	}
	if err != nil {
		jsonErr(w, err, geofenceErrorStatus(err))
		return
	}
	jsonOK(w, v)
}
func (s *Server) apiGeofenceDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonErr(w, err, 400)
		return
	}
	if err := s.Store.DeleteGeofence(id); err != nil {
		jsonErr(w, err, geofenceErrorStatus(err))
		return
	}
	jsonOK(w, map[string]any{"deleted": true})
}
func (s *Server) apiGeofenceEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	v, err := s.Store.ListGeofenceEvents(limit)
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	jsonOK(w, v)
}
