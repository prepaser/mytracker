package owntracks_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/owntracks"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/testutil"
)

const locationJSON = `{"_type":"location","lat":37.5665,"lon":126.978,"tst":1751800000,"tid":"ab"}`

type noopStamp struct{}

func (noopStamp) Process(int64, [32]byte) {}

func setup(t *testing.T) (*config.Config, *store.Store, *owntracks.Handler) {
	t.Helper()
	cfg, st, _, h, _, _ := testutil.Setup(t)
	h.Stamp = noopStamp{}
	return cfg, st, h
}

func TestHandlerUnauthorized(t *testing.T) {
	_, _, h := setup(t)
	req := httptest.NewRequest(http.MethodPost, "/pub", strings.NewReader(locationJSON))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestHandlerRejectsEmptyConfiguredCredentials(t *testing.T) {
	_, st, _ := setup(t)
	cfg := config.Default()
	cfg.PubUser = ""
	cfg.PubPass = ""
	h := owntracks.New(st, noopStamp{}, config.NewManager(cfg, nil))
	req := httptest.NewRequest(http.MethodPost, "/pub", strings.NewReader(locationJSON))
	req.SetBasicAuth("", "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestHandlerRateLimit(t *testing.T) {
	_, _, h := setup(t)
	var rec *httptest.ResponseRecorder
	for range 121 {
		req := httptest.NewRequest(http.MethodPost, "/pub", strings.NewReader(locationJSON))
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing retry-after")
	}
}

func TestHandlerAcceptsLocation(t *testing.T) {
	cfg, st, h := setup(t)
	req := httptest.NewRequest(http.MethodPost, "/pub?u=me&d=phone", bytes.NewBufferString(locationJSON))
	req.SetBasicAuth(cfg.PubUser, cfg.PubPass)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "[]" {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if n, _ := st.Count(); n != 1 {
		t.Fatalf("count=%d", n)
	}
}

func TestHandlerDuplicateIsOK(t *testing.T) {
	cfg, st, h := setup(t)
	post := func() {
		req := httptest.NewRequest(http.MethodPost, "/pub", bytes.NewBufferString(locationJSON))
		req.SetBasicAuth(cfg.PubUser, cfg.PubPass)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d", rec.Code)
		}
	}
	post()
	post()
	if n, _ := st.Count(); n != 1 {
		t.Fatalf("count=%d want 1", n)
	}
}

func TestHandlerIgnoresNonLocation(t *testing.T) {
	cfg, st, h := setup(t)
	req := httptest.NewRequest(http.MethodPost, "/pub", strings.NewReader(`{"_type":"status"}`))
	req.SetBasicAuth(cfg.PubUser, cfg.PubPass)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "[]" {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if n, _ := st.Count(); n != 0 {
		t.Fatalf("count=%d", n)
	}
}

func TestHandlerIdentFromHeaders(t *testing.T) {
	cfg, st, h := setup(t)
	req := httptest.NewRequest(http.MethodPost, "/pub", bytes.NewBufferString(locationJSON))
	req.SetBasicAuth(cfg.PubUser, cfg.PubPass)
	req.Header.Set("X-Limit-U", "header-user")
	req.Header.Set("X-Limit-D", "header-device")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	loc, _ := st.List(1, 0)
	if len(loc) != 1 || loc[0].User != "header-user" || loc[0].Device != "header-device" {
		t.Fatalf("ident: %+v", loc)
	}
}

func TestHandlerTransitionIdentityPrecedence(t *testing.T) {
	cfg, st, h := setup(t)
	body := `{"_type":"transition","topic":"owntracks/topic-user/topic-device","t":1751800000,"lat":37.5,"lon":127}`
	req := httptest.NewRequest(http.MethodPost, "/pub?u=query-user", strings.NewReader(body))
	req.SetBasicAuth(cfg.PubUser, cfg.PubPass)
	req.Header.Set("X-Limit-U", "header-user")
	req.Header.Set("X-Limit-D", "header-device")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "[]" {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	locs, err := st.List(1, 0)
	if err != nil || len(locs) != 1 {
		t.Fatalf("locations=%+v err=%v", locs, err)
	}
	loc := locs[0]
	if loc.User != "query-user" || loc.Device != "header-device" || loc.Tst != 1751800000 || loc.Lat != 37.5 || loc.Lon != 127 {
		t.Fatalf("location=%+v", loc)
	}
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	_, _, h := setup(t)
	req := httptest.NewRequest(http.MethodGet, "/pub", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestHandlerRejectsBadJSONAndAcceptsEmptyBody(t *testing.T) {
	cfg, _, h := setup(t)
	for _, tc := range []struct {
		body string
		code int
	}{
		{"{", http.StatusBadRequest},
		{"", http.StatusOK},
		{"null", http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodPost, "/pub", strings.NewReader(tc.body))
		req.SetBasicAuth(cfg.PubUser, cfg.PubPass)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.code {
			t.Fatalf("body=%q code=%d", tc.body, rec.Code)
		}
	}
}

func TestHandlerRejectsBodyReadFailure(t *testing.T) {
	cfg, st, h := setup(t)
	body := io.MultiReader(strings.NewReader(locationJSON), iotest.ErrReader(io.ErrUnexpectedEOF))
	req := httptest.NewRequest(http.MethodPost, "/pub", body)
	req.SetBasicAuth(cfg.PubUser, cfg.PubPass)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if n, err := st.Count(); err != nil || n != 0 {
		t.Fatalf("stored=%d err=%v", n, err)
	}
}

func TestHandlerRejectsInvalidLocationAsBadRequest(t *testing.T) {
	cfg, st, h := setup(t)
	for _, body := range []string{
		`{"_type":"location","lon":126.978,"tst":1751800000}`,
		`{"_type":"location","lat":91,"lon":126.978,"tst":1751800000}`,
		`{"_type":"location","lat":37.5,"lon":126.978,"tst":-1}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/pub", strings.NewReader(body))
		req.SetBasicAuth(cfg.PubUser, cfg.PubPass)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s code=%d response=%q", body, rec.Code, rec.Body.String())
		}
	}
	if n, err := st.Count(); err != nil || n != 0 {
		t.Fatalf("count=%d err=%v", n, err)
	}
}

func TestHandlerRejectsOversizedBody(t *testing.T) {
	cfg, st, h := setup(t)
	body := locationJSON + strings.Repeat(" ", (1<<20)-len(locationJSON)) + "x"
	req := httptest.NewRequest(http.MethodPost, "/pub", strings.NewReader(body))
	req.SetBasicAuth(cfg.PubUser, cfg.PubPass)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if n, _ := st.Count(); n != 0 {
		t.Fatalf("stored=%d", n)
	}
}

func TestReloadAndIdentityRateLimit(t *testing.T) {
	cfg, _, h := setup(t)
	cfg.PubRateLimit = 1
	h.ReloadLimits(cfg)
	if !h.AllowIngest(" a ", " p ") {
		t.Fatal("first ingest denied")
	}
	if h.AllowIngest("a", "p") {
		t.Fatal("second ingest should be limited")
	}
}

func TestIdentityRateLimitNormalizesBlankComponent(t *testing.T) {
	cfg, _, h := setup(t)
	cfg.PubRateLimit = 1
	h.ReloadLimits(cfg)
	if !h.AllowIngest(" ", "p") {
		t.Fatal("first blank identity denied")
	}
	if h.AllowIngest("", "p") {
		t.Fatal("blank identity used a different quota")
	}
}
