package atomicfile_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/prepaser/mytracker/internal/atomicfile"
)

func TestWriteReplacesFileAndKeepsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "value")
	if err := atomicfile.Write(path, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicfile.Write(path, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if string(data) != "second" || info.Mode().Perm() != 0600 {
		t.Fatalf("data=%q mode=%o", data, info.Mode().Perm())
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".value.tmp-*"))
	if len(files) != 0 {
		t.Fatalf("temporary files: %v", files)
	}
}

func TestWriteFuncFailurePreservesOriginal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("write failed")
	if err := atomicfile.WriteFunc(path, 0600, func(f *os.File) error { _, _ = f.WriteString("partial"); return want }); !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "original" {
		t.Fatalf("data=%q err=%v", data, err)
	}
}

func TestWriteRejectsFileAsParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent")
	if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicfile.Write(filepath.Join(parent, "value"), []byte("x"), 0600); err == nil {
		t.Fatal("write with file parent succeeded")
	}
}

func TestCommittedErrorClassification(t *testing.T) {
	want := errors.New("sync failed")
	err := &atomicfile.CommittedError{Err: want}
	if !atomicfile.IsCommitted(err) || !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
}
