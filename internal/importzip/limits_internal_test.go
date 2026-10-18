package importzip

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/export"
	"github.com/prepaser/mytracker/internal/store"
)

func TestRecordReaderPreservesBufferedValues(t *testing.T) {
	const limit = 31
	input := "\n" + `{"note":"abcdefghijklmnopqrst"}` + "\n\n" + `{"next":true}`
	r := &recordReader{r: bufio.NewReader(strings.NewReader(input))}
	dec := json.NewDecoder(r)
	for range 2 {
		space, onlySpace := bufferedJSONSpace(dec)
		start := dec.InputOffset() + space
		r.remaining = limit + 1 - (r.total - start)
		r.skipSpace = onlySpace
		var value map[string]any
		if err := dec.Decode(&value); err != nil {
			t.Fatal(err)
		}
		if dec.InputOffset()-start > limit {
			t.Fatal("record exceeds budget")
		}
	}
	r.remaining = limit + 1 - (r.total - dec.InputOffset())
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("end=%v", err)
	}
	r.remaining = 0
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, errRecordTooLarge) {
		t.Fatalf("budget error=%v", err)
	}
}

func TestRecordLimit(t *testing.T) {
	if err := recordLimit(maxLocations-1, maxLocations, "locations"); err != nil {
		t.Fatal(err)
	}
	if err := recordLimit(maxLocations, maxLocations, "locations"); err == nil {
		t.Fatal("record limit accepted overflow")
	}
}

func TestV6RecordMetadata(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","tst":1,"lat":1,"lon":2}`)
	base := boundedTestRecord(t, "u", "d", payload)
	base.EventTimeUTC = time.Unix(1, 0).UTC().Format(time.RFC3339)
	base.ReceivedUTC = base.EventTimeUTC
	base.Latitude, base.Longitude = 1, 2
	base.TsaStatus, base.OtsStatus = "pending", "pending"
	if !validRecord(base, 6) {
		t.Fatal("valid v6 record rejected")
	}
	for name, mutate := range map[string]func(*export.Record){
		"identity": func(rec *export.Record) {
			rec.User = "bad\nuser"
			rec.DigestSHA256 = testDigest(t, rec.User, rec.Device, rec.Payload)
		},
		"event time":  func(rec *export.Record) { rec.EventTimeUTC = "invalid" },
		"received":    func(rec *export.Record) { rec.ReceivedUTC = "invalid" },
		"coordinates": func(rec *export.Record) { rec.Latitude++ },
		"status":      func(rec *export.Record) { rec.TsaStatus = "unknown" },
	} {
		rec := base
		mutate(&rec)
		if validRecord(rec, 6) {
			t.Errorf("accepted invalid %s", name)
		}
	}
	legacy := base
	legacy.EventTimeUTC, legacy.ReceivedUTC = "", "invalid"
	legacy.Latitude, legacy.Longitude = 0, 0
	if !validRecord(legacy, 5) {
		t.Fatal("legacy record compatibility lost")
	}
}

func TestRecordRejectsNonRFC3339TimestampAcrossVersions(t *testing.T) {
	const nonRepresentableUnix = int64(1<<63 - 1024)
	payload := json.RawMessage(fmt.Sprintf(`{"_type":"location","tst":%d,"lat":1,"lon":2}`, nonRepresentableUnix))
	rec := boundedTestRecord(t, "u", "d", payload)
	rec.EventUnix = nonRepresentableUnix
	for version := 1; version <= 6; version++ {
		if validRecord(rec, version) {
			t.Fatalf("accepted non-RFC3339 timestamp for v%d", version)
		}
	}

	legacy := boundedTestRecord(t, "u", "d", json.RawMessage(`{"_type":"location","tst":253402300799,"lat":1,"lon":2}`))
	legacy.EventUnix = 253402300799
	if !validRecord(legacy, 5) {
		t.Fatal("rejected representable legacy timestamp")
	}
}

func TestValidRecordEnforcesPerRecordBounds(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","tst":1,"lat":1,"lon":2}`)
	base := boundedTestRecord(t, "u", "d", payload)
	for name, mutate := range map[string]func(*export.Record){
		"user": func(rec *export.Record) {
			rec.User = strings.Repeat("u", maxIdentity+1)
			rec.DigestSHA256 = testDigest(t, rec.User, rec.Device, rec.Payload)
		},
		"device": func(rec *export.Record) {
			rec.Device = strings.Repeat("d", maxIdentity+1)
			rec.DigestSHA256 = testDigest(t, rec.User, rec.Device, rec.Payload)
		},
		"payload": func(rec *export.Record) {
			rec.Payload = json.RawMessage(`{"_type":"location","tst":1,"lat":1,"lon":2,"extra":"` + strings.Repeat("x", maxPayloadBytes) + `"}`)
		},
	} {
		rec := base
		mutate(&rec)
		if validRecord(rec, 5) {
			t.Errorf("accepted oversized %s record", name)
		}
	}
}

func boundedTestRecord(t *testing.T, user, device string, payload json.RawMessage) export.Record {
	t.Helper()
	return export.Record{
		ID: 1, User: user, Device: device, EventUnix: 1,
		DigestSHA256: testDigest(t, user, device, payload), Payload: payload,
	}
}

func testDigest(t *testing.T, user, device string, payload []byte) string {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	rec, err := canonical.FromLocation(user, device, raw)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := rec.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(digest[:])
}

func TestVerificationEvidenceBounds(t *testing.T) {
	run := export.VerificationRunRecord{VerificationRun: store.VerificationRun{
		ID: 1, LocationID: 1, Kind: "tsa", Trigger: "manual", Result: "missing", CheckedAt: 1,
	}}
	for name, mutate := range map[string]func(*export.VerificationRunRecord){
		"subject": func(rec *export.VerificationRunRecord) { rec.TSASignerSubject = strings.Repeat("x", maxSubjectText+1) },
		"policy":  func(rec *export.VerificationRunRecord) { rec.TSAPolicyOID = strings.Repeat("1", maxOIDText+1) },
		"error":   func(rec *export.VerificationRunRecord) { rec.ErrorCode = strings.Repeat("x", maxErrorText+1) },
	} {
		candidate := run
		mutate(&candidate)
		if validVerificationRun(candidate) {
			t.Errorf("accepted oversized %s evidence", name)
		}
	}
	attempt := store.VerificationAttempt{
		ID: 1, RunID: 1, ProviderID: "esplora-1", Result: "error",
		BlockHash: strings.Repeat("a", maxHashText+1),
	}
	if validVerificationAttempt(attempt) {
		t.Fatal("accepted oversized attempt block hash")
	}
	attempt.BlockHash = ""
	attempt.Confirmations = -1
	if validVerificationAttempt(attempt) {
		t.Fatal("accepted negative attempt evidence")
	}
}

func TestJSONLRejectsOversizedAndTrailingRecords(t *testing.T) {
	for name, content := range map[string]string{
		"oversized": strings.Repeat("x", maxRecordJSON+1),
		"trailing":  `{"id":1} {"id":2}`,
	} {
		var archive bytes.Buffer
		zw := zip.NewWriter(&archive)
		w, err := zw.Create("records.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(w, content); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
		if err != nil {
			t.Fatal(err)
		}
		err = eachJSONLContext(t.Context(), zr.File[0], func(dec *json.Decoder) error {
			var value map[string]any
			return dec.Decode(&value)
		})
		if err == nil {
			t.Errorf("accepted malformed %s JSONL", name)
		}
	}
}

func TestJSONLAcceptsUnterminatedFinalRecord(t *testing.T) {
	line, err := readJSONLine(bufio.NewReader(strings.NewReader(`{"id":1}`)))
	if err != nil || string(line) != `{"id":1}` {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

func TestManifestRejectsTrailingJSON(t *testing.T) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.Create("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(`{"version":2} {"version":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkManifestContext(t.Context(), zr); err == nil {
		t.Fatal("manifest with trailing JSON accepted")
	}
}
