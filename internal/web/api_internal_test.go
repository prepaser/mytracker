package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/privacy"
	"github.com/prepaser/mytracker/internal/store"
)

func TestAPIHelpers(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 413, 429, 500} {
		if got := httpErrorCode(status); got == "" {
			t.Fatalf("status %d has no code", status)
		}
	}
	cursor := encodeCursor(42)
	if id, err := decodeCursor(cursor); err != nil || id != 42 {
		t.Fatalf("cursor=%d err=%v", id, err)
	}
	if _, err := decodeCursor(encodeCursor(-1)); err == nil {
		t.Fatal("negative cursor accepted")
	}
	base := httptest.NewRecorder()
	w := &statusWriter{ResponseWriter: base}
	w.WriteHeader(http.StatusCreated)
	w.WriteHeader(http.StatusInternalServerError)
	if _, err := w.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if w.status != http.StatusCreated || w.bytes != 2 || w.Unwrap() != base {
		t.Fatalf("writer=%+v", w)
	}
	if verificationErrorStatus(sql.ErrNoRows) != http.StatusNotFound ||
		verificationErrorStatus(errors.New("db failed")) != http.StatusInternalServerError {
		t.Fatal("verification errors mapped to the wrong status")
	}
}

func TestDecodeGeofenceRequiresCoordinatesButAllowsZero(t *testing.T) {
	for _, raw := range []string{`{"name":"x","lon":1,"radius_m":25}`, `{"name":"x","lat":1,"radius_m":25}`, `{"name":"x","lat":null,"lon":1,"radius_m":25}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/geofences", strings.NewReader(raw))
		if _, err := decodeGeofence(req); err == nil {
			t.Fatalf("accepted missing coordinate: %s", raw)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/geofences", strings.NewReader(`{"name":"origin","lat":0,"lon":0,"radius_m":25}`))
	v, err := decodeGeofence(req)
	if err != nil || v.Lat != 0 || v.Lon != 0 {
		t.Fatalf("zero coordinates rejected: %+v err=%v", v, err)
	}
}

func TestGeofenceBodyLimitAcceptsBoundaryAndRejectsOverflow(t *testing.T) {
	const maxBody = 64 << 10
	prefix := `{"name":"`
	suffix := `","lat":1,"lon":2,"radius_m":25}`
	body := prefix + strings.Repeat("x", maxBody-len(prefix)-len(suffix)) + suffix
	if len(body) != maxBody {
		t.Fatalf("body length=%d", len(body))
	}
	if _, err := decodeGeofence(httptest.NewRequest(http.MethodPost, "/api/geofences", strings.NewReader(body))); err != nil {
		t.Fatalf("boundary body rejected: %v", err)
	}
	if _, err := decodeGeofence(httptest.NewRequest(http.MethodPost, "/api/geofences", strings.NewReader(body+"x"))); err == nil {
		t.Fatal("oversized body accepted")
	}
}

func TestExportConcurrencyLimit(t *testing.T) {
	s := &Server{exportSem: make(chan struct{}, 1)}
	first := httptest.NewRecorder()
	release, ok := s.beginExport(first)
	if !ok {
		t.Fatal("first export rejected")
	}
	blocked := httptest.NewRecorder()
	if _, ok := s.beginExport(blocked); ok || blocked.Code != http.StatusTooManyRequests || blocked.Header().Get("Retry-After") != "10" {
		t.Fatalf("second export: ok=%t code=%d headers=%v", ok, blocked.Code, blocked.Header())
	}
	release()
	if release, ok := s.beginExport(httptest.NewRecorder()); !ok {
		t.Fatal("export slot was not released")
	} else {
		release()
	}
}

func TestDeleteRejectsConcurrentPurge(t *testing.T) {
	s := &Server{}
	s.deleteMu.Lock()
	defer s.deleteMu.Unlock()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/delete?from=1&confirm=DELETE&purge_backups=1", nil)
	s.apiDelete(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "privacy purge") {
		t.Fatalf("response=%d %s", rec.Code, rec.Body.String())
	}
}

func TestBackupStateVerifiesLatestFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mytracker.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	if state := s.verifyBackupState(filepath.Join(dir, "missing.mtbackup")); state != "invalid" {
		t.Fatalf("missing backup state=%q", state)
	}
	if state := s.verifyBackupState(dbPath); state != "ok" {
		t.Fatalf("valid backup state=%q", state)
	}
	if state := s.verifyBackupState(dbPath); state != "ok" {
		t.Fatalf("cached backup state=%q", state)
	}
	bad := filepath.Join(dir, "broken.mtbackup")
	if err := os.WriteFile(bad, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if state := s.verifyBackupState(bad); state != "invalid" {
		t.Fatalf("broken backup state=%q", state)
	}
}

func TestBackupAndResponseHelpers(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	if err := os.WriteFile(src, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "mytracker-keep.mtbackup")
	if err := copyPrivateFile(src, keep); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyPrivateFileContext(ctx, src, keep); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled private copy err=%v", err)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "original" {
		t.Fatalf("private copy changed destination: %q (%v)", data, err)
	}
	if temp, _ := filepath.Glob(filepath.Join(dir, ".mytracker-keep.mtbackup.tmp-*")); len(temp) != 0 {
		t.Fatalf("private copy temporary files remain: %v", temp)
	}
	old := filepath.Join(dir, "mytracker-old.db")
	if err := os.WriteFile(old, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if count, oldest := backupInventory(dir); count != 2 || oldest == "" {
		t.Fatalf("count=%d oldest=%q", count, oldest)
	}
	if err := privacy.RemoveBackupsExcept(dir, keep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old backup remains: %v", err)
	}
	safety := filepath.Join(dir, ".privacy-safety.mtbackup")
	if err := os.WriteFile(safety, []byte("safety"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := privacy.ClearSafetyBackup(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(safety); !os.IsNotExist(err) {
		t.Fatalf("safety backup remains: %v", err)
	}
	blocked := privacySafetyBackupPath(dir)
	if err := os.MkdirAll(filepath.Join(blocked, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := privacy.ClearSafetyBackup(dir); err == nil {
		t.Fatal("non-empty safety directory removed")
	}
	rec := httptest.NewRecorder()
	jsonOKStatus(rec, map[string]bool{"ok": true}, http.StatusAccepted)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("response=%d %s", rec.Code, rec.Body.String())
	}
}

func TestAPIRequestValidation(t *testing.T) {
	cfg := config.Default()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	if !validateAPIRequest(rec, req, cfg) {
		t.Fatal("GET rejected")
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/backups", nil)
	if !validateAPIRequest(rec, req, cfg) {
		t.Fatalf("bodyless POST rejected: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/backups", strings.NewReader("x"))
	if validateAPIRequest(rec, req, cfg) || rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("unsupported response=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/backups", strings.NewReader("x"))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = 2 << 20
	if validateAPIRequest(rec, req, cfg) || rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("large response=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(strings.Repeat("x", 65<<10)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if validateAPIRequest(rec, req, cfg) || rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized settings response=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/import", strings.NewReader("x"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	if !validateAPIRequest(rec, req, cfg) {
		t.Fatalf("multipart response=%d", rec.Code)
	}
}

func TestNonAPIFormRequestValidation(t *testing.T) {
	cfg := config.Default()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader("x"))
	req.Header.Set("Content-Type", "text/plain")
	if validateAPIRequest(rec, req, cfg) || rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("unsupported form response=%d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(strings.Repeat("x", 65<<10)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if validateAPIRequest(rec, req, cfg) || rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized form response=%d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader("active_tab=tracking"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if !validateAPIRequest(rec, req, cfg) {
		t.Fatalf("valid form rejected: %d", rec.Code)
	}
}

func TestUnknownAPIMethodUsesJSONEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/status", nil)
	if !rejectUnknownAPIMethod(rec, req) {
		t.Fatal("unsupported API method was not rejected")
	}
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Content-Type") != "application/json" ||
		!strings.Contains(rec.Body.String(), `"request_id"`) {
		t.Fatalf("response=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestLocationSummaryOmitsSensitiveFields(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "locations.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Insert(&store.Location{
		User: "u", Device: "d", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{"secret":"payload"}`), Digest: []byte{1, 2}, TSANonce: []byte{3, 4},
	}); err != nil {
		t.Fatal(err)
	}
	summaries, err := st.ListCursorSummary(0, 2)
	if err != nil || len(summaries) != 1 {
		t.Fatalf("summaries=%+v err=%v", summaries, err)
	}
	b, err := json.Marshal(summaries[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "payload") || strings.Contains(string(b), "digest") || strings.Contains(string(b), "nonce") {
		t.Fatalf("sensitive fields in summary: %s", b)
	}
}

func TestMobilityHelpers(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/geofences", strings.NewReader(`{"name":"x","lat":1,"lon":2,"radius_m":25}`))
	v, err := decodeGeofence(req)
	if err != nil || v.Name != "x" {
		t.Fatalf("geofence=%+v err=%v", v, err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/geofences", strings.NewReader(`{`))
	if _, err = decodeGeofence(req); err == nil {
		t.Fatal("invalid geofence accepted")
	}
	for _, raw := range []string{`{} {}`, `{} trailing`} {
		req = httptest.NewRequest(http.MethodPost, "/api/geofences", strings.NewReader(raw))
		if _, err = decodeGeofence(req); err == nil {
			t.Fatalf("trailing geofence data accepted: %q", raw)
		}
	}
	cursor := encodeTimelineCursor(100, 42)
	tst, id, err := decodeTimelineCursor(cursor)
	if err != nil || tst != 100 || id != 42 {
		t.Fatalf("timeline cursor=%d/%d err=%v", tst, id, err)
	}
	if _, _, err = decodeTimelineCursor("bad"); err == nil {
		t.Fatal("invalid timeline cursor accepted")
	}
}

func TestParseShareFormBounds(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/shares",
		strings.NewReader("hours=0&expires_days=3&delay_minutes=9999&precision_m=7"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, _, err := parseShareForm(req); err == nil {
		t.Fatal("out-of-range share accepted")
	}
	req = httptest.NewRequest(http.MethodPost, "/api/shares", strings.NewReader("hours=24&expires_days=3&delay_minutes=15&precision_m=500"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	link, risky, err := parseShareForm(req)
	if err != nil || link.Hours != 24 || link.DelayMinutes != 15 || link.PrecisionM != 500 || !link.ExpiresAt.Valid || risky {
		t.Fatalf("link=%+v risky=%t err=%v", link, risky, err)
	}
	for _, body := range []string{
		"hours=24&label=" + strings.Repeat("x", 81),
		"hours=24&user=bad%0Aidentity",
		"hours=24&device=" + strings.Repeat("x", 129),
	} {
		req = httptest.NewRequest(http.MethodPost, "/api/shares", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if _, _, err := parseShareForm(req); err == nil {
			t.Fatalf("oversized share accepted: %q", body)
		}
	}
}
