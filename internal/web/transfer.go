package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prepaser/mytracker/internal/atomicfile"
	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/export"
	"github.com/prepaser/mytracker/internal/gpx"
	"github.com/prepaser/mytracker/internal/importzip"
	"github.com/prepaser/mytracker/internal/metrics"
	"github.com/prepaser/mytracker/internal/privacy"
	"github.com/prepaser/mytracker/internal/store"
)

const privacySafetyBackupName = privacy.SafetyBackupName

func privacySafetyBackupPath(dataDir string) string {
	return filepath.Join(dataDir, privacySafetyBackupName)
}

func privacySafetyBackupExists(dataDir string) bool {
	pending, err := privacy.Pending(dataDir)
	return pending || err != nil
}

func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	release, ok := s.beginExport(w)
	if !ok {
		return
	}
	defer release()
	download, err := newDownloadWriter(w, r.Context(), downloadWriteTimeout)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	w = download
	prepareCtx, cancel := context.WithTimeout(r.Context(), downloadPrepareTimeout)
	defer cancel()
	started := time.Now()
	from, to, err := parseLocationRange(r.URL.Query(), started, true)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	filter := store.ExportFilter{From: from, To: to}
	cfg := s.Cfg.Snapshot()
	if err := backup.Scavenge(cfg); err != nil {
		slog.Warn("scavenge export artifacts failed")
	}
	name := export.Filename(time.Now())
	exportFile, err := os.CreateTemp(cfg.DataDir, ".export-*.zip")
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	nameOnDisk := exportFile.Name()
	defer func() {
		_ = exportFile.Close()
		_ = os.Remove(nameOnDisk)
	}()
	if err := exportFile.Chmod(0600); err != nil {
		jsonErr(w, err, 500)
		return
	}
	exported := 0
	if err := export.WriteZipContext(prepareCtx, exportFile, export.Options{
		Filter:    filter,
		ProofsDir: cfg.ProofsDir(),
		Store:     s.Store,
		OnCount:   func(n int) { exported = n },
	}); err != nil {
		jsonErr(w, err, 500)
		return
	}
	cancel()
	info, err := exportFile.Stat()
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	if _, err := exportFile.Seek(0, io.SeekStart); err != nil {
		jsonErr(w, err, 500)
		return
	}
	metrics.IncExport()
	metrics.ObserveData("export", false, time.Since(started), exported)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
	acknowledgeDownload(w, r, s.secureCookie(r))
	http.ServeContent(w, r, name, info.ModTime(), exportFile)
}

func (s *Server) apiGPXImport(w http.ResponseWriter, r *http.Request) {
	result := "error"
	defer func() { metrics.GPX("import", result) }()
	cfg := s.Cfg.Snapshot()
	r.Body = http.MaxBytesReader(w, r.Body, cfg.ImportMaxBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		jsonErr(w, fmt.Errorf("invalid multipart request"), 400)
		return
	}
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}
	user := strings.TrimSpace(r.FormValue("user"))
	device := strings.TrimSpace(r.FormValue("device"))
	if err := gpx.ValidateIdentity(user, device); err != nil {
		jsonErr(w, err, 400)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		jsonErr(w, fmt.Errorf("GPX file is required"), 400)
		return
	}
	defer func() { _ = f.Close() }()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil || size > cfg.ImportMaxBytes {
		jsonErr(w, fmt.Errorf("GPX file exceeds import_max_bytes"), 400)
		return
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		jsonErr(w, fmt.Errorf("invalid GPX file"), 400)
		return
	}
	counts, err := gpx.ImportContext(r.Context(), f, s.Store, user, device, cfg.StampRetries)
	if err != nil {
		jsonErr(w, fmt.Errorf("GPX import failed"), 400)
		return
	}
	s.Stamp.Wake()
	if s.Timeline != nil {
		s.Timeline.Wake()
	}
	result = "ok"
	jsonOK(w, map[string]any{"imported": counts.Imported, "skipped": counts.Skipped})
}

func (s *Server) apiGPXExport(w http.ResponseWriter, r *http.Request) {
	release, ok := s.beginExport(w)
	if !ok {
		return
	}
	defer release()
	download, err := newDownloadWriter(w, r.Context(), downloadWriteTimeout)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	w = download
	prepareCtx, cancel := context.WithTimeout(r.Context(), downloadPrepareTimeout)
	defer cancel()
	result := "error"
	defer func() { metrics.GPX("export", result) }()
	from, to, err := parseLocationRange(r.URL.Query(), time.Now(), false)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	f := store.TrackFilter{User: r.URL.Query().Get("user"), Device: r.URL.Query().Get("device"), From: from, To: to}
	snap, err := s.Store.BeginTrackSnapshotContext(prepareCtx, f)
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	defer func() { _ = snap.Close() }()
	cfg := s.Cfg.Snapshot()
	if err := backup.Scavenge(cfg); err != nil {
		slog.Warn("scavenge export artifacts failed")
	}
	tmp, err := os.CreateTemp(cfg.DataDir, ".export-*.gpx")
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	name := tmp.Name()
	defer func() { _ = tmp.Close(); _ = os.Remove(name) }()
	if err = tmp.Chmod(0600); err == nil {
		err = gpx.Write(tmp, "mytracker", func(fn func([]store.Location) error) error {
			return snap.Iterate(500, func(locs []store.Location) error {
				if err := prepareCtx.Err(); err != nil {
					return err
				}
				return fn(locs)
			})
		}, cfg.TripGapDuration())
	}
	if err == nil {
		err = snap.Close()
	}
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	cancel()
	info, err := tmp.Stat()
	if err != nil {
		jsonErr(w, err, 500)
		return
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		jsonErr(w, err, 500)
		return
	}
	w.Header().Set("Content-Type", "application/gpx+xml")
	w.Header().Set("Content-Disposition", `attachment; filename="mytracker.gpx"`)
	acknowledgeDownload(w, r, s.secureCookie(r))
	http.ServeContent(w, r, "mytracker.gpx", info.ModTime(), tmp)
	if download.err == nil {
		result = "ok"
	}
}

func (s *Server) beginExport(w http.ResponseWriter) (func(), bool) {
	select {
	case s.exportSem <- struct{}{}:
		return func() { <-s.exportSem }, true
	default:
		w.Header().Set("Retry-After", "10")
		jsonErr(w, fmt.Errorf("another export is already running"), http.StatusTooManyRequests)
		return func() {}, false
	}
}

func (s *Server) apiImport(w http.ResponseWriter, r *http.Request) {
	cfg := s.Cfg.Snapshot()
	r.Body = http.MaxBytesReader(w, r.Body, cfg.ImportMaxBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		jsonErr(w, fmt.Errorf("invalid multipart request"), 400)
		return
	}
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		jsonErr(w, fmt.Errorf("import file is required"), 400)
		return
	}
	defer func() { _ = f.Close() }()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		jsonErr(w, fmt.Errorf("invalid import file"), 400)
		return
	}
	if size > cfg.ImportMaxBytes {
		jsonErr(w, fmt.Errorf("archive exceeds import_max_bytes"), 400)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		jsonErr(w, fmt.Errorf("invalid import file"), 400)
		return
	}
	archive, err := importzip.ValidateArchiveContext(r.Context(), f, size)
	if err != nil {
		jsonErr(w, fmt.Errorf("invalid import archive"), 400)
		return
	}
	defer func() { _ = archive.Close() }()
	if _, err := backup.CreateContext(r.Context(), cfg, s.Store); err != nil {
		jsonErr(w, fmt.Errorf("backup failed: %w", err), 500)
		return
	}
	metrics.Backup("ok")
	res, err := importzip.ImportValidatedContext(r.Context(), archive, s.Store, cfg.ProofsDir(), s.Stamp, cfg.StampRetries)
	if err != nil {
		if importzip.IsCommitted(err) {
			slog.Warn("import committed with follow-up pending")
			jsonOKStatus(w, res, http.StatusAccepted)
			return
		}
		jsonErr(w, fmt.Errorf("import failed"), 400)
		return
	}
	jsonOK(w, res)
}

func (s *Server) apiDeletePreview(w http.ResponseWriter, r *http.Request) {
	filter, err := exportFilter(r)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if filter.From == 0 && filter.To == 0 {
		jsonErr(w, fmt.Errorf("from or to is required"), http.StatusBadRequest)
		return
	}
	n, err := s.Store.CountExport(filter)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	proofs, err := s.Store.CountExportProofs(filter)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	cfg := s.Cfg.Snapshot()
	backups, oldest := backupInventory(cfg.BackupsDir())
	jsonOK(w, map[string]any{
		"locations": n, "proofs": proofs, "backups": backups, "oldest_backup_at": oldest,
		"purge_pending": privacySafetyBackupExists(cfg.DataDir),
	})
}

func (s *Server) apiDelete(w http.ResponseWriter, r *http.Request) {
	filter, err := exportFilter(r)
	if err != nil || (filter.From == 0 && filter.To == 0) {
		jsonErr(w, fmt.Errorf("from or to is required"), http.StatusBadRequest)
		return
	}
	if r.FormValue("confirm") != "DELETE" {
		jsonErr(w, fmt.Errorf("confirmation required"), http.StatusBadRequest)
		return
	}
	if !s.deleteMu.TryLock() {
		jsonErr(w, fmt.Errorf("another privacy purge is already running"), http.StatusConflict)
		return
	}
	defer s.deleteMu.Unlock()
	cfg := s.Cfg.Snapshot()
	purge := r.FormValue("purge_backups") == "1"
	createBackup := backup.CreateLockedContext
	if purge {
		createBackup = backup.CreateSafetyLockedContext
	}
	unlockBackup, err := s.Store.LockBackupContext(r.Context())
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer unlockBackup()
	maxID, err := s.Store.LocationWatermarkContext(r.Context())
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	pre, err := createBackup(r.Context(), cfg, s.Store)
	if err != nil {
		metrics.Backup("fail")
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	metrics.Backup("ok")
	safety := ""
	if purge {
		safety = privacySafetyBackupPath(cfg.DataDir)
		if _, err := os.Lstat(safety); os.IsNotExist(err) {
			if err := copyPrivateFileContext(r.Context(), pre, safety); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
		} else if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}
	n, err := s.Store.DeleteRangeThroughContext(r.Context(), cfg.ProofsDir(), filter, maxID)
	if err != nil {
		if errors.Is(err, store.ErrDeletionPending) || n > 0 && errors.Is(err, context.Canceled) {
			slog.Warn("security privacy purge pending", "deleted", n)
			jsonOKStatus(w, map[string]any{"deleted": n, "purge_pending": true}, http.StatusAccepted)
			return
		}
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !purge {
		pending, err := privacy.Pending(cfg.DataDir)
		if err != nil {
			slog.Warn("security privacy purge state inspection failed", "deleted", n, "err", err)
			jsonOKStatus(w, map[string]any{"deleted": n, "purge_pending": true}, http.StatusAccepted)
			return
		}
		if pending {
			slog.Warn("security privacy purge pending", "deleted", n, "backups_purged", false)
			jsonOKStatus(w, map[string]any{"deleted": n, "purge_pending": true}, http.StatusAccepted)
			return
		}
		slog.Info("security privacy purge completed", "deleted", n, "backups_purged", false)
		jsonOK(w, map[string]any{"deleted": n, "purge_pending": false})
		return
	}
	post, err := createBackup(r.Context(), cfg, s.Store)
	if err != nil {
		slog.Warn("security privacy purge pending", "deleted", n)
		jsonOKStatus(w, map[string]any{"deleted": n, "purge_pending": true}, http.StatusAccepted)
		return
	}
	unlockProofs, err := s.Store.LockProofSnapshotContext(r.Context())
	if err != nil {
		jsonOKStatus(w, map[string]any{"deleted": n, "purge_pending": true}, http.StatusAccepted)
		return
	}
	defer unlockProofs()
	gc, err := s.Store.ProofGCStatusContext(r.Context())
	if err != nil || gc.Pending > 0 || s.Store.CheckpointContext(r.Context()) != nil {
		jsonOKStatus(w, map[string]any{"deleted": n, "purge_pending": true}, http.StatusAccepted)
		return
	}
	if _, err := os.Stat(post); err != nil {
		slog.Warn("security privacy purge pending", "deleted", n)
		jsonOKStatus(w, map[string]any{"deleted": n, "purge_pending": true}, http.StatusAccepted)
		return
	}
	if err := privacy.RemoveBackupsExcept(cfg.BackupsDir(), post); err != nil {
		jsonOKStatus(w, map[string]any{"deleted": n, "purge_pending": true}, http.StatusAccepted)
		return
	}
	if err := privacy.ClearSafetyBackup(cfg.DataDir); err != nil {
		slog.Warn("security privacy purge pending", "deleted", n)
		jsonOKStatus(w, map[string]any{"deleted": n, "purge_pending": true}, http.StatusAccepted)
		return
	}
	slog.Info("security privacy purge completed", "deleted", n, "backups_purged", true)
	jsonOK(w, map[string]any{"deleted": n, "purge_pending": false, "backup": filepath.Base(post)})
}

func exportFilter(r *http.Request) (store.ExportFilter, error) {
	var filter store.ExportFilter
	for key, dst := range map[string]*int64{"from": &filter.From, "to": &filter.To} {
		raw := strings.TrimSpace(r.FormValue(key))
		if raw == "" {
			continue
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			return filter, fmt.Errorf("%s is invalid", key)
		}
		*dst = v
	}
	if filter.From > 0 && filter.To > 0 && filter.From > filter.To {
		return filter, fmt.Errorf("from must not exceed to")
	}
	return filter, nil
}

func (s *Server) apiMetrics(w http.ResponseWriter, r *http.Request) {
	metrics.Handler().ServeHTTP(w, r)
}

func backupInventory(dir string) (int, string) {
	entries, _ := os.ReadDir(dir)
	count := 0
	var oldest time.Time
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".mtbackup") && !strings.HasSuffix(entry.Name(), ".db")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		count++
		if oldest.IsZero() || info.ModTime().Before(oldest) {
			oldest = info.ModTime()
		}
	}
	if oldest.IsZero() {
		return count, ""
	}
	return count, oldest.UTC().Format(time.RFC3339)
}

func copyPrivateFile(src, dst string) error {
	return copyPrivateFileContext(context.Background(), src, dst)
}

func copyPrivateFileContext(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	return atomicfile.WriteFunc(dst, 0600, func(out *os.File) error {
		_, err := io.Copy(out, contextReader{ctx: ctx, r: in})
		return err
	})
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
