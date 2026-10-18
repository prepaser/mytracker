package backup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestStreamingManifestValidation(t *testing.T) {
	hash := strings.Repeat("a", 64)
	info := fmt.Sprintf(`{"sha256":%q,"size":1}`, hash)
	base := `{"format_version":1,"schema_version":1,"files":{"mytracker.db":` + info + `}}`
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"valid", base, true},
		{"duplicate", `{"format_version":1,"schema_version":1,"files":{"mytracker.db":` + info + `,"mytracker.db":` + info + `}}`, false},
		{"unlisted", strings.Replace(base, "mytracker.db", "proofs/2.tsr", 1), false},
		{"oversized key", strings.Replace(base, "mytracker.db", strings.Repeat("x", maxManifestValueBytes*4), 1), false},
		{"invalid digest", strings.Replace(base, hash, "bad", 1), false},
		{"negative size", strings.Replace(base, `"size":1`, `"size":-1`, 1), false},
		{"suffix", base + `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			index, err := newBackupIndex(t.Context(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = index.Close() }()
			if err := index.Add(t.Context(), "mytracker.db", 0, 1); err != nil {
				t.Fatal(err)
			}
			m, _, err := readManifest(t.Context(), strings.NewReader(tc.data), index)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if err != nil {
				return
			}
			var output bytes.Buffer
			if err := writeManifest(t.Context(), &output, m, index); err != nil {
				t.Fatal(err)
			}
			var parsed Manifest
			if err := json.Unmarshal(output.Bytes(), &parsed); err != nil || parsed.Files["mytracker.db"].SHA256 != hash {
				t.Fatalf("roundtrip manifest=%+v err=%v", parsed, err)
			}
		})
	}
}
