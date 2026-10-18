package gpx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/store"
)

func TestParseAndWrite(t *testing.T) {
	src := `<gpx><trk><trkseg><trkpt lat="37.1" lon="127.2"><ele>12.5</ele><time>2026-01-02T03:04:05Z</time></trkpt></trkseg></trk></gpx>`
	points, err := Parse(strings.NewReader(src))
	if err != nil || len(points) != 1 || points[0].Ele == nil {
		t.Fatalf("points=%+v err=%v", points, err)
	}
	loc, err := Location(points[0], "u", "d")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = Write(&out, "test", func(fn func([]store.Location) error) error { return fn([]store.Location{*loc}) }, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	round, err := Parse(bytes.NewReader(out.Bytes()))
	if err != nil || len(round) != 1 || round[0].Tst != points[0].Tst {
		t.Fatalf("round=%+v err=%v xml=%s", round, err, out.String())
	}
}

func TestParseRejectsMissingTimeAndDuplicates(t *testing.T) {
	tests := []string{`<gpx><trkpt lat="1" lon="2"></trkpt></gpx>`, `<gpx><trkpt lat="1" lon="2"><time>2026-01-01T00:00:00Z</time></trkpt><trkpt lat="2" lon="3"><time>2026-01-01T00:00:00Z</time></trkpt></gpx>`}
	for _, src := range tests {
		if _, err := Parse(strings.NewReader(src)); err == nil {
			t.Fatalf("accepted %s", src)
		}
	}
}

func TestParseRejectsMalformedAndOutOfRange(t *testing.T) {
	for _, src := range []string{`<gpx>`, `<gpx></gpx>`, `<gpx><trkpt lat="91" lon="2"><time>2026-01-01T00:00:00Z</time></trkpt></gpx>`, `<gpx><trkpt lat="NaN" lon="2"><time>2026-01-01T00:00:00Z</time></trkpt></gpx>`} {
		if _, err := Parse(strings.NewReader(src)); err == nil {
			t.Fatalf("accepted %s", src)
		}
	}
	loc, err := Location(Point{Lat: 1, Lon: 2, Tst: 1}, "u", "d")
	if err != nil || loc.Tst != 1 {
		t.Fatalf("loc=%+v err=%v", loc, err)
	}
}

type readProof struct {
	data []byte
	max  int
}

func (r *readProof) Read(p []byte) (int, error) {
	if len(p) > r.max {
		r.max = len(p)
	}
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestScanRejectsOversizedMetadataBeforeReadOverrun(t *testing.T) {
	data := []byte(`<gpx><metadata>` + strings.Repeat("x", maxXMLTokenBytes+1) + `</metadata></gpx>`)
	r := &readProof{data: data}
	if err := Scan(r, func(Point) error { return nil }); !errors.Is(err, ErrXMLTooLarge) {
		t.Fatalf("err=%v", err)
	}
	if r.max > maxXMLTokenBytes {
		t.Fatalf("reader overread request=%d", r.max)
	}
}

func TestScanRejectsOversizedTrackPoint(t *testing.T) {
	src := `<gpx><trkpt lat="1" lon="2"><note>` + strings.Repeat("x", maxTrackPointBytes) +
		`</note><time>2026-01-01T00:00:00Z</time></trkpt></gpx>`
	if err := Scan(strings.NewReader(src), func(Point) error { return nil }); !errors.Is(err, ErrXMLTooLarge) {
		t.Fatalf("err=%v", err)
	}
}

func TestScanAcceptsNamespacedMultibatchGPX(t *testing.T) {
	src := `<gpx version="1.1" xmlns="http://www.topografix.com/GPX/1/1"><trk><trkseg>` +
		`<trkpt lat="1" lon="2"><time>2026-01-01T00:00:00Z</time></trkpt>` +
		`<trkpt lat="3" lon="4"><time>2026-01-01T00:01:00Z</time></trkpt>` +
		`</trkseg><trkseg><trkpt lat="5" lon="6"><time>2026-01-01T00:02:00Z</time></trkpt></trkseg>` +
		`</trk></gpx>`
	r := &readProof{data: []byte(src)}
	var points []Point
	if err := Scan(r, func(p Point) error {
		points = append(points, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 || points[2].Lat != 5 || points[2].Tst != 1767225720 {
		t.Fatalf("points=%+v", points)
	}
}

func TestLocationRejectsInvalidIdentity(t *testing.T) {
	point := Point{Lat: 1, Lon: 2, Tst: 1}
	for _, value := range []string{"", strings.Repeat("u", 129), " u", "u ", "u\n", "u\x00", "\xff"} {
		for _, identity := range [][2]string{{value, "d"}, {"u", value}} {
			if _, err := Location(point, identity[0], identity[1]); err == nil {
				t.Fatalf("accepted identity=%q", identity)
			}
		}
	}
	if _, err := Location(point, strings.Repeat("\xc3\xa9", 64), "d"); err != nil {
		t.Fatalf("valid 128-byte identity: %v", err)
	}
}

func TestImportContextCommitsCountsAndRetryLimits(t *testing.T) {
	st := openImportStore(t)
	result, err := ImportContext(context.Background(), strings.NewReader(`<gpx><trkpt lat="1" lon="2"><time>2026-01-01T00:00:00Z</time></trkpt><trkpt lat="3" lon="4"><time>2026-01-01T00:01:00Z</time></trkpt></gpx>`), st, "u", "d", 7)
	if err != nil || result.Imported != 2 || result.Skipped != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	jobs, err := st.ListStampJobs(10)
	if err != nil || len(jobs) != 4 {
		t.Fatalf("jobs=%d err=%v", len(jobs), err)
	}
	for _, job := range jobs {
		if job.MaxAttempts != 7 {
			t.Fatalf("job=%+v", job)
		}
	}
}

func TestImportContextSkipsExistingRows(t *testing.T) {
	st := openImportStore(t)
	loc, err := Location(Point{Lat: 1, Lon: 2, Tst: 1767225600}, "u", "d")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(loc); err != nil {
		t.Fatal(err)
	}
	result, err := ImportContext(context.Background(), strings.NewReader(`<gpx><trkpt lat="1" lon="2"><time>2026-01-01T00:00:00Z</time></trkpt><trkpt lat="3" lon="4"><time>2026-01-01T00:01:00Z</time></trkpt></gpx>`), st, "u", "d", 7)
	if err != nil || result.Imported != 1 || result.Skipped != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	locations, err := st.ListAll()
	if err != nil || len(locations) != 2 {
		t.Fatalf("locations=%d err=%v", len(locations), err)
	}
}

func TestImportContextRollsBackCountsAndJobsOnLateError(t *testing.T) {
	for name, source := range map[string]string{
		"duplicate": `<gpx><trkpt lat="1" lon="2"><time>2026-01-01T00:00:00Z</time></trkpt><trkpt lat="3" lon="4"><time>2026-01-01T00:01:00Z</time></trkpt><trkpt lat="5" lon="6"><time>2026-01-01T00:01:00Z</time></trkpt></gpx>`,
		"malformed": `<gpx><trkpt lat="1" lon="2"><time>2026-01-01T00:00:00Z</time></trkpt><trkpt lat="3" lon="4"></trkpt></gpx>`,
	} {
		t.Run(name, func(t *testing.T) {
			st := openImportStore(t)
			result, err := ImportContext(context.Background(), strings.NewReader(source), st, "u", "d", 7)
			if result != (ImportResult{}) || err == nil || name == "duplicate" && !errors.Is(err, ErrDuplicateTimestamp) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			locations, err := st.ListAll()
			if err != nil || len(locations) != 0 {
				t.Fatalf("locations=%d err=%v", len(locations), err)
			}
			status, err := st.JobStatus()
			if err != nil || status.Queued != 0 || status.Running != 0 || status.Retry != 0 || status.Failed != 0 {
				t.Fatalf("status=%+v err=%v", status, err)
			}
		})
	}
}

func TestImportContextCancellationRollsBack(t *testing.T) {
	st := openImportStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	result, err := ImportContext(ctx, &cancelReader{first: []byte(`<gpx><trkpt lat="1" lon="2"><time>2026-01-01T00:00:00Z</time></trkpt>`), cancel: cancel}, st, "u", "d", 7)
	if !errors.Is(err, context.Canceled) || result != (ImportResult{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	locations, err := st.ListAll()
	if err != nil || len(locations) != 0 {
		t.Fatalf("locations=%d err=%v", len(locations), err)
	}
}

type cancelReader struct {
	first  []byte
	cancel context.CancelFunc
	done   bool
}

func (r *cancelReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.first), nil
	}
	r.cancel()
	return 0, context.Canceled
}

func openImportStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestWriteFailure(t *testing.T) {
	if err := Write(failWriter{}, "x", func(func([]store.Location) error) error { return nil }, time.Minute); err == nil {
		t.Fatal("write failure ignored")
	}
}

func TestWriteSegmentsAndCallbackError(t *testing.T) {
	locs := []store.Location{{User: "u", Device: "a", Tst: 1, Lat: 1, Lon: 2, Payload: []byte(`{}`)}, {User: "u", Device: "a", Tst: 4000, Lat: 2, Lon: 3, Payload: []byte(`{}`)}, {User: "u", Device: "b", Tst: 4001, Lat: 3, Lon: 4, Payload: []byte(`{}`)}}
	var out bytes.Buffer
	if err := Write(&out, "a&b", func(fn func([]store.Location) error) error { return fn(locs) }, time.Minute); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "<trkseg>") != 3 || !strings.Contains(out.String(), "a&amp;b") {
		t.Fatalf("xml=%s", out.String())
	}
	want := io.ErrUnexpectedEOF
	if err := Write(io.Discard, "x", func(func([]store.Location) error) error { return want }, time.Minute); err != want {
		t.Fatalf("err=%v", err)
	}
}
