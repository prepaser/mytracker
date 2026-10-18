package web

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

func (s *Server) apiVerify(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonErr(w, err, 400)
		return
	}
	out, err := s.Verification.LatestVerification(id)
	if err != nil {
		jsonErr(w, err, verificationErrorStatus(err))
		return
	}
	jsonOK(w, map[string]any{"data": out})
}

func verificationErrorStatus(err error) int {
	if errors.Is(err, sql.ErrNoRows) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func (s *Server) apiVerifyEnqueue(w http.ResponseWriter, r *http.Request) {
	var probe [1]byte
	if n, _ := r.Body.Read(probe[:]); n > 0 {
		jsonErr(w, fmt.Errorf("request body is not allowed"), http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonErr(w, fmt.Errorf("location id is invalid"), http.StatusBadRequest)
		return
	}
	jobs, err := s.Verification.EnqueueVerification(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonErr(w, err, http.StatusNotFound)
			return
		}
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOKStatus(w, map[string]any{"data": map[string]any{
		"location_id": id,
		"jobs":        jobs,
	}}, http.StatusAccepted)
}

func (s *Server) apiVerifyHistory(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonErr(w, fmt.Errorf("location id is invalid"), http.StatusBadRequest)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	before, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		jsonErr(w, fmt.Errorf("cursor is invalid"), http.StatusBadRequest)
		return
	}
	history, err := s.Verification.VerificationHistory(id, before, limit+1)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonErr(w, err, http.StatusNotFound)
			return
		}
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	next := ""
	if len(history) > limit {
		history = history[:limit]
		next = encodeCursor(history[len(history)-1].ID)
	}
	jsonOK(w, map[string]any{"data": history, "next_cursor": next})
}
