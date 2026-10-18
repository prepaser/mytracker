package web_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/testutil"
)

func TestStampJobsLocationFilterReturnsCurrentJobStates(t *testing.T) {
	_, st, stampSvc, _, srv, _ := testutil.Setup(t)
	stampSvc.Shutdown()
	id, err := st.Insert(&store.Location{
		User: "api", Device: "stamp", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: []byte{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	leaseUntil := time.Now().Unix() + 60
	job, err := st.ClaimStampJob(time.Now().Unix(), "api-test", leaseUntil)
	if err != nil || job == nil {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	if job.Kind == "tsa" {
		stampTime := time.Now().Unix()
		err = st.CompleteStampJob(job, "ok", &stampTime)
	} else {
		err = st.CompleteStampJob(job, "submitted", nil)
	}
	if err != nil {
		t.Fatal(err)
	}

	path := "/api/stamp-jobs?location_id=" + strconv.FormatInt(id, 10) + "&limit=1"
	rec := authed(t, srv.Handler(), http.MethodGet, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("location jobs=%d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []store.StampJob `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 2 {
		t.Fatalf("location jobs=%d want 2: %+v", len(out.Data), out.Data)
	}
	states := map[string]bool{}
	for _, item := range out.Data {
		if item.LocationID != id {
			t.Fatalf("job location=%d want %d", item.LocationID, id)
		}
		states[item.State] = true
	}
	if !states["done"] || !states["queued"] {
		t.Fatalf("states=%v", states)
	}

	bad := authed(t, srv.Handler(), http.MethodGet, "/api/stamp-jobs?location_id=bad", nil)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid location id=%d", bad.Code)
	}
	empty := authed(t, srv.Handler(), http.MethodGet, "/api/stamp-jobs?location_id=999999", nil)
	if empty.Code != http.StatusOK {
		t.Fatalf("empty location jobs=%d %s", empty.Code, empty.Body.String())
	}
	var emptyOut struct {
		Data []store.StampJob `json:"data"`
	}
	if err := json.NewDecoder(empty.Body).Decode(&emptyOut); err != nil {
		t.Fatal(err)
	}
	if emptyOut.Data == nil || len(emptyOut.Data) != 0 {
		t.Fatalf("empty location jobs=%v", emptyOut.Data)
	}
}
