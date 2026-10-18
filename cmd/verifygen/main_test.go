package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratesSelfContainedVerifier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.html")
	if err := run([]string{"-out", path, "-tsa-policy", "1.2.3.4"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	for _, forbidden := range []string{"{{LUCIDE_JS}}", "{{WORKER_JS_JSON}}", "{{DEFAULT_OPTIONS_JSON}}", "<script src=", "<link rel=\"stylesheet\" href=\"http"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("unresolved or external resource: %s", forbidden)
		}
	}
	if !strings.Contains(html, "WebAssembly.instantiate") || !strings.Contains(html, "AGFzbQ") {
		t.Fatal("embedded verifier is missing")
	}
	for _, notice := range []string{"AGPL-3.0-only", "https://github.com/prepaser/mytracker", "github.com/digitorus/pkcs7", "github.com/nbd-wtf/opentimestamps"} {
		if !strings.Contains(html, notice) {
			t.Fatalf("license notice missing: %s", notice)
		}
	}
}

func TestRejectsPrivateKeysAsTrustRoots(t *testing.T) {
	if _, err := publicRoots([]byte("-----BEGIN PRIVATE KEY-----\nAA==\n-----END PRIVATE KEY-----\n")); err == nil {
		t.Fatal("private key accepted")
	}
}
