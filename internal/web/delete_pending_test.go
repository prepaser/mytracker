package web_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/privacy"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/testutil"
)

func TestDeleteReportsExistingPendingCleanup(t *testing.T) {
	cfg, st, _, _, srv, _ := testutil.Setup(t)
	raw := map[string]any{"_type": "location", "tst": int64(100), "lat": 1.0, "lon": 2.0}
	canonicalRecord, err := canonical.FromLocation("u", "d", raw)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := canonicalRecord.Digest()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(&store.Location{
		User: "u", Device: "d", Tst: 100, Lat: 1, Lon: 2,
		Payload: payload, Digest: digest[:],
	}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(cfg.DataDir, ".privacy-purge.pending")
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"from": {"100"}, "to": {"100"}, "confirm": {"DELETE"}}
	response := authed(t, srv.Handler(), http.MethodPost, "/api/delete", strings.NewReader(form.Encode()))
	if response.Code != http.StatusAccepted {
		t.Fatalf("delete=%d %s", response.Code, response.Body.String())
	}
	var body struct {
		Deleted int  `json:"deleted"`
		Pending bool `json:"purge_pending"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Deleted != 1 || !body.Pending {
		t.Fatalf("delete response=%s", response.Body.String())
	}
	pending, err := privacy.Pending(cfg.DataDir)
	if err != nil || !pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("pending marker removed: %v", err)
	}
}
