package importzip

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/export"
)

func TestAuditFileToPreservesOutputShapes(t *testing.T) {
	archive := makeArchive(t, map[string][]byte{
		"manifest.json":   []byte(`{"version":2}`),
		"locations.jsonl": []byte{},
	})
	path := filepath.Join(t.TempDir(), "export.zip")
	if err := os.WriteFile(path, archive, 0600); err != nil {
		t.Fatal(err)
	}
	var jsonOut bytes.Buffer
	report, err := AuditFileTo(context.Background(), path, config.Default(), &jsonOut, "json")
	if err != nil {
		t.Fatal(err)
	}
	if report.Overall != "inconclusive" || report.locationCount != 0 {
		t.Fatalf("report=%+v", report)
	}
	var decoded AuditReport
	if err := json.Unmarshal(jsonOut.Bytes(), &decoded); err != nil {
		t.Fatalf("json=%q: %v", jsonOut.String(), err)
	}
	if decoded.ArchiveVersion != 2 || decoded.Overall != "inconclusive" || decoded.Locations != nil {
		t.Fatalf("decoded=%+v", decoded)
	}

	var textOut bytes.Buffer
	if _, err := AuditFileTo(context.Background(), path, config.Default(), &textOut, "text"); err != nil {
		t.Fatal(err)
	}
	if got, want := textOut.String(), "archive v2: inconclusive\n"; got != want {
		t.Fatalf("text=%q, want %q", got, want)
	}
}

func TestAuditFileToWritesNonEmptyJSON(t *testing.T) {
	payload := json.RawMessage(`{"_type":"location","tst":100,"lat":1,"lon":2}`)
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	can, err := canonical.FromLocation("u", "d", raw)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := can.Digest()
	if err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(export.Record{
		ID: 1, User: "u", Device: "d", EventUnix: 100,
		EventTimeUTC: "1970-01-01T00:01:40Z", ReceivedUTC: "1970-01-01T00:01:40Z",
		Latitude: 1, Longitude: 2, DigestSHA256: fmt.Sprintf("%x", digest), Payload: payload,
		TsaStatus: "pending", OtsStatus: "pending",
	})
	if err != nil {
		t.Fatal(err)
	}
	archive := makeArchive(t, map[string][]byte{
		"manifest.json":   []byte(`{"version":2}`),
		"locations.jsonl": append(record, '\n'),
	})
	path := filepath.Join(t.TempDir(), "export.zip")
	if err := os.WriteFile(path, archive, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	report, err := AuditFileTo(context.Background(), path, config.Default(), &out, "json")
	if err != nil {
		t.Fatal(err)
	}
	if report.locationCount != 1 || report.Overall != "invalid" {
		t.Fatalf("report=%+v", report)
	}
	var decoded AuditReport
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("json=%q: %v", out.String(), err)
	}
	if len(decoded.Locations) != 1 || decoded.Locations[0].ID != 1 || decoded.Locations[0].TSA.Result != "missing" {
		t.Fatalf("decoded=%+v", decoded)
	}
}

func TestAuditFileToPropagatesWriterFailure(t *testing.T) {
	archive := makeArchive(t, map[string][]byte{
		"manifest.json":   []byte(`{"version":2}`),
		"locations.jsonl": []byte{},
	})
	path := filepath.Join(t.TempDir(), "export.zip")
	if err := os.WriteFile(path, archive, 0600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("output failed")
	if _, err := AuditFileTo(context.Background(), path, config.Default(), errorWriter{err: want}, "json"); !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
}
