package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

func acknowledgeDownload(w http.ResponseWriter, r *http.Request, secure bool) {
	id := r.URL.Query().Get("download_id")
	if id == "" || len(id) > 80 || strings.ContainsFunc(id, func(r rune) bool {
		return r != '-' && (r < '0' || r > '9')
	}) {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "mytracker-download-" + id, Value: "1", Path: "/", MaxAge: 3600,
		Secure: secure, SameSite: http.SameSiteStrictMode,
	})
}

const (
	downloadWriteTimeout   = 120 * time.Second
	downloadPrepareTimeout = 30 * time.Minute
)

type downloadWriter struct {
	http.ResponseWriter
	ctx        context.Context
	controller *http.ResponseController
	timeout    time.Duration
	err        error
}

func newDownloadWriter(w http.ResponseWriter, ctx context.Context, timeout time.Duration) (*downloadWriter, error) {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return nil, err
	}
	return &downloadWriter{ResponseWriter: w, ctx: ctx, controller: controller, timeout: timeout}, nil
}

func (w *downloadWriter) deadline() error {
	if w.err == nil {
		w.err = w.ctx.Err()
	}
	if w.err == nil {
		err := w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
		if !errors.Is(err, http.ErrNotSupported) {
			w.err = err
		}
	}
	return w.err
}

func (w *downloadWriter) WriteHeader(status int) {
	if w.deadline() == nil {
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *downloadWriter) Write(p []byte) (int, error) {
	if err := w.deadline(); err != nil {
		return 0, err
	}
	n, err := w.ResponseWriter.Write(p)
	w.err = err
	return n, err
}

func (w *downloadWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
