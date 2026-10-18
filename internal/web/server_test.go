package web_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/export"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/testutil"
)

func login(t *testing.T, srv http.Handler) *http.Cookie {
	t.Helper()
	form := url.Values{"password": {testutil.AdminPassword}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login code=%d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "session" && c.Value != "" {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func authed(t *testing.T, srv http.Handler, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	c := login(t, srv)
	req := httptest.NewRequest(method, path, body)
	req.AddCookie(c)
	if method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", csrfToken(t, srv, c))
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func csrfToken(t *testing.T, srv http.Handler, c *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(rec.Body.String())
	if len(m) != 2 {
		t.Fatal("no csrf token")
	}
	return m[1]
}

func TestLoginSuccess(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	c := login(t, srv.Handler())
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatal("bad session cookie")
	}
	if !strings.HasPrefix(srv.Cfg.Snapshot().AdminHash, "$argon2id$") {
		t.Fatal("bcrypt hash was not upgraded after login")
	}
}

func TestProtectedGETAllowsHEADWithoutCSRF(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	handler := srv.Handler()
	c := login(t, handler)
	req := httptest.NewRequest(http.MethodHead, "/api/status", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD /api/status code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLoginFailure(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	form := url.Values{"password": {"wrong"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "login failed") {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	form := url.Values{"password": {"wrong"}}
	var rec *httptest.ResponseRecorder
	for range 11 {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec = httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing retry-after")
	}
}

func TestAuthRedirect(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("code=%d loc=%s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestDashboardRequiresAuth(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	rec := authed(t, srv.Handler(), http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Dashboard") {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestAPIStats(t *testing.T) {
	cfg, _, _, ot, srv, _ := testutil.Setup(t)
	req := httptest.NewRequest(http.MethodPost, "/pub", strings.NewReader(`{"_type":"location","lat":1,"lon":2,"tst":99}`))
	req.SetBasicAuth(cfg.PubUser, cfg.PubPass)
	rec := httptest.NewRecorder()
	ot.ServeHTTP(rec, req)

	rec = authed(t, srv.Handler(), http.MethodGet, "/api/stats", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var stats struct {
		Total int `json:"Total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if stats.Total != 1 {
		t.Fatalf("total=%d", stats.Total)
	}
}

func TestSettingsSaveOwnTracks(t *testing.T) {
	_, st, _, _, srv, _ := testutil.Setup(t)
	c := login(t, srv.Handler())
	form := url.Values{
		"active_tab":            {"proofs"},
		"pub_user":              {"newuser"},
		"pub_pass":              {"newpass"},
		"tsa_url":               {"https://example.com/tsr"},
		"ots_calendars":         {"https://calendar.example/"},
		"esplora_urls":          {"https://one.example/api\nhttps://two.example/api"},
		"tsa_trust_roots":       {"/tmp/tsa-roots.pem"},
		"tsa_policy_oids":       {"1.2.3.4\n1.2.3.5"},
		"ots_min_confirmations": {"12"},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrfToken(t, srv.Handler(), c))
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code=%d", rec.Code)
	}
	if location := rec.Header().Get("Location"); location != "/settings?saved=1#proofs" {
		t.Fatalf("location=%q", location)
	}
	snap := srv.Cfg.Snapshot()
	if snap.PubUser != "newuser" || snap.PubPass != "newpass" {
		t.Fatalf("cfg not updated: user=%q pass=%q", snap.PubUser, snap.PubPass)
	}
	data, err := st.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	var saved config.Settings
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.PubUser != "newuser" || saved.TSAURL != "https://example.com/tsr" {
		t.Fatalf("not persisted: %+v", saved)
	}
	if len(saved.EsploraURLs) != 2 || saved.EsploraURLs[1] != "https://two.example/api" {
		t.Fatalf("esplora_urls=%v", saved.EsploraURLs)
	}
	if saved.TSATrustRoots != "/tmp/tsa-roots.pem" || len(saved.TSAPolicyOIDs) != 2 ||
		saved.OTSConfirmations != 12 {
		t.Fatalf("trust policy=%+v", saved)
	}
	if snap.PubURL("wrong.host") != "http://newuser:newpass@wrong.host:8080/pub" {
		t.Fatalf("pub url=%q", snap.PubURL("wrong.host"))
	}
}

func TestSettingsIgnoresDeploymentFields(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	before := srv.Cfg.Snapshot()
	c := login(t, srv.Handler())
	get := httptest.NewRequest(http.MethodGet, "/settings", nil)
	get.AddCookie(c)
	page := httptest.NewRecorder()
	srv.Handler().ServeHTTP(page, get)
	if page.Code != http.StatusOK {
		t.Fatalf("settings page code=%d", page.Code)
	}
	for _, field := range []string{
		`name="public_url"`, `name="trusted_proxies"`, `name="log_level"`,
		`name="login_rate_limit"`, `name="login_rate_window"`, `name="pub_rate_limit"`,
		`name="pub_rate_window"`, `name="share_rate_limit"`, `name="share_rate_window"`,
		`name="rate_limit_max_ips"`, `name="stamp_workers"`, `name="stamp_retries"`,
		`name="import_max_mib"`,
	} {
		if strings.Contains(page.Body.String(), field) {
			t.Errorf("settings page contains deployment field %s", field)
		}
	}
	form := url.Values{
		"public_url":         {"https://changed.example"},
		"trusted_proxies":    {"not-an-ip"},
		"log_level":          {"invalid"},
		"login_rate_limit":   {"invalid"},
		"login_rate_window":  {"invalid"},
		"pub_rate_limit":     {"invalid"},
		"pub_rate_window":    {"invalid"},
		"share_rate_limit":   {"invalid"},
		"share_rate_window":  {"invalid"},
		"rate_limit_max_ips": {"invalid"},
		"stamp_workers":      {"invalid"},
		"stamp_retries":      {"invalid"},
		"import_max_mib":     {"invalid"},
		"pub_user":           {"owntracks"},
		"tsa_url":            {"https://example.com/tsr"},
		"ots_calendars":      {"https://calendar.example/"},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrfToken(t, srv.Handler(), c))
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	after := srv.Cfg.Snapshot()
	if after.PublicURL != before.PublicURL || strings.Join(after.TrustedProxies, ",") != strings.Join(before.TrustedProxies, ",") ||
		after.LogLevel != before.LogLevel || after.LoginRateLimit != before.LoginRateLimit ||
		after.LoginRateWindow != before.LoginRateWindow || after.PubRateLimit != before.PubRateLimit ||
		after.PubRateWindow != before.PubRateWindow || after.ShareRateLimit != before.ShareRateLimit ||
		after.ShareRateWindow != before.ShareRateWindow || after.RateLimitMaxIPs != before.RateLimitMaxIPs ||
		after.StampWorkers != before.StampWorkers || after.StampRetries != before.StampRetries ||
		after.ImportMaxBytes != before.ImportMaxBytes {
		t.Fatalf("deployment settings changed: before=%+v after=%+v", before.Deployment, after.Deployment)
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("missing referrer policy")
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" || strings.Contains(csp, "unsafe-inline") {
		t.Fatal("missing csp")
	}
	for _, header := range []string{
		"Permissions-Policy", "Cross-Origin-Opener-Policy",
		"Cross-Origin-Resource-Policy", "Origin-Agent-Cluster",
	} {
		if rec.Header().Get(header) == "" {
			t.Fatalf("missing %s", header)
		}
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing no-store")
	}
}

func TestHSTSWithoutDeprecationHeaders(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("missing hsts")
	}
	if rec.Header().Get("Deprecation") != "" || rec.Header().Get("Link") != "" {
		t.Fatal("unexpected API version headers")
	}
}

func TestAPITrack(t *testing.T) {
	cfg, _, _, ot, srv, _ := testutil.Setup(t)
	tst := time.Now().Unix()
	body := fmt.Sprintf(`{"_type":"location","lat":37.5,"lon":127.0,"tst":%d}`, tst)
	req := httptest.NewRequest(http.MethodPost, "/pub", strings.NewReader(body))
	req.SetBasicAuth(cfg.PubUser, cfg.PubPass)
	ot.ServeHTTP(httptest.NewRecorder(), req)

	rec := authed(t, srv.Handler(), http.MethodGet, "/api/track?hours=24", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var fc struct {
		Type     string `json:"type"`
		Features []any  `json:"features"`
		Metadata struct {
			TotalPoints    int  `json:"total_points"`
			ReturnedPoints int  `json:"returned_points"`
			Sampled        bool `json:"sampled"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&fc); err != nil {
		t.Fatal(err)
	}
	if fc.Type != "FeatureCollection" || len(fc.Features) < 2 {
		t.Fatalf("got %+v", fc)
	}
	if fc.Metadata.TotalPoints != 1 || fc.Metadata.ReturnedPoints != 1 || fc.Metadata.Sampled {
		t.Fatalf("metadata=%+v", fc.Metadata)
	}
}

func TestAPIIdentitiesEmpty(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	rec := authed(t, srv.Handler(), http.MethodGet, "/api/identities", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"data":[]}` {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPITrackSamplesWholeRange(t *testing.T) {
	_, st, _, _, srv, _ := testutil.Setup(t)
	for i := range 3 {
		if _, err := st.Insert(&store.Location{
			User: "u", Device: "d", Tst: int64(100 + i),
			Lat: 37 + float64(i), Lon: 127,
			Payload: []byte(`{}`), Digest: []byte{byte(i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	rec := authed(t, srv.Handler(), http.MethodGet, "/api/track?limit=2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var fc struct {
		Features []any `json:"features"`
		Metadata struct {
			TotalPoints    int    `json:"total_points"`
			ReturnedPoints int    `json:"returned_points"`
			Sampled        bool   `json:"sampled"`
			Algorithm      string `json:"algorithm"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&fc); err != nil {
		t.Fatal(err)
	}
	if !fc.Metadata.Sampled || fc.Metadata.TotalPoints != 3 ||
		fc.Metadata.ReturnedPoints != 2 || fc.Metadata.Algorithm != "trajectory-v1" ||
		len(fc.Features) != 3 {
		t.Fatalf("response=%+v features=%d", fc.Metadata, len(fc.Features))
	}
}

func TestAPIExport(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	rec := authed(t, srv.Handler(), http.MethodGet, "/api/export", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("content-type=%q", ct)
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "mytracker-export-") {
		t.Fatalf("disposition=%q", rec.Header().Get("Content-Disposition"))
	}
	if len(rec.Body.Bytes()) < 100 {
		t.Fatal("zip too small")
	}
}

func TestOwnTracksURLIsRevealedOnlyByAPI(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	page := authed(t, srv.Handler(), http.MethodGet, "/settings", nil)
	if strings.Contains(page.Body.String(), "pub-secret") {
		t.Fatal("credential leaked in settings HTML")
	}
	c := login(t, srv.Handler())
	req := httptest.NewRequest(http.MethodPost, "/api/owntracks-url", strings.NewReader("csrf="+csrfToken(t, srv.Handler(), c)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pub-secret") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/api/owntracks-url", strings.NewReader("csrf="+csrfToken(t, srv.Handler(), c)))
	req.Host = "tracker.example.com"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://owntracks:pub-secret@tracker.example.com/pub") {
		t.Fatalf("proxied URL code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeletePreviewAndDelete(t *testing.T) {
	cfg, st, _, _, srv, _ := testutil.Setup(t)
	for _, tst := range []int64{100, 200} {
		if _, err := st.Insert(&store.Location{
			User: "a", Device: "p", Tst: tst, Lat: 1, Lon: 2,
			Payload: []byte(`{}`), Digest: []byte{byte(tst)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, ".privacy-safety.mtbackup"), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	preview := authed(t, srv.Handler(), http.MethodGet, "/api/delete-preview?from=150&to=250", nil)
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), `"locations":1`) ||
		!strings.Contains(preview.Body.String(), `"purge_pending":true`) {
		t.Fatalf("preview=%d %s", preview.Code, preview.Body.String())
	}
	c := login(t, srv.Handler())
	form := url.Values{
		"from": {"150"}, "to": {"250"}, "confirm": {"DELETE"}, "purge_backups": {"1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrfToken(t, srv.Handler(), c))
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deleted":1`) {
		t.Fatalf("delete=%d %s", rec.Code, rec.Body.String())
	}
	if count, _ := backupInventoryForTest(cfg.BackupsDir()); count != 1 {
		t.Fatalf("backups=%d", count)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, ".privacy-safety.mtbackup")); !os.IsNotExist(err) {
		t.Fatalf("safety backup remains after successful purge: %v", err)
	}
}

func TestDeleteReportsPendingProofCleanup(t *testing.T) {
	cfg, st, _, _, srv, _ := testutil.Setup(t)
	id, err := st.Insert(&store.Location{
		User: "a", Device: "p", Tst: 100, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: []byte{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(cfg.ProofsDir(), fmt.Sprintf("%d.tsr", id))
	if err := os.MkdirAll(filepath.Join(blocked, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	safety := filepath.Join(cfg.DataDir, ".privacy-safety.mtbackup")
	if err := os.WriteFile(safety, []byte("previous safety"), 0600); err != nil {
		t.Fatal(err)
	}
	c := login(t, srv.Handler())
	form := url.Values{"from": {"1"}, "confirm": {"DELETE"}, "purge_backups": {"1"}}
	req := httptest.NewRequest(http.MethodPost, "/api/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrfToken(t, srv.Handler(), c))
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"purge_pending":true`) ||
		!strings.Contains(rec.Body.String(), `"deleted":1`) {
		t.Fatalf("delete=%d %s", rec.Code, rec.Body.String())
	}
	if count, _ := backupInventoryForTest(cfg.BackupsDir()); count == 0 {
		t.Fatal("recovery backup was removed")
	}
	if data, err := os.ReadFile(safety); err != nil || string(data) != "previous safety" {
		t.Fatalf("safety backup changed after pending purge: %q (%v)", data, err)
	}
	if _, err := os.Stat(safety); err != nil {
		t.Fatalf("safety backup missing after pending purge: %v", err)
	}
}

func backupInventoryForTest(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".mtbackup") {
			count++
		}
	}
	return count, nil
}

func TestStatusJobsAndErrors(t *testing.T) {
	cfg, st, _, _, srv, _ := testutil.Setup(t)
	if _, err := st.Insert(&store.Location{
		User: "a", Device: "p", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: []byte{1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, ".privacy-safety.mtbackup"), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/status", "/api/stamp-jobs"} {
		rec := authed(t, srv.Handler(), http.MethodGet, path, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data"`) {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	status := authed(t, srv.Handler(), http.MethodGet, "/api/status", nil)
	for _, field := range []string{`"workers"`, `"mqtt"`, `"proof_gc"`, `"proof_verification"`,
		`"policy_stale"`, `"backup"`, `"stamp_jobs"`, `"maintenance"`, `"realtime"`,
		`"admin_connections"`, `"share_connections"`, `"hub"`} {
		if !strings.Contains(status.Body.String(), field) {
			t.Fatalf("status missing %s: %s", field, status.Body.String())
		}
	}
	var statusBody struct {
		Data struct {
			ProofVerification struct {
				ExpiredLeases int `json:"expired_leases"`
			} `json:"proof_verification"`
			Privacy struct {
				BackupRetentionDays int  `json:"backup_retention_days"`
				BackupKeep          int  `json:"backup_keep"`
				PurgePending        bool `json:"purge_pending"`
			} `json:"privacy"`
		} `json:"data"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &statusBody); err != nil ||
		statusBody.Data.ProofVerification.ExpiredLeases != 0 ||
		statusBody.Data.Privacy.BackupRetentionDays != 14 || statusBody.Data.Privacy.BackupKeep != 14 ||
		!statusBody.Data.Privacy.PurgePending {
		t.Fatalf("status body=%+v err=%v", statusBody, err)
	}
	bad := authed(t, srv.Handler(), http.MethodGet, "/api/locations?cursor=bad!", nil)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), `"code":"bad_request"`) ||
		!strings.Contains(bad.Body.String(), `"request_id"`) {
		t.Fatalf("bad cursor=%d %s", bad.Code, bad.Body.String())
	}
	old := authed(t, srv.Handler(), http.MethodGet, "/api/v2/status", nil)
	if old.Code != http.StatusNotFound {
		t.Fatalf("old API code=%d body=%s", old.Code, old.Body.String())
	}
}

func TestMaintenanceAPIContract(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	jobs := authed(t, srv.Handler(), http.MethodGet, "/api/maintenance-jobs", nil)
	if jobs.Code != http.StatusOK || !strings.Contains(jobs.Body.String(), `"kind":"backup"`) {
		t.Fatalf("jobs=%d %s", jobs.Code, jobs.Body.String())
	}
	filtered := authed(t, srv.Handler(), http.MethodGet, "/api/maintenance-jobs?kind=backup", nil)
	if filtered.Code != http.StatusOK || strings.Contains(filtered.Body.String(), `"kind":"proof_gc"`) {
		t.Fatalf("filtered=%d %s", filtered.Code, filtered.Body.String())
	}
	bad := authed(t, srv.Handler(), http.MethodGet, "/api/maintenance-jobs?state=bad", nil)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), `"request_id"`) {
		t.Fatalf("bad=%d %s", bad.Code, bad.Body.String())
	}
	runs := authed(t, srv.Handler(), http.MethodGet, "/api/maintenance-runs?limit=1", nil)
	if runs.Code != http.StatusOK || !strings.Contains(runs.Body.String(), `"next_cursor"`) {
		t.Fatalf("runs=%d %s", runs.Code, runs.Body.String())
	}
	badCursor := authed(t, srv.Handler(), http.MethodGet, "/api/maintenance-runs?cursor=bad!", nil)
	if badCursor.Code != http.StatusBadRequest {
		t.Fatalf("cursor=%d %s", badCursor.Code, badCursor.Body.String())
	}
	retry := authed(t, srv.Handler(), http.MethodPost, "/api/maintenance-jobs/999/retry", nil)
	if retry.Code != http.StatusConflict {
		t.Fatalf("retry=%d %s", retry.Code, retry.Body.String())
	}
}

func TestManualBackupAPI(t *testing.T) {
	cfg, _, _, _, srv, _ := testutil.Setup(t)
	rec := authed(t, srv.Handler(), http.MethodPost, "/api/backups", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("backup=%d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data struct {
			Job store.MaintenanceJob `json:"job"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Job.Kind != "backup" {
		t.Fatalf("job=%+v", out.Data.Job)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		path, _, err := store.LatestBackup(cfg.BackupsDir())
		if err == nil {
			if _, err := backup.VerifyFile(path); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("backup did not complete")
}

func TestAdditionalAdminRoutes(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	for _, path := range []string{"/map", "/api/locations", "/metrics"} {
		rec := authed(t, srv.Handler(), http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s code=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	c := login(t, srv.Handler())
	form := url.Values{"csrf": {csrfToken(t, srv.Handler(), c)}}
	req := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout=%d", rec.Code)
	}
}

func TestVerifyAPIWithoutProofs(t *testing.T) {
	_, st, _, _, srv, _ := testutil.Setup(t)
	payload := []byte(`{"_type":"location","lat":1,"lon":2,"tst":100}`)
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	rec, _ := canonical.FromLocation("u", "d", raw)
	digest, _ := rec.Digest()
	id, err := st.Insert(&store.Location{
		User: "u", Device: "d", Tst: 100, Lat: 1, Lon: 2,
		Payload: payload, Digest: digest[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	out := authed(t, srv.Handler(), http.MethodGet, "/api/verify/"+strconv.FormatInt(id, 10), nil)
	if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), `"overall"`) ||
		!strings.Contains(out.Body.String(), `"trust_policy"`) {
		t.Fatalf("verify=%d %s", out.Code, out.Body.String())
	}
	out = authed(t, srv.Handler(), http.MethodPost, "/api/verify/"+strconv.FormatInt(id, 10), nil)
	if out.Code != http.StatusAccepted || !strings.Contains(out.Body.String(), `"jobs"`) {
		t.Fatalf("reverify=%d %s", out.Code, out.Body.String())
	}
	jobs := authed(t, srv.Handler(), http.MethodGet, "/api/verification-jobs?state=queued&limit=1", nil)
	if jobs.Code != http.StatusOK || !strings.Contains(jobs.Body.String(), `"data"`) {
		t.Fatalf("verification jobs=%d %s", jobs.Code, jobs.Body.String())
	}
	badJobs := authed(t, srv.Handler(), http.MethodGet, "/api/verification-jobs?state=bad", nil)
	if badJobs.Code != http.StatusBadRequest {
		t.Fatalf("bad verification jobs=%d %s", badJobs.Code, badJobs.Body.String())
	}
	waitVerificationResult(t, st, id, "tsa", "missing")
	out = authed(t, srv.Handler(), http.MethodGet, "/api/verify/"+strconv.FormatInt(id, 10), nil)
	if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), `"result":"missing"`) ||
		!strings.Contains(out.Body.String(), `"jobs"`) {
		t.Fatalf("completed verify=%d %s", out.Code, out.Body.String())
	}
	history := authed(t, srv.Handler(), http.MethodGet,
		"/api/verify/"+strconv.FormatInt(id, 10)+"/history?limit=1", nil)
	if history.Code != http.StatusOK || !strings.Contains(history.Body.String(), `"next_cursor"`) {
		t.Fatalf("history=%d %s", history.Code, history.Body.String())
	}
	for _, tc := range []struct {
		method string
		path   string
		code   int
	}{
		{http.MethodPost, "/api/verify/nope", http.StatusBadRequest},
		{http.MethodPost, "/api/verify/999999", http.StatusNotFound},
		{http.MethodGet, "/api/verify/nope/history", http.StatusBadRequest},
		{http.MethodGet, "/api/verify/999999/history", http.StatusNotFound},
		{http.MethodGet, "/api/verify/" + strconv.FormatInt(id, 10) + "/history?cursor=bad!", http.StatusBadRequest},
	} {
		rec := authed(t, srv.Handler(), tc.method, tc.path, nil)
		if rec.Code != tc.code {
			t.Fatalf("%s %s=%d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestImportRejectsInvalidArchive(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "bad.zip")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("not a zip"))
	_ = mw.Close()
	c := login(t, srv.Handler())
	req := httptest.NewRequest(http.MethodPost, "/api/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken(t, srv.Handler(), c))
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestImportValidArchive(t *testing.T) {
	srcDir := t.TempDir()
	src, err := store.Open(filepath.Join(srcDir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"_type":"location","lat":1,"lon":2,"tst":100}`)
	var raw map[string]any
	_ = json.Unmarshal(payload, &raw)
	can, _ := canonical.FromLocation("u", "d", raw)
	digest, _ := can.Digest()
	if _, err := src.Insert(&store.Location{
		User: "u", Device: "d", Tst: 100, Lat: 1, Lon: 2,
		Payload: payload, Digest: digest[:],
	}); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := export.WriteZip(&archive, export.Options{Store: src, ProofsDir: filepath.Join(srcDir, "proofs")}); err != nil {
		t.Fatal(err)
	}
	_ = src.Close()

	_, target, _, _, srv, _ := testutil.Setup(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "export.zip")
	_, _ = part.Write(archive.Bytes())
	_ = mw.Close()
	c := login(t, srv.Handler())
	req := httptest.NewRequest(http.MethodPost, "/api/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken(t, srv.Handler(), c))
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if n, _ := target.Count(); n != 1 {
		t.Fatalf("count=%d", n)
	}
}

func TestTimelineGeofenceAndGPXAPIs(t *testing.T) {
	_, st, _, _, srv, _ := testutil.Setup(t)
	h := srv.Handler()
	c := login(t, h)
	csrf := csrfToken(t, h, c)
	do := func(method, path, content string, body io.Reader) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, body)
		req.AddCookie(c)
		if method != http.MethodGet {
			req.Header.Set("X-CSRF-Token", csrf)
			req.Header.Set("Content-Type", content)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	created := do(http.MethodPost, "/api/geofences", "application/json", strings.NewReader(`{"name":"Home","lat":37,"lon":127,"radius_m":200,"enabled":true}`))
	if created.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	var fence store.Geofence
	if err := json.Unmarshal(created.Body.Bytes(), &fence); err != nil {
		t.Fatal(err)
	}
	if rec := do(http.MethodPost, "/api/geofences", "application/json", strings.NewReader(`{`)); rec.Code != 400 {
		t.Fatalf("bad geofence=%d", rec.Code)
	}
	if rec := do(http.MethodPost, "/api/geofences", "application/json", strings.NewReader(`{"name":"Bad","lat":1,"lon":2,"radius_m":1,"enabled":true}`)); rec.Code != 400 {
		t.Fatalf("invalid radius=%d", rec.Code)
	}
	if rec := do(http.MethodPatch, "/api/geofences/bad", "application/json", strings.NewReader(`{}`)); rec.Code != 400 {
		t.Fatalf("bad patch=%d", rec.Code)
	}
	if rec := do(http.MethodPatch, "/api/geofences/999", "application/json", strings.NewReader(`{"name":"Missing","lat":1,"lon":2,"radius_m":25,"enabled":true}`)); rec.Code != 404 {
		t.Fatalf("missing patch=%d", rec.Code)
	}
	if rec := do(http.MethodDelete, "/api/geofences/999", "application/x-www-form-urlencoded", nil); rec.Code != 404 {
		t.Fatalf("missing delete=%d", rec.Code)
	}
	if rec := do(http.MethodGet, "/api/geofences", "", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Home") {
		t.Fatalf("list=%d %s", rec.Code, rec.Body.String())
	}
	updated := do(http.MethodPatch, fmt.Sprintf("/api/geofences/%d", fence.ID), "application/json", strings.NewReader(`{"name":"Office","lat":37,"lon":127,"radius_m":300,"enabled":true}`))
	if updated.Code != 200 {
		t.Fatalf("patch=%d %s", updated.Code, updated.Body.String())
	}
	partial := do(http.MethodPatch, fmt.Sprintf("/api/geofences/%d", fence.ID), "application/json", strings.NewReader(`{"enabled":false}`))
	if partial.Code != 200 || strings.Contains(partial.Body.String(), `"name":"Home"`) || !strings.Contains(partial.Body.String(), `"name":"Office"`) || strings.Contains(partial.Body.String(), `"enabled":true`) {
		t.Fatalf("partial patch=%d %s", partial.Code, partial.Body.String())
	}
	if partial = do(http.MethodPatch, fmt.Sprintf("/api/geofences/%d", fence.ID), "application/json", strings.NewReader(`{"enabled":true}`)); partial.Code != 200 {
		t.Fatalf("partial restore=%d %s", partial.Code, partial.Body.String())
	}
	if partial = do(http.MethodPatch, fmt.Sprintf("/api/geofences/%d", fence.ID), "application/json", strings.NewReader(`{"enabled":null}`)); partial.Code != 400 {
		t.Fatalf("invalid partial patch=%d", partial.Code)
	}
	for i, lat := range []float64{37, 37.01} {
		loc := sampleWebLocation(t, int64(200+i))
		loc.Lat = lat
		loc.Lon = 127
		if _, err := st.Insert(loc); err != nil {
			t.Fatal(err)
		}
	}
	if rec := do(http.MethodGet, "/api/geofence-events?limit=10", "", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"event":"exit"`) {
		t.Fatalf("events=%d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodGet, "/api/timeline?limit=10", "", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"summary"`) {
		t.Fatalf("timeline=%d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodGet, "/timeline", "", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Trips and stays") {
		t.Fatalf("timeline page=%d", rec.Code)
	}
	if rec := do(http.MethodGet, "/api/timeline?from=1&to=999&user=u&device=d&limit=1", "", nil); rec.Code != 200 {
		t.Fatalf("timeline filter=%d", rec.Code)
	}
	for _, path := range []string{"/api/timeline?from=bad", "/api/timeline?from=2&to=1", "/api/timeline?cursor=bad"} {
		if rec := do(http.MethodGet, path, "", nil); rec.Code != 400 {
			t.Fatalf("invalid timeline %s=%d", path, rec.Code)
		}
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "track.gpx")
	_, _ = part.Write([]byte(`<gpx><trk><trkseg><trkpt lat="1" lon="2"><time>2026-01-01T00:00:00Z</time></trkpt></trkseg></trk></gpx>`))
	_ = mw.WriteField("user", "gpx")
	_ = mw.WriteField("device", "phone")
	_ = mw.Close()
	rec := do(http.MethodPost, "/api/import/gpx", mw.FormDataContentType(), &body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"imported":1`) {
		t.Fatalf("gpx import=%d %s", rec.Code, rec.Body.String())
	}
	var missing bytes.Buffer
	missingWriter := multipart.NewWriter(&missing)
	missingPart, _ := missingWriter.CreateFormFile("file", "track.gpx")
	_, _ = missingPart.Write([]byte(`<gpx><trkpt lat="1" lon="2"><time>2026-01-01T00:00:00Z</time></trkpt></gpx>`))
	_ = missingWriter.Close()
	if rec = do(http.MethodPost, "/api/import/gpx", missingWriter.FormDataContentType(), &missing); rec.Code != 400 {
		t.Fatalf("missing identity=%d", rec.Code)
	}
	var noFile bytes.Buffer
	noFileWriter := multipart.NewWriter(&noFile)
	_ = noFileWriter.WriteField("user", "u")
	_ = noFileWriter.WriteField("device", "d")
	_ = noFileWriter.Close()
	if rec = do(http.MethodPost, "/api/import/gpx", noFileWriter.FormDataContentType(), &noFile); rec.Code != 400 {
		t.Fatalf("missing file=%d", rec.Code)
	}
	var bad bytes.Buffer
	badWriter := multipart.NewWriter(&bad)
	badPart, _ := badWriter.CreateFormFile("file", "bad.gpx")
	_, _ = badPart.Write([]byte(`<gpx><trkpt lat="1" lon="2"/></gpx>`))
	_ = badWriter.WriteField("user", "u")
	_ = badWriter.WriteField("device", "d")
	_ = badWriter.Close()
	if rec = do(http.MethodPost, "/api/import/gpx", badWriter.FormDataContentType(), &bad); rec.Code != 400 {
		t.Fatalf("bad GPX=%d %s", rec.Code, rec.Body.String())
	}
	before, _ := st.Count()
	var duplicate bytes.Buffer
	duplicateWriter := multipart.NewWriter(&duplicate)
	duplicatePart, _ := duplicateWriter.CreateFormFile("file", "duplicate.gpx")
	_, _ = duplicatePart.Write([]byte(`<gpx><trkpt lat="1" lon="2"><time>2026-02-01T00:00:00Z</time></trkpt><trkpt lat="2" lon="3"><time>2026-02-01T00:00:00Z</time></trkpt></gpx>`))
	_ = duplicateWriter.WriteField("user", "dupe")
	_ = duplicateWriter.WriteField("device", "phone")
	_ = duplicateWriter.Close()
	if rec = do(http.MethodPost, "/api/import/gpx", duplicateWriter.FormDataContentType(), &duplicate); rec.Code != 400 {
		t.Fatalf("duplicate GPX=%d %s", rec.Code, rec.Body.String())
	}
	after, _ := st.Count()
	if after != before {
		t.Fatalf("partial GPX import: before=%d after=%d", before, after)
	}
	if rec = do(http.MethodGet, "/api/export/gpx?user=gpx&device=phone", "", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "<trkpt") {
		t.Fatalf("gpx export=%d %s", rec.Code, rec.Body.String())
	}
	if rec = do(http.MethodDelete, fmt.Sprintf("/api/geofences/%d", fence.ID), "application/x-www-form-urlencoded", nil); rec.Code != 200 {
		t.Fatalf("delete=%d %s", rec.Code, rec.Body.String())
	}
}

func sampleWebLocation(t *testing.T, tst int64) *store.Location {
	t.Helper()
	payload := []byte(fmt.Sprintf(`{"_type":"location","lat":37,"lon":127,"tst":%d}`, tst))
	var raw map[string]any
	_ = json.Unmarshal(payload, &raw)
	can, err := canonical.FromLocation("u", "d", raw)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := can.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return &store.Location{User: "u", Device: "d", Tst: tst, Lat: 37, Lon: 127, Payload: payload, Digest: digest[:]}
}

func TestVerifyAPIWithInvalidProofFiles(t *testing.T) {
	cfg, st, _, _, srv, _ := testutil.Setup(t)
	payload := []byte(`{"_type":"location","lat":1,"lon":2,"tst":101}`)
	var raw map[string]any
	_ = json.Unmarshal(payload, &raw)
	can, _ := canonical.FromLocation("u", "d", raw)
	digest, _ := can.Digest()
	id, err := st.Insert(&store.Location{
		User: "u", Device: "d", Tst: 101, Lat: 1, Lon: 2,
		Payload: payload, Digest: digest[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{".tsr", ".ots"} {
		if err := os.WriteFile(filepath.Join(cfg.ProofsDir(), strconv.FormatInt(id, 10)+ext), []byte("invalid"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	rec := authed(t, srv.Handler(), http.MethodPost, "/api/verify/"+strconv.FormatInt(id, 10), nil)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"jobs"`) {
		t.Fatalf("verify=%d %s", rec.Code, rec.Body.String())
	}
	waitVerificationResult(t, st, id, "tsa", "invalid")
	waitVerificationResult(t, st, id, "ots", "invalid")
	rec = authed(t, srv.Handler(), http.MethodPost, "/api/verify/"+strconv.FormatInt(id, 10),
		strings.NewReader(`{}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("body accepted=%d %s", rec.Code, rec.Body.String())
	}
}

func waitVerificationResult(t *testing.T, st *store.Store, id int64, kind, result string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		run, err := st.LatestVerification(id, kind)
		if err == nil && run.Result == result {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, err := st.LatestVerification(id, kind)
	t.Fatalf("%s verification=%+v err=%v", kind, run, err)
}

func TestOTSUpgradeAndFilteredRoutes(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	if rec := authed(t, srv.Handler(), http.MethodPost, "/api/ots/upgrade", nil); rec.Code != http.StatusAccepted {
		t.Fatalf("upgrade=%d", rec.Code)
	}
	for _, path := range []string{
		"/api/track?from=1&to=2&limit=1",
		"/api/export?from=1&to=2",
		"/api/locations?limit=1",
	} {
		if rec := authed(t, srv.Handler(), http.MethodGet, path, nil); rec.Code != http.StatusOK {
			t.Fatalf("%s=%d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestLocationRoutesRejectInvalidRanges(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	for _, path := range []string{
		"/api/track?from=bad",
		"/api/track?from=20&to=10",
		"/api/track?hours=999999999999",
		"/api/track?limit=bad",
		"/api/export?to=bad",
		"/api/export/gpx?from=bad",
	} {
		rec := authed(t, srv.Handler(), http.MethodGet, path, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: code=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestAdminAPIErrorResponses(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	cases := []struct {
		method string
		path   string
		code   int
	}{
		{http.MethodGet, "/api/verify/bad", http.StatusBadRequest},
		{http.MethodGet, "/api/verify/999", http.StatusNotFound},
		{http.MethodPost, "/api/stamps/bad/retry", http.StatusBadRequest},
		{http.MethodPost, "/api/stamps/999/retry", http.StatusConflict},
		{http.MethodDelete, "/api/shares/bad", http.StatusBadRequest},
		{http.MethodDelete, "/api/shares/999", http.StatusNotFound},
		{http.MethodGet, "/api/delete-preview?from=bad", http.StatusBadRequest},
		{http.MethodPost, "/api/delete", http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec := authed(t, srv.Handler(), tc.method, tc.path, nil)
		if rec.Code != tc.code {
			t.Fatalf("%s %s: got=%d want=%d body=%s", tc.method, tc.path, rec.Code, tc.code, rec.Body.String())
		}
	}
}

func TestVerifyPayloadMismatchAndLoggedInLoginPage(t *testing.T) {
	_, st, _, _, srv, _ := testutil.Setup(t)
	id, err := st.Insert(&store.Location{
		User: "u", Device: "d", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{"_type":"location","lat":1,"lon":2,"tst":1}`),
		Digest:  make([]byte, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := authed(t, srv.Handler(), http.MethodGet, "/api/verify/"+strconv.FormatInt(id, 10), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"payload_ok":false`) {
		t.Fatalf("verify=%d %s", rec.Code, rec.Body.String())
	}
	c := login(t, srv.Handler())
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.AddCookie(c)
	page := httptest.NewRecorder()
	srv.Handler().ServeHTTP(page, req)
	if page.Code != http.StatusSeeOther || page.Header().Get("Location") != "/" {
		t.Fatalf("login page=%d", page.Code)
	}
}

func TestRetryReturnsJobState(t *testing.T) {
	_, st, _, _, srv, _ := testutil.Setup(t)
	id, err := st.Insert(&store.Location{
		User: "a", Device: "p", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: []byte{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTSA(id, "fail", nil); err != nil {
		t.Fatal(err)
	}
	rec := authed(t, srv.Handler(), http.MethodPost, "/api/stamps/"+strconv.FormatInt(id, 10)+"/retry", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"location_id":`+strconv.FormatInt(id, 10)) {
		t.Fatalf("retry=%d %s", rec.Code, rec.Body.String())
	}
}

func TestPostRequiresCSRF(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	c := login(t, srv.Handler())
	req := httptest.NewRequest(http.MethodPost, "/api/ots/upgrade", nil)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d", rec.Code)
	}
	other := login(t, srv.Handler())
	for _, test := range []struct {
		token string
		code  int
	}{
		{csrfToken(t, srv.Handler(), other), http.StatusForbidden},
		{csrfToken(t, srv.Handler(), c), http.StatusAccepted},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/ots/upgrade", nil)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", test.token)
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != test.code {
			t.Fatalf("code=%d want=%d", rec.Code, test.code)
		}
	}
}

func TestSecureCookieBehindProxy(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	form := url.Values{"password": {testutil.AdminPassword}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "session" {
			if !c.Secure {
				t.Fatal("session cookie is not secure")
			}
			return
		}
	}
	t.Fatal("no session cookie")
}

func TestUntrustedProxyCannotSetSecureCookie(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	form := url.Values{"password": {testutil.AdminPassword}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.RemoteAddr = "198.51.100.10:12345"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "session" && c.Secure {
			t.Fatal("untrusted proxy set secure cookie")
		}
	}
}

func TestVerifyAPIRequiresAuth(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	req := httptest.NewRequest(http.MethodGet, "/api/verify/1", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"request_id"`) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestShareLinkPublicAccess(t *testing.T) {
	cfg, st, _, ot, srv, _ := testutil.Setup(t)
	tst := time.Now().Unix()
	body := fmt.Sprintf(`{"_type":"location","lat":37.5,"lon":127.0,"tst":%d}`, tst)
	req := httptest.NewRequest(http.MethodPost, "/pub", strings.NewReader(body))
	req.SetBasicAuth(cfg.PubUser, cfg.PubPass)
	ot.ServeHTTP(httptest.NewRecorder(), req)

	link := &store.ShareLink{Label: "family", Hours: 24}
	id, err := st.CreateShare(link)
	if err != nil {
		t.Fatal(err)
	}
	token := link.Token

	page := httptest.NewRecorder()
	srv.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/share/"+token, nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "family") {
		t.Fatalf("page code=%d", page.Code)
	}

	track := httptest.NewRecorder()
	srv.Handler().ServeHTTP(track, httptest.NewRequest(http.MethodGet, "/share/"+token+"/api/track", nil))
	if track.Code != http.StatusOK {
		t.Fatalf("track code=%d", track.Code)
	}
	var fc struct {
		Features []map[string]any `json:"features"`
	}
	if err := json.NewDecoder(track.Body).Decode(&fc); err != nil {
		t.Fatal(err)
	}
	if len(fc.Features) < 2 {
		t.Fatalf("features=%d", len(fc.Features))
	}

	if err := st.DeleteShare(id); err != nil {
		t.Fatal(err)
	}
	gone := httptest.NewRecorder()
	srv.Handler().ServeHTTP(gone, httptest.NewRequest(http.MethodGet, "/share/"+token, nil))
	if gone.Code != http.StatusNotFound {
		t.Fatalf("code=%d", gone.Code)
	}
}

func TestShareDelayAndPrecision(t *testing.T) {
	_, st, _, _, srv, _ := testutil.Setup(t)
	for i, tst := range []int64{time.Now().Add(-2 * time.Hour).Unix(), time.Now().Unix()} {
		if _, err := st.Insert(&store.Location{
			User: "a", Device: "p", Tst: tst,
			Lat: 37.5665123 + float64(i)/100, Lon: 126.9780123,
			Payload: []byte(`{}`), Digest: []byte{1, 2, byte(i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	link := &store.ShareLink{User: "a", Device: "p", Hours: 24, DelayMinutes: 60, PrecisionM: 1000}
	if _, err := st.CreateShare(link); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/share/"+link.Token+"/api/track", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var fc struct {
		Features []struct {
			Geometry struct {
				Coordinates json.RawMessage `json:"coordinates"`
			} `json:"geometry"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fc); err != nil {
		t.Fatal(err)
	}
	if len(fc.Features) != 2 {
		t.Fatalf("features=%d", len(fc.Features))
	}
	if strings.Contains(string(fc.Features[1].Geometry.Coordinates), "37.5665123") {
		t.Fatal("exact coordinate leaked")
	}
}

func TestReadyAndCursorLocations(t *testing.T) {
	_, st, stampSvc, _, srv, _ := testutil.Setup(t)
	for i := range 3 {
		if _, err := st.Insert(&store.Location{
			User: "a", Device: "p", Tst: int64(i + 1), Lat: 1, Lon: 2,
			Payload: []byte(`{}`), Digest: []byte{byte(i + 1)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	ready := httptest.NewRecorder()
	srv.Handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("ready code=%d body=%s", ready.Code, ready.Body.String())
	}
	rec := authed(t, srv.Handler(), http.MethodGet, "/api/locations?limit=2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("locations code=%d body=%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Data       []store.Location `json:"data"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 2 || page.NextCursor == "" {
		t.Fatalf("page=%+v", page)
	}
	next := authed(t, srv.Handler(), http.MethodGet, "/api/locations?limit=2&cursor="+url.QueryEscape(page.NextCursor), nil)
	if next.Code != http.StatusOK {
		t.Fatalf("next code=%d body=%s", next.Code, next.Body.String())
	}
	now := time.Now().Unix()
	stampSvc.Shutdown()
	if _, _, err := st.EnqueueVerification(1, "tsa", "manual", now); err != nil {
		t.Fatal(err)
	}
	if job, err := st.ClaimVerificationJob(now, "expired", now-1); err != nil || job == nil {
		t.Fatalf("claim expired job=%+v err=%v", job, err)
	}
	notReady := httptest.NewRecorder()
	srv.Handler().ServeHTTP(notReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if notReady.Code != http.StatusServiceUnavailable || !strings.Contains(notReady.Body.String(), `"verification_leases":false`) {
		t.Fatalf("not ready code=%d body=%s", notReady.Code, notReady.Body.String())
	}
}

func TestShareCreateRequiresAuth(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	req := httptest.NewRequest(http.MethodPost, "/api/shares", strings.NewReader("label=family&hours=24"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth code=%d", rec.Code)
	}
	c := login(t, srv.Handler())
	form := url.Values{"label": {"family"}, "hours": {"24"}, "expires_days": {"7"}}
	req = httptest.NewRequest(http.MethodPost, "/api/shares", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrfToken(t, srv.Handler(), c))
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestShareCreateJSONPreservesFilters(t *testing.T) {
	_, st, _, _, srv, _ := testutil.Setup(t)
	c := login(t, srv.Handler())
	body := `{"label":"family","user":"alice","device":"phone","hours":48,"expires_days":3,"delay_minutes":30,"precision_m":500}`
	for _, contentType := range []string{"application/json", "Application/JSON; charset=UTF-8"} {
		req := httptest.NewRequest(http.MethodPost, "/api/shares", strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("X-CSRF-Token", csrfToken(t, srv.Handler(), c))
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("content-type=%q code=%d body=%s", contentType, rec.Code, rec.Body.String())
		}
	}
	shares, err := st.ListShares()
	if err != nil || len(shares) != 2 {
		t.Fatalf("shares=%+v err=%v", shares, err)
	}
	for _, share := range shares {
		if share.Label != "family" || share.User != "alice" || share.Device != "phone" || share.Hours != 48 || share.DelayMinutes != 30 || share.PrecisionM != 500 {
			t.Fatalf("share=%+v", share)
		}
	}
}

func TestLoginDoesNotReadPasswordFromQuery(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	req := httptest.NewRequest(http.MethodPost, "/login?password="+url.QueryEscape(testutil.AdminPassword), strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusSeeOther {
		t.Fatalf("query password authenticated: headers=%v", rec.Header())
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "session" && cookie.Value != "" {
			t.Fatal("query password created a session")
		}
	}
}

func TestHealthz(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !out.OK {
		t.Fatalf("body=%q", rec.Body.String())
	}
}

func TestShareListAndDelete(t *testing.T) {
	_, st, _, _, srv, _ := testutil.Setup(t)
	id, err := st.CreateShare(&store.ShareLink{Label: "x", Hours: 12})
	if err != nil {
		t.Fatal(err)
	}
	c := login(t, srv.Handler())

	list := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/shares", nil)
	req.AddCookie(c)
	srv.Handler().ServeHTTP(list, req)
	if list.Code != http.StatusOK {
		t.Fatalf("list code=%d", list.Code)
	}
	var rows []map[string]any
	if err := json.NewDecoder(list.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	if _, ok := rows[0]["url"]; ok {
		t.Fatal("share URL leaked from list")
	}
	if _, ok := rows[0]["token"]; ok {
		t.Fatal("share token leaked from list")
	}

	rotate := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/shares/"+strconv.FormatInt(id, 10)+"/rotate",
		strings.NewReader("csrf="+csrfToken(t, srv.Handler(), c)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrfToken(t, srv.Handler(), c))
	req.AddCookie(c)
	srv.Handler().ServeHTTP(rotate, req)
	if rotate.Code != http.StatusOK || !strings.Contains(rotate.Body.String(), `"url"`) ||
		strings.Contains(rotate.Body.String(), `"token"`) {
		t.Fatalf("rotate code=%d body=%s", rotate.Code, rotate.Body.String())
	}

	del := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/shares/"+strconv.FormatInt(id, 10), nil)
	req.Header.Set("X-CSRF-Token", csrfToken(t, srv.Handler(), c))
	req.AddCookie(c)
	srv.Handler().ServeHTTP(del, req)
	if del.Code != http.StatusOK {
		t.Fatalf("delete code=%d", del.Code)
	}
}

func TestSessionsAPI(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	first := login(t, srv.Handler())
	_ = login(t, srv.Handler())
	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.AddCookie(first)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"current":true`) {
		t.Fatalf("sessions code=%d body=%s", rec.Code, rec.Body.String())
	}
	var sessions []struct {
		ID      int64 `json:"id"`
		Current bool  `json:"current"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sessions); err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if session.Current {
			continue
		}
		req = httptest.NewRequest(http.MethodDelete, "/api/sessions/"+strconv.FormatInt(session.ID, 10), nil)
		req.Header.Set("X-CSRF-Token", csrfToken(t, srv.Handler(), first))
		req.AddCookie(first)
		rec = httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("delete session code=%d", rec.Code)
		}
	}
	second := login(t, srv.Handler())
	req = httptest.NewRequest(http.MethodPost, "/api/sessions/revoke-others", strings.NewReader("csrf="+csrfToken(t, srv.Handler(), first)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrfToken(t, srv.Handler(), first))
	req.AddCookie(first)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke code=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	req.AddCookie(second)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("second session code=%d", rec.Code)
	}
}

func TestSecurityAPIContracts(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	c := login(t, srv.Handler())
	csrf := csrfToken(t, srv.Handler(), c)
	call := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		body := ""
		if form != nil {
			body = form.Encode()
		}
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", csrf)
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	risky := url.Values{"precision_m": {"0"}, "expires_days": {"0"}}
	if rec := call(http.MethodPost, "/api/shares", risky); rec.Code != http.StatusBadRequest {
		t.Fatalf("risk confirmation code=%d", rec.Code)
	}
	risky.Set("confirm_risk", "1")
	rec := call(http.MethodPost, "/api/shares", risky)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("share response=%d %s", rec.Code, rec.Body.String())
	}
	for _, tc := range []struct {
		method string
		path   string
		code   int
	}{
		{http.MethodPost, "/api/shares/bad/rotate", http.StatusBadRequest},
		{http.MethodPost, "/api/shares/999/rotate", http.StatusNotFound},
		{http.MethodDelete, "/api/sessions/bad", http.StatusBadRequest},
		{http.MethodDelete, "/api/sessions/999", http.StatusNotFound},
	} {
		if got := call(tc.method, tc.path, nil); got.Code != tc.code {
			t.Fatalf("%s code=%d body=%s", tc.path, got.Code, got.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/backups", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "text/plain")
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type code=%d", rec.Code)
	}
}

func TestLoginBodyAndPasswordLimits(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	form := url.Values{"password": {strings.Repeat("x", 257)}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "login failed") {
		t.Fatalf("password limit code=%d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(strings.Repeat("x", loginBodySizeForTest)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("body limit code=%d", rec.Code)
	}
}

const loginBodySizeForTest = 9 << 10

func TestStampRetryAll(t *testing.T) {
	_, st, stampSvc, _, srv, _ := testutil.Setup(t)
	stampSvc.Shutdown()
	id, err := st.Insert(&store.Location{
		User: "a", Device: "p", Tst: time.Now().UnixNano(), Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: []byte{1, 2, 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTSA(id, "fail", nil); err != nil {
		t.Fatal(err)
	}
	rec := authed(t, srv.Handler(), http.MethodPost, "/api/stamps/retry", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data struct {
			Retried int `json:"retried"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.Retried < 1 {
		t.Fatalf("retried=%d", out.Data.Retried)
	}
}

func TestStampRetry(t *testing.T) {
	_, st, stampSvc, _, srv, _ := testutil.Setup(t)
	stampSvc.Shutdown()
	id, err := st.Insert(&store.Location{
		User: "a", Device: "p", Tst: time.Now().UnixNano(), Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: []byte{1, 2, 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTSA(id, "fail", nil); err != nil {
		t.Fatal(err)
	}
	c := login(t, srv.Handler())
	req := httptest.NewRequest(http.MethodPost, "/api/stamps/"+strconv.FormatInt(id, 10)+"/retry", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrfToken(t, srv.Handler(), c))
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	loc, _ := st.Get(id)
	if loc.TsaStatus != "pending" {
		t.Fatalf("status=%q", loc.TsaStatus)
	}
}

func TestPasswordChangeInvalidatesSession(t *testing.T) {
	_, _, _, _, srv, _ := testutil.Setup(t)
	c := login(t, srv.Handler())
	form := url.Values{
		"csrf":          {csrfToken(t, srv.Handler(), c)},
		"new_password":  {"new-admin-pass"},
		"pub_user":      {"owntracks"},
		"tsa_url":       {"http://127.0.0.1:1/nope"},
		"ots_calendars": {"http://127.0.0.1:1/nope"},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save code=%d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatal("old session should be invalid")
	}
}
