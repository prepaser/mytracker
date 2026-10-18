package web

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

const (
	maxShareQueries      = 8
	maxShareQueriesPerID = 2
	shareQueryTimeout    = 30 * time.Second
	shareQueryRetryAfter = 10
)

func (s *Server) beginShareQuery(w http.ResponseWriter, r *http.Request, id int64) (context.Context, func(), bool) {
	if !s.acquireShareQuery(id) {
		w.Header().Set("Retry-After", fmt.Sprint(shareQueryRetryAfter))
		jsonErr(w, fmt.Errorf("share query limit reached"), http.StatusServiceUnavailable)
		return nil, func() {}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), shareQueryTimeout)
	return ctx, func() {
		cancel()
		s.releaseShareQuery(id)
	}, true
}

func (s *Server) acquireShareQuery(id int64) bool {
	s.shareQueryMu.Lock()
	defer s.shareQueryMu.Unlock()
	if s.shareQueries >= maxShareQueries || s.shareQueryByID[id] >= maxShareQueriesPerID {
		return false
	}
	if s.shareQueryByID == nil {
		s.shareQueryByID = make(map[int64]int)
	}
	s.shareQueries++
	s.shareQueryByID[id]++
	return true
}

func (s *Server) releaseShareQuery(id int64) {
	s.shareQueryMu.Lock()
	s.shareQueries--
	s.shareQueryByID[id]--
	if s.shareQueryByID[id] == 0 {
		delete(s.shareQueryByID, id)
	}
	s.shareQueryMu.Unlock()
}
