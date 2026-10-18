package browserverify

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	libots "github.com/nbd-wtf/opentimestamps"
	"github.com/prepaser/mytracker/internal/canonical"
)

func testArchive(t *testing.T, mutate func(map[string][]byte, *manifest)) []byte {
	t.Helper()
	raw := map[string]any{"_type": "location", "tst": float64(1700000000), "lat": float64(1), "lon": float64(2)}
	can, err := canonical.FromLocation("private-user", "private-device", raw)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := can.Digest()
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(raw)
	rec := record{ID: 1, User: can.User, Device: can.Device, EventUnix: can.Tst, EventTimeUTC: time.Unix(can.Tst, 0).UTC().Format(time.RFC3339), ReceivedUTC: time.Unix(can.Tst, 0).UTC().Format(time.RFC3339), Latitude: can.Lat, Longitude: can.Lon, DigestSHA256: hex.EncodeToString(digest[:]), Payload: payload, TsaStatus: "pending", OtsStatus: "submitted"}
	rec.Proofs.OTS = "proofs/1.ots"
	line, _ := json.Marshal(rec)
	proof := (libots.File{Digest: digest[:], Sequences: []libots.Sequence{{{Attestation: &libots.Attestation{BitcoinBlockHeight: 1}}}}}).SerializeToFile()
	files := map[string][]byte{"README.txt": []byte("fixture"), "locations.jsonl": line, "locations.csv": []byte("id\n1\n"), "verification_runs.jsonl": nil, "verification_attempts.jsonl": nil, "proofs/1.ots": proof}
	m := manifest{Version: 6, Locations: 1, Entries: map[string]string{}}
	if mutate != nil {
		mutate(files, &m)
	}
	for name, data := range files {
		sum := sha256.Sum256(data)
		if _, set := m.Entries[name]; !set {
			m.Entries[name] = hex.EncodeToString(sum[:])
		}
	}
	manifestData, _ := json.Marshal(m)
	files["manifest.json"] = manifestData
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestArchiveIntegrityAndOfflineEvidence(t *testing.T) {
	data := testArchive(t, nil)
	report, err := Verify(t.Context(), data, Options{MinConfirmations: 6}, nil)
	if err != nil || report.Integrity != "valid" || report.Entries != 7 || len(report.Locations) != 1 || report.Locations[0].OTS.Result != "inconclusive" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	b, _ := json.Marshal(report)
	if bytes.Contains(b, []byte("private-user")) || bytes.Contains(b, []byte("latitude")) {
		t.Fatal("private location data in report")
	}
}

func TestHistoryMetadataMatchesNativeValidation(t *testing.T) {
	for name, line := range map[string]string{
		"negative checked_at":   `{"id":1,"location_id":1,"kind":"tsa","result":"verified","checked_at":-1}`,
		"negative accuracy":     `{"id":1,"location_id":1,"kind":"tsa","result":"verified","tsa_accuracy_ms":-1}`,
		"unknown error":         `{"id":1,"location_id":1,"kind":"tsa","result":"error","error_code":"private-detail"}`,
		"oversized trigger":     `{"id":1,"location_id":1,"kind":"tsa","result":"verified","trigger":"` + strings.Repeat("x", maxHistoryTriggerText+1) + `"}`,
		"invalid engine type":   `{"id":1,"location_id":1,"kind":"tsa","result":"verified","engine_version":"unknown"}`,
		"invalid evidence type": `{"id":1,"location_id":1,"kind":"tsa","result":"verified","bitcoin_pow_valid":"yes"}`,
		"invalid attempts type": `{"id":1,"location_id":1,"kind":"tsa","result":"verified","attempts":"unknown"}`,
	} {
		t.Run(name, func(t *testing.T) {
			data := testArchive(t, func(files map[string][]byte, m *manifest) {
				m.Runs = 1
				files["verification_runs.jsonl"] = []byte(line)
			})
			if _, err := Verify(t.Context(), data, Options{MinConfirmations: 6}, nil); err == nil {
				t.Fatal("malformed verification history accepted")
			}
		})
	}

	run := `{"id":1,"location_id":1,"kind":"ots","result":"error"}`
	for name, attempt := range map[string]string{
		"noncanonical provider": `{"id":1,"run_id":1,"provider_id":"esplora-01","result":"error"}`,
		"unknown error":         `{"id":1,"run_id":1,"provider_id":"esplora-1","result":"error","error_code":"private-detail"}`,
		"negative duration":     `{"id":1,"run_id":1,"provider_id":"esplora-1","result":"error","duration_ms":-1}`,
	} {
		t.Run(name, func(t *testing.T) {
			data := testArchive(t, func(files map[string][]byte, m *manifest) {
				m.Runs, m.Attempts = 1, 1
				files["verification_runs.jsonl"] = []byte(run)
				files["verification_attempts.jsonl"] = []byte(attempt)
			})
			if _, err := Verify(t.Context(), data, Options{MinConfirmations: 6}, nil); err == nil {
				t.Fatal("malformed verification attempt accepted")
			}
		})
	}
}

func TestHistoryMetadataAcceptsLegacyFields(t *testing.T) {
	data := testArchive(t, func(files map[string][]byte, m *manifest) {
		m.Runs = 1
		files["verification_runs.jsonl"] = []byte(`{"id":1,"location_id":1,"kind":"tsa","result":"missing"}`)
	})
	if _, err := Verify(t.Context(), data, Options{MinConfirmations: 6}, nil); err != nil {
		t.Fatalf("legacy verification history rejected: %v", err)
	}
}

func TestRejectsInvalidArchiveBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer srv.Close()
	for _, mutation := range []func(map[string][]byte, *manifest){
		func(f map[string][]byte, m *manifest) { m.Entries["README.txt"] = strings.Repeat("0", 64) },
		func(f map[string][]byte, m *manifest) {
			f["locations.jsonl"] = bytes.Replace(f["locations.jsonl"], []byte(`"latitude":1`), []byte(`"latitude":3`), 1)
		},
		func(f map[string][]byte, m *manifest) { f["proofs/2.tsr"] = []byte("unreferenced") },
		func(f map[string][]byte, m *manifest) { m.Locations = 2 },
		func(f map[string][]byte, m *manifest) {
			m.Runs = 1
			f["verification_runs.jsonl"] = []byte(`{"id":1,"location_id":99,"kind":"tsa","result":"verified"}`)
		},
	} {
		if _, err := Verify(t.Context(), testArchive(t, mutation), Options{MinConfirmations: 6, Providers: []string{srv.URL}, Online: true}, nil); err == nil {
			t.Fatal("invalid archive accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("network used before archive validation")
	}
}

func TestTruncatedZIP64Directory(t *testing.T) {
	b := make([]byte, 42)
	binary.LittleEndian.PutUint32(b, 0x07064b50)
	binary.LittleEndian.PutUint32(b[20:], 0x06054b50)
	binary.LittleEndian.PutUint16(b[28:], 65535)
	binary.LittleEndian.PutUint16(b[30:], 65535)
	if _, err := openArchive(t.Context(), b, nil); err == nil {
		t.Fatal("truncated ZIP64 accepted")
	}
}
