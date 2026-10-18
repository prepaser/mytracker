package web_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/testutil"
)

type settingsAPIResponse struct {
	Data struct {
		Redirect string `json:"redirect"`
	} `json:"data"`
	Error struct {
		Code             string `json:"code"`
		Message          string `json:"message"`
		SettingsRevision string `json:"settings_revision"`
		Committed        bool   `json:"committed"`
	} `json:"error"`
}

func TestAPISettingsRequiresAuthAndCSRF(t *testing.T) {
	_, _, _, _, server, _ := testutil.Setup(t)
	handler := server.Handler()

	rec := apiSettingsRequest(handler, nil, url.Values{"settings_revision": {"missing"}})
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"unauthorized"`) {
		t.Fatalf("unauthenticated response=%d %s", rec.Code, rec.Body.String())
	}

	cookie := login(t, handler)
	rec = apiSettingsRequest(handler, cookie, url.Values{"settings_revision": {"missing"}})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"code":"forbidden"`) {
		t.Fatalf("csrf response=%d %s", rec.Code, rec.Body.String())
	}
}

func TestAPISettingsSaveSuccess(t *testing.T) {
	_, _, _, _, server, _ := testutil.Setup(t)
	handler := server.Handler()
	cookie := login(t, handler)
	cfg, revision := server.Cfg.SnapshotWithRevision()
	form := settingsRetryForm(csrfToken(t, handler, cookie), revision, cfg.TSAURL)

	rec := apiSettingsRequest(handler, cookie, form)
	var response settingsAPIResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || response.Data.Redirect != "/settings?saved=1#tracking" {
		t.Fatalf("response=%d %+v", rec.Code, response)
	}
	if got := server.Cfg.Snapshot().StayRadiusM; got != 225 {
		t.Fatalf("radius=%d", got)
	}
}

func TestAPISettingsValidationRetainsSubmittedRevision(t *testing.T) {
	_, _, _, _, server, _ := testutil.Setup(t)
	handler := server.Handler()
	cookie := login(t, handler)
	cfg, revision := server.Cfg.SnapshotWithRevision()
	form := settingsRetryForm(csrfToken(t, handler, cookie), revision, cfg.TSAURL)
	form.Set("backup_keep", "invalid")

	rec := apiSettingsRequest(handler, cookie, form)
	var response settingsAPIResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest || response.Error.Code != "bad_request" {
		t.Fatalf("response=%d %+v", rec.Code, response)
	}
	if response.Error.SettingsRevision != revision {
		t.Fatalf("submitted revision=%q returned=%q", revision, response.Error.SettingsRevision)
	}
	if got := server.Cfg.Snapshot().BackupKeep; got != cfg.BackupKeep {
		t.Fatalf("backup keep changed to %d", got)
	}
}

func TestAPISettingsRevisionConflictRetainsSubmittedRevision(t *testing.T) {
	_, _, _, _, server, _ := testutil.Setup(t)
	handler := server.Handler()
	cookie := login(t, handler)
	cfg, revision := server.Cfg.SnapshotWithRevision()
	if _, err := server.Cfg.Update(func(next *config.Settings) error {
		next.TileURL = "https://concurrent.example/{z}/{x}/{y}.png"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	form := settingsRetryForm(csrfToken(t, handler, cookie), revision, cfg.TSAURL)

	rec := apiSettingsRequest(handler, cookie, form)
	var response settingsAPIResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict || response.Error.Code != "conflict" {
		t.Fatalf("response=%d %+v", rec.Code, response)
	}
	if response.Error.SettingsRevision != revision {
		t.Fatalf("submitted revision=%q returned=%q", revision, response.Error.SettingsRevision)
	}
	if got := server.Cfg.Snapshot().TileURL; got != "https://concurrent.example/{z}/{x}/{y}.png" {
		t.Fatalf("concurrent settings overwritten: %q", got)
	}
}

func TestAPISettingsRetryUsesCommittedRevisionAfterTimelineFailure(t *testing.T) {
	_, _, _, _, server, _ := testutil.Setup(t)
	reload := &settingsRetryTimeline{}
	server.Timeline = reload
	handler := server.Handler()
	cookie := login(t, handler)
	cfg, revision := server.Cfg.SnapshotWithRevision()
	form := settingsRetryForm(csrfToken(t, handler, cookie), revision, cfg.TSAURL)

	first := apiSettingsRequest(handler, cookie, form)
	var response settingsAPIResponse
	if err := json.NewDecoder(first.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if first.Code != http.StatusInternalServerError || !response.Error.Committed {
		t.Fatalf("first response=%d %+v", first.Code, response)
	}
	_, committedRevision := server.Cfg.SnapshotWithRevision()
	if response.Error.SettingsRevision != committedRevision {
		t.Fatalf("returned revision=%q committed=%q", response.Error.SettingsRevision, committedRevision)
	}

	form.Set("settings_revision", response.Error.SettingsRevision)
	second := apiSettingsRequest(handler, cookie, form)
	if second.Code != http.StatusOK || reload.calls != 2 {
		t.Fatalf("retry status=%d reload_calls=%d", second.Code, reload.calls)
	}
}

func TestAPISettingsSaveFailureIsGenericAndDoesNotCommit(t *testing.T) {
	_, st, _, _, server, _ := testutil.Setup(t)
	handler := server.Handler()
	cookie := login(t, handler)
	csrf := csrfToken(t, handler, cookie)
	before, revision := server.Cfg.SnapshotWithRevision()
	server.Cfg = config.NewManager(before, &settingsAPISaveFailureStore{
		SettingsStore: st,
		err:           errors.New("synthetic driver failure"),
	})
	form := settingsRetryForm(csrf, revision, before.TSAURL)

	rec := apiSettingsRequest(handler, cookie, form)
	var response settingsAPIResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError || response.Error.Code != "internal_error" || response.Error.Message != "internal error" {
		t.Fatalf("response=%d %+v", rec.Code, response)
	}
	after, currentRevision := server.Cfg.SnapshotWithRevision()
	if currentRevision != revision || after.StayRadiusM != before.StayRadiusM {
		t.Fatalf("settings committed after storage failure: revision=%q radius=%d", currentRevision, after.StayRadiusM)
	}
}

type settingsAPISaveFailureStore struct {
	config.SettingsStore
	err error
}

func (s *settingsAPISaveFailureStore) SaveSettings([]byte, bool) error {
	return s.err
}

func apiSettingsRequest(handler http.Handler, cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
