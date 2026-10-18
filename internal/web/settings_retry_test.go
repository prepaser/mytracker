package web_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/testutil"
)

type settingsRetryTimeline struct {
	calls  int
	onLoad func()
}

func (r *settingsRetryTimeline) Reload() error {
	r.calls++
	if r.calls == 1 {
		if r.onLoad != nil {
			r.onLoad()
		}
		return errors.New("transient enqueue failure")
	}
	return nil
}

func (*settingsRetryTimeline) Wake()         {}
func (*settingsRetryTimeline) Healthy() bool { return true }

func TestSettingsRetryUsesCommittedRevisionAfterTimelineFailure(t *testing.T) {
	_, _, _, _, server, _ := testutil.Setup(t)
	reload := &settingsRetryTimeline{}
	server.Timeline = reload
	handler := server.Handler()
	cookie := login(t, handler)
	csrf := csrfToken(t, handler, cookie)
	cfg, revision := server.Cfg.SnapshotWithRevision()
	form := settingsRetryForm(csrf, revision, cfg.TSAURL)
	save := settingsRetrySave(handler, cookie, form)

	first := save()
	if first.Code != http.StatusInternalServerError || server.Cfg.Snapshot().StayRadiusM != 225 {
		t.Fatalf("status=%d radius=%d", first.Code, server.Cfg.Snapshot().StayRadiusM)
	}
	returned := settingsRetryRevision(t, first.Body.String())
	_, current := server.Cfg.SnapshotWithRevision()
	if returned != current {
		t.Fatalf("returned revision=%q current=%q", returned, current)
	}
	form.Set("settings_revision", returned)
	second := save()
	if second.Code != http.StatusSeeOther || reload.calls != 2 {
		t.Fatalf("retry status=%d reload_calls=%d", second.Code, reload.calls)
	}
}

func TestSettingsRetryPreservesConflictAfterConcurrentSave(t *testing.T) {
	_, _, _, _, server, _ := testutil.Setup(t)
	reload := &settingsRetryTimeline{}
	reload.onLoad = func() {
		if _, err := server.Cfg.Update(func(next *config.Settings) error {
			next.TileURL = "https://concurrent.example/{z}/{x}/{y}.png"
			return nil
		}); err != nil {
			t.Fatalf("concurrent settings update: %v", err)
		}
	}
	server.Timeline = reload
	handler := server.Handler()
	cookie := login(t, handler)
	csrf := csrfToken(t, handler, cookie)
	cfg, revision := server.Cfg.SnapshotWithRevision()
	form := settingsRetryForm(csrf, revision, cfg.TSAURL)
	save := settingsRetrySave(handler, cookie, form)

	first := save()
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first status=%d", first.Code)
	}
	returned := settingsRetryRevision(t, first.Body.String())
	_, current := server.Cfg.SnapshotWithRevision()
	if returned == current {
		t.Fatal("post-commit response used the concurrent save's revision")
	}
	form.Set("settings_revision", returned)
	second := save()
	if second.Code != http.StatusConflict || reload.calls != 1 {
		t.Fatalf("retry status=%d reload_calls=%d", second.Code, reload.calls)
	}
	if got := server.Cfg.Snapshot().TileURL; got != "https://concurrent.example/{z}/{x}/{y}.png" {
		t.Fatalf("concurrent settings overwritten: %q", got)
	}
}

func settingsRetryForm(csrf, revision, tsaURL string) url.Values {
	return url.Values{
		"csrf": {csrf}, "settings_revision": {revision}, "active_tab": {"tracking"},
		"tsa_url": {tsaURL}, "stay_radius_m": {"225"},
	}
}

func settingsRetrySave(handler http.Handler, cookie *http.Cookie, form url.Values) func() *httptest.ResponseRecorder {
	return func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out
	}
}

func settingsRetryRevision(t *testing.T, body string) string {
	t.Helper()
	match := regexp.MustCompile(`name="settings_revision" value="([^"]+)"`).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("no returned settings revision")
	}
	return match[1]
}
