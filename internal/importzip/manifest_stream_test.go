package importzip

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestVerifyStreamsUnknownManifestValues(t *testing.T) {
	archive := makeArchive(t, map[string][]byte{
		"manifest.json":   []byte(`{"version":2,"created_at":"now","format":"test","proof_sha256":{"proofs/ignored.tsr":"0000000000000000000000000000000000000000000000000000000000000000"},"extra":{"items":[null,true,3,{"nested":"value"}]}}`),
		"locations.jsonl": nil,
	})
	result, err := Verify(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.Imported != 0 || result.Proofs != 0 || result.Verifications != 0 {
		t.Fatalf("result=%+v", result)
	}
	nullMap := makeArchive(t, map[string][]byte{
		"manifest.json":   []byte(`{"version":2,"proof_sha256":null}`),
		"locations.jsonl": nil,
	})
	if _, err := Verify(bytes.NewReader(nullMap), int64(len(nullMap))); err != nil {
		t.Fatalf("Verify null digest map: %v", err)
	}
}

func TestVerifyRejectsStreamingManifestErrors(t *testing.T) {
	longName := strings.Repeat("n", maxProofPath+1)
	longDigest := strings.Repeat("0", maxHashText+1)
	tests := []struct {
		name     string
		manifest string
	}{
		{name: "non-object", manifest: `[]`},
		{name: "trailing-value", manifest: `{"version":2}{}`},
		{name: "digest-map-array", manifest: `{"version":2,"proof_sha256":[]}`},
		{name: "digest-map-value", manifest: `{"version":2,"proof_sha256":{"proofs/1.tsr":1}}`},
		{name: "digest-name-bound", manifest: `{"version":2,"proof_sha256":{"` + longName + `":"` + strings.Repeat("0", maxHashText) + `"}}`},
		{name: "digest-value-bound", manifest: `{"version":2,"proof_sha256":{"proofs/1.tsr":"` + longDigest + `"}}`},
		{name: "unknown-malformed", manifest: `{"version":2,"unknown":{"nested":[}`},
		{name: "unknown-unclosed", manifest: `{"version":2,"unknown":{"nested":1}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := makeArchive(t, map[string][]byte{
				"manifest.json":   []byte(test.manifest),
				"locations.jsonl": nil,
			})
			if _, err := Verify(bytes.NewReader(archive), int64(len(archive))); err == nil {
				t.Fatalf("accepted manifest %s", test.name)
			}
		})
	}
}

func TestVerifyCanceledBeforeManifestIndex(t *testing.T) {
	archive := makeArchive(t, map[string][]byte{
		"manifest.json":   []byte(`{"version":2}`),
		"locations.jsonl": nil,
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := VerifyContext(ctx, bytes.NewReader(archive), int64(len(archive))); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}
