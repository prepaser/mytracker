package web

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/prepaser/mytracker/internal/store"
)

func (s *Server) apiLocations(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	effectiveLimit := limit
	if effectiveLimit < 1 {
		effectiveLimit = 50
	}
	if effectiveLimit > 200 {
		effectiveLimit = 200
	}
	before, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		jsonErr(w, fmt.Errorf("cursor is invalid"), http.StatusBadRequest)
		return
	}
	locs, err := s.Store.ListCursorSummary(before, effectiveLimit+1)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	next := ""
	if len(locs) > effectiveLimit {
		locs = locs[:effectiveLimit]
		next = encodeCursor(locs[len(locs)-1].ID)
	}
	jsonOK(w, map[string]any{"data": locs, "next_cursor": next})
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.collectStatusContext(r.Context(), time.Now())
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": status})
}

func (s *Server) apiStampJobs(w http.ResponseWriter, r *http.Request) {
	if raw := r.URL.Query().Get("location_id"); raw != "" {
		locationID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || locationID < 1 {
			jsonErr(w, fmt.Errorf("location id is invalid"), http.StatusBadRequest)
			return
		}
		jobs, err := s.Store.ListLocationStampJobs(locationID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if jobs == nil {
			jobs = make([]store.StampJob, 0)
		}
		jsonOK(w, map[string]any{"data": jobs})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	jobs, err := s.Store.ListStampJobsByState(r.URL.Query().Get("state"), limit)
	if err != nil {
		if errors.Is(err, store.ErrInvalidStampJobState) {
			jsonErr(w, err, http.StatusBadRequest)
		} else {
			jsonErr(w, err, http.StatusInternalServerError)
		}
		return
	}
	jsonOK(w, map[string]any{"data": jobs})
}

func (s *Server) apiVerificationJobs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	jobs, err := s.Store.ListVerificationJobs(r.URL.Query().Get("state"), limit)
	if err != nil {
		if errors.Is(err, store.ErrInvalidVerificationJobState) {
			jsonErr(w, err, http.StatusBadRequest)
		} else {
			jsonErr(w, err, http.StatusInternalServerError)
		}
		return
	}
	jsonOK(w, map[string]any{"data": jobs})
}

func (s *Server) apiMaintenanceJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Store.ListMaintenanceJobs(r.URL.Query().Get("kind"), r.URL.Query().Get("state"))
	if err != nil {
		if errors.Is(err, store.ErrInvalidMaintenanceKind) || errors.Is(err, store.ErrInvalidMaintenanceState) {
			jsonErr(w, err, http.StatusBadRequest)
		} else {
			jsonErr(w, err, http.StatusInternalServerError)
		}
		return
	}
	jsonOK(w, map[string]any{"data": jobs})
}

func (s *Server) apiMaintenanceRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	before, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		jsonErr(w, fmt.Errorf("cursor is invalid"), http.StatusBadRequest)
		return
	}
	effective := limit
	if effective < 1 {
		effective = 50
	}
	if effective > 200 {
		effective = 200
	}
	runs, err := s.Store.ListMaintenanceRuns(before, effective+1)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	next := ""
	if len(runs) > effective {
		runs = runs[:effective]
		next = encodeCursor(runs[len(runs)-1].ID)
	}
	jsonOK(w, map[string]any{"data": runs, "next_cursor": next})
}

func (s *Server) apiMaintenanceRetry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonErr(w, fmt.Errorf("maintenance job id is invalid"), http.StatusBadRequest)
		return
	}
	job, err := s.Maintenance.Retry(id)
	if err != nil {
		jsonErr(w, err, http.StatusConflict)
		return
	}
	jsonOKStatus(w, map[string]any{"data": map[string]any{"job": job}}, http.StatusAccepted)
}

func (s *Server) apiStampRetry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonErr(w, fmt.Errorf("location id is invalid"), http.StatusBadRequest)
		return
	}
	n, err := s.Stamp.RetryResult(id)
	if err != nil {
		jsonErr(w, err, http.StatusConflict)
		return
	}
	jobs, err := s.Store.ListLocationStampJobs(id)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": map[string]any{"enqueued": n, "jobs": jobs}})
}

func (s *Server) apiStampRetryAll(w http.ResponseWriter, r *http.Request) {
	n, err := s.Stamp.RetryAll()
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"data": map[string]int{"retried": n}})
}

func (s *Server) apiBackup(w http.ResponseWriter, r *http.Request) {
	job, coalesced, err := s.Maintenance.Enqueue("backup", "manual")
	if err != nil {
		jsonErr(w, err, http.StatusServiceUnavailable)
		return
	}
	jsonOKStatus(w, map[string]any{
		"data": map[string]any{"job": job, "coalesced": coalesced},
	}, http.StatusAccepted)
}

func encodeCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

func decodeCursor(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, err
	}
	id, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("invalid cursor")
	}
	return id, nil
}
