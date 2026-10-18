package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMainBuilds(t *testing.T) {
	out := filepath.Join(t.TempDir(), "mytracker")
	if err := exec.Command("go", "build", "-o", out, ".").Run(); err != nil {
		t.Fatal(err)
	}
	version, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, flag := range []string{"-version", "--version"} {
		cmd := exec.Command(out, flag)
		cmd.Dir = dir
		got, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", flag, err, got)
		}
		if want := "mytracker " + strings.TrimSpace(string(version)) + "\n"; string(got) != want {
			t.Fatalf("%s: got %q, want %q", flag, got, want)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("version command created runtime files")
	}
}
