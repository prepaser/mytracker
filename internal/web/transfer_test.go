package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/testutil"
)

type downloadCheckpointWriter struct {
	*httptest.ResponseRecorder
	beforeWrite func()
}

func (w *downloadCheckpointWriter) Write(p []byte) (int, error) {
	if w.beforeWrite != nil {
		fn := w.beforeWrite
		w.beforeWrite = nil
		fn()
	}
	return w.ResponseRecorder.Write(p)
}

func TestGPXExportReleasesSnapshotBeforeDownload(t *testing.T) {
	_, st, stamp, _, server, _ := testutil.Setup(t)
	stamp.Shutdown()
	if _, err := st.Insert(sampleWebLocation(t, 100)); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	cookie := login(t, handler)
	req := httptest.NewRequest(http.MethodGet, "/api/export/gpx", nil)
	req.AddCookie(cookie)
	checked := false
	w := &downloadCheckpointWriter{ResponseRecorder: httptest.NewRecorder(), beforeWrite: func() {
		checked = true
		if _, err := st.Insert(sampleWebLocation(t, 200)); err != nil {
			t.Fatal(err)
		}
		if err := st.Checkpoint(); err != nil {
			t.Fatalf("download retains SQLite snapshot: %v", err)
		}
	}}
	handler.ServeHTTP(w, req)
	if !checked || w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<trkpt") {
		t.Fatalf("checked=%t status=%d body=%q", checked, w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "1970-01-01T00:03:20Z") {
		t.Fatal("concurrent location leaked into the serialized snapshot")
	}
}
