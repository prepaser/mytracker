package export

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type cancelingReader struct {
	cancel context.CancelFunc
	done   bool
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestWriteFileContextRejectsOversizedProof(t *testing.T) {
	path := filepath.Join(t.TempDir(), "1.tsr")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, MaxProofBytes+1); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	if _, _, _, err := writeFileContext(context.Background(), zw, "proofs/1.tsr", path); err == nil {
		t.Fatal("oversized proof accepted")
	}
	_ = zw.Close()
}

func TestLimitWriterRejectsOverflow(t *testing.T) {
	var dst bytes.Buffer
	w := &limitWriter{w: &dst, max: 3}
	if n, err := w.Write([]byte("abc")); err != nil || n != 3 {
		t.Fatalf("exact write n=%d err=%v", n, err)
	}
	if n, err := w.Write([]byte("d")); err == nil || n != 0 {
		t.Fatalf("overflow write n=%d err=%v", n, err)
	}
}

func (r *cancelingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	p[0] = 1
	r.cancel()
	return 1, nil
}

func TestCopyContextStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelingReader{cancel: cancel}
	if err := copyContext(ctx, io.Discard, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func TestContextIOErrors(t *testing.T) {
	want := errors.New("I/O failed")
	if err := copyContext(context.Background(), io.Discard, errorReader{want}); !errors.Is(err, want) {
		t.Fatalf("read error=%v", err)
	}
	if err := copyContext(context.Background(), errorWriter{want}, bytes.NewReader([]byte("x"))); !errors.Is(err, want) {
		t.Fatalf("write error=%v", err)
	}
	if err := copyContext(context.Background(), shortWriter{}, bytes.NewReader([]byte("x"))); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error=%v", err)
	}
	var dst bytes.Buffer
	if n, err := (contextWriter{ctx: context.Background(), w: &dst}).Write([]byte("ok")); err != nil || n != 2 {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (contextWriter{ctx: ctx, w: &dst}).Write(nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write error=%v", err)
	}
	if err := writeBytesContext(ctx, zip.NewWriter(io.Discard), "x", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled bytes error=%v", err)
	}
	if err := WriteZipContext(ctx, io.Discard, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled export error=%v", err)
	}
}

func TestContextZipHelpers(t *testing.T) {
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	if err := writeBytesContext(context.Background(), zw, "large", bytes.Repeat([]byte("x"), 70<<10)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, _, _, err := writeFileContext(context.Background(), zw, "missing", filepath.Join(dir, "missing")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := writeFileContext(context.Background(), zw, "directory", dir); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil || len(zr.File) != 1 || zr.File[0].UncompressedSize64 != 70<<10 {
		t.Fatalf("zip files=%d err=%v", len(zr.File), err)
	}
}

func TestCSVTextNeutralizesFormulaPrefixes(t *testing.T) {
	for _, tc := range []struct {
		input, want string
	}{
		{"=1+1", "'=1+1"},
		{"+1+1", "'+1+1"},
		{"-1+1", "'-1+1"},
		{"@SUM(A1:A2)", "'@SUM(A1:A2)"},
		{"\t=1+1", "'\t=1+1"},
		{" \u2003=1+1", "' \u2003=1+1"},
		{"\ufeff=1+1", "'\ufeff=1+1"},
		{"＝1+1", "＝1+1"},
		{"alice", "alice"},
		{"", ""},
	} {
		if got := csvText(tc.input); got != tc.want {
			t.Fatalf("csvText(%q)=%q, want %q", tc.input, got, tc.want)
		}
	}
}
