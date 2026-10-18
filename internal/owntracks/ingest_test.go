package owntracks_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/owntracks"
	"github.com/prepaser/mytracker/internal/stamp"
	"github.com/prepaser/mytracker/internal/store"
)

func TestIngestRejectsOversizedPayload(t *testing.T) {
	body := bytes.Repeat([]byte{'x'}, owntracks.MaxPayloadBytes+1)
	if _, err := owntracks.Ingest(nil, noopStamp{}, "u", "d", body); !errors.Is(err, owntracks.ErrInvalidPayload) {
		t.Fatalf("oversized payload error=%v", err)
	}
}

func TestIngestRejectsInvalidIdentity(t *testing.T) {
	body := []byte(`{"_type":"location","tst":1,"lat":1,"lon":2}`)
	for _, identity := range []string{strings.Repeat("x", owntracks.MaxIdentityBytes+1), "bad\nname", "   "} {
		if _, err := owntracks.Ingest(nil, noopStamp{}, identity, "device", body); !errors.Is(err, owntracks.ErrInvalidPayload) {
			t.Fatalf("identity %q error=%v", identity, err)
		}
	}
}

func TestIngestDropsInvalidOptionalPayloadValues(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	body := []byte(`{"_type":"location","tst":1,"lat":1,"lon":2,"acc":{"padding":"` + strings.Repeat("x", 100000) + `"},"batt":[1,2,3]}`)
	id, err := owntracks.Ingest(st, noopStamp{}, "user", "device", body)
	if err != nil {
		t.Fatal(err)
	}
	loc, err := st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loc.Payload) > 100 || bytes.Contains(loc.Payload, []byte("padding")) {
		t.Fatalf("stored optional payload was not minimized: %d bytes", len(loc.Payload))
	}
}

func TestIngestClassifiesDigestNumericOverflow(t *testing.T) {
	body := []byte(`{"_type":"location","tst":1,"lat":1,"lon":2,"acc":1e308}`)
	if _, err := owntracks.Ingest(nil, noopStamp{}, "u", "d", body); !errors.Is(err, owntracks.ErrInvalidPayload) {
		t.Fatalf("numeric overflow error=%v", err)
	}
}

func TestIngestWaypoint(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	body := []byte(`{"_type":"waypoint","lat":37.5,"lon":127.0,"tst":1751800000}`)
	id, err := owntracks.Ingest(st, noopStamp{}, "u", "d", body, 1)
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("expected insert")
	}
	jobs, err := st.ListLocationStampJobs(id)
	if err != nil || len(jobs) != 2 || jobs[0].MaxAttempts != 1 || jobs[1].MaxAttempts != 1 {
		t.Fatalf("stamp jobs=%+v err=%v", jobs, err)
	}
}

func TestIngestRejectsFutureLocation(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	body := []byte(fmt.Sprintf(`{"_type":"location","lat":1,"lon":2,"tst":%d}`, time.Now().Add(25*time.Hour).Unix()))
	if _, err := owntracks.Ingest(st, noopStamp{}, "u", "d", body); !errors.Is(err, owntracks.ErrInvalidPayload) {
		t.Fatalf("future location error=%v", err)
	}
}

func TestIngestTransition(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	cfgMgr := config.NewManager(cfg, nil)
	stampSvc := stamp.New(cfgMgr, st)
	body := []byte(`{"_type":"transition","t":1751800000,"lat":37.5,"lon":127.0}`)
	id, err := owntracks.Ingest(st, stampSvc, "u", "d", body)
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("expected insert")
	}
}

func TestIngestTransitionDigestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	body := []byte(`{"_type":"transition","t":1751800000,"lat":37.5,"lon":127.0}`)
	id, err := owntracks.Ingest(st, noopStamp{}, "u", "d", body)
	if err != nil || id == 0 {
		t.Fatalf("ingest: id=%d err=%v", id, err)
	}
	loc, err := st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(loc.Payload, &raw); err != nil {
		t.Fatal(err)
	}
	rec, err := canonical.FromLocation(loc.User, loc.Device, raw)
	if err != nil {
		t.Fatal(err)
	}
	d, err := rec.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if string(d[:]) != string(loc.Digest) {
		t.Fatal("digest mismatch from stored payload")
	}
}

func TestIngestUsesStandardOwnTracksTopic(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	body := []byte(`{"_type":"location","topic":"owntracks/alice/phone","tst":1,"lat":1,"lon":2}`)
	id, err := owntracks.Ingest(st, noopStamp{}, "", "", body)
	if err != nil {
		t.Fatal(err)
	}
	loc, err := st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if loc.User != "alice" || loc.Device != "phone" {
		t.Fatalf("identity=%s/%s", loc.User, loc.Device)
	}
	if string(loc.Payload) != `{"_type":"location","lat":1,"lon":2,"tst":1}` {
		t.Fatalf("payload=%s", loc.Payload)
	}
}

func TestIngestNormalizesIdentity(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	body := []byte(`{"_type":"location","tst":1,"lat":1,"lon":2}`)
	id, err := owntracks.Ingest(st, noopStamp{}, " alice ", " phone ", body)
	if err != nil {
		t.Fatal(err)
	}
	loc, err := st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if loc.User != "alice" || loc.Device != "phone" {
		t.Fatalf("identity=%s/%s", loc.User, loc.Device)
	}
}
