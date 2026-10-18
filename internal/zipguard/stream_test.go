package zipguard

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveWalkAndFileAt(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, tc := range []struct{ name, body string }{{"one", "first"}, {"two", "second"}} {
		out, err := w.Create(tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(out, tc.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := Open(t.Context(), bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	err = a.Walk(t.Context(), func(offset int64, f *zip.File) error {
		if offset < 0 {
			return zip.ErrFormat
		}
		at, err := a.FileAt(offset)
		if err != nil || at.Name != f.Name {
			return errors.New("file lookup mismatch")
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		_, err = io.ReadAll(r)
		closeErr := r.Close()
		if err != nil {
			return err
		}
		seen++
		return closeErr
	})
	if err != nil || seen != 2 {
		t.Fatalf("walk: seen=%d err=%v", seen, err)
	}
}

func TestArchiveSFXPrefix(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	entry, _ := w.Create("entry")
	_, _ = io.WriteString(entry, "payload")
	_ = w.Close()
	data := append([]byte("prefix"), buf.Bytes()...)
	a, err := Open(t.Context(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Walk(t.Context(), func(_ int64, f *zip.File) error {
		r, err := f.Open()
		if err != nil {
			return err
		}
		got, err := io.ReadAll(r)
		closeErr := r.Close()
		if err != nil {
			return err
		}
		if string(got) != "payload" {
			t.Fatalf("payload=%q", got)
		}
		return closeErr
	}); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveRejectsTruncatedDirectory(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	_, _ = w.Create("x")
	_ = w.Close()
	data := buf.Bytes()[:buf.Len()-1]
	if _, err := Open(t.Context(), bytes.NewReader(data), int64(len(data))); !errors.Is(err, zip.ErrFormat) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err=%v", err)
	}
}

func TestArchiveContextCancellation(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	_, _ = w.Create("x")
	_ = w.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Open(ctx, bytes.NewReader(buf.Bytes()), int64(buf.Len())); !errors.Is(err, context.Canceled) {
		t.Fatalf("open err=%v", err)
	}
}

func TestArchiveLimitsRejectDirectoryCountBeforeWalk(t *testing.T) {
	data := testArchiveBytes(t, []string{"one", "two"})
	if _, err := OpenWithLimits(t.Context(), bytes.NewReader(data), int64(len(data)), 1); !errors.Is(err, zip.ErrFormat) {
		t.Fatalf("err=%v", err)
	}
}

func TestArchiveLimitsRejectDirectoryBytesBeforeWalk(t *testing.T) {
	const directorySize = int64(maxDirectoryBytes + 1)
	size := directorySize + endHeaderLen
	eocd := make([]byte, endHeaderLen)
	binary.LittleEndian.PutUint32(eocd[:4], 0x06054b50)
	binary.LittleEndian.PutUint16(eocd[8:10], 1)
	binary.LittleEndian.PutUint16(eocd[10:12], 1)
	binary.LittleEndian.PutUint32(eocd[12:16], uint32(directorySize))
	probeErr := errors.New("central directory read")
	r := &directoryLimitReader{size: size, eocd: eocd, err: probeErr}
	if _, err := OpenWithLimits(t.Context(), r, size, 1); !errors.Is(err, zip.ErrFormat) {
		t.Fatalf("err=%v", err)
	}
}

func TestArchiveLimitsAcceptsValidArchive(t *testing.T) {
	data := testArchiveBytes(t, []string{"entry"})
	a, err := OpenWithLimits(t.Context(), bytes.NewReader(data), int64(len(data)), 1)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	if err := a.Walk(t.Context(), func(_ int64, _ *zip.File) error {
		seen++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Fatalf("seen=%d", seen)
	}
}

func TestArchiveRejectsMalformedHeaders(t *testing.T) {
	data := testArchiveBytes(t, []string{"entry"})
	eocd := len(data) - endHeaderLen
	cd := int(binary.LittleEndian.Uint32(data[eocd+16 : eocd+20]))
	cases := []struct {
		name string
		edit func([]byte)
	}{
		{"local signature", func(b []byte) { b[0] ^= 1 }},
		{"central signature", func(b []byte) { b[cd] ^= 1 }},
		{"missing ZIP64 extra", func(b []byte) { binary.LittleEndian.PutUint32(b[cd+20:cd+24], maxUint32) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := append([]byte(nil), data...)
			tc.edit(input)
			if _, err := Open(t.Context(), bytes.NewReader(input), int64(len(input))); !errors.Is(err, zip.ErrFormat) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestArchiveDescriptorCRCFailure(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	entry, err := w.CreateHeader(&zip.FileHeader{Name: "entry", Method: zip.Store, Flags: 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(entry, "payload"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	eocd := len(data) - endHeaderLen
	cd := int(binary.LittleEndian.Uint32(data[eocd+16 : eocd+20]))
	localLen := 30 + int(binary.LittleEndian.Uint16(data[26:28])) + int(binary.LittleEndian.Uint16(data[28:30]))
	compressed := int(binary.LittleEndian.Uint32(data[cd+20 : cd+24]))
	descriptor := localLen + compressed
	data[descriptor+4] ^= 1
	a, err := Open(t.Context(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	err = a.Walk(t.Context(), func(_ int64, f *zip.File) error {
		r, err := f.Open()
		if err != nil {
			return err
		}
		_, readErr := io.ReadAll(r)
		_ = r.Close()
		return readErr
	})
	if !errors.Is(err, zip.ErrChecksum) {
		t.Fatalf("walk err=%v", err)
	}
}

func TestArchiveWalkCancellationAfterEntry(t *testing.T) {
	a := testOpenArchive(t, []string{"one", "two"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seen := 0
	err := a.Walk(ctx, func(_ int64, _ *zip.File) error {
		seen++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || seen != 1 {
		t.Fatalf("seen=%d err=%v", seen, err)
	}
}

func testArchiveBytes(t *testing.T, names []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, name := range names {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, name+"-body"); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testOpenArchive(t *testing.T, names []string) *Archive {
	t.Helper()
	data := testArchiveBytes(t, names)
	a, err := Open(t.Context(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSpoolWriterAbortAndCanceledCloseCleanup(t *testing.T) {
	spoolDir := t.TempDir()
	out, err := os.CreateTemp(t.TempDir(), "archive-*")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	w, err := NewSpoolWriter(ctx, out, spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := w.CreateHeader(&zip.FileHeader{Name: "entry", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := entry.Write([]byte("payload")); !errors.Is(err, context.Canceled) {
		t.Fatalf("write err=%v", err)
	}
	if err := w.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("close err=%v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(spoolDir, ".zipguard-central-*")); len(matches) != 0 {
		t.Fatalf("spool files remain after close failure: %v", matches)
	}

	out, err = os.CreateTemp(t.TempDir(), "archive-*")
	if err != nil {
		t.Fatal(err)
	}
	w, err = NewSpoolWriter(t.Context(), out, spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateHeader(&zip.FileHeader{Name: "entry", Method: zip.Store}); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(filepath.Join(spoolDir, ".zipguard-central-*")); len(matches) != 0 {
		t.Fatalf("spool files remain after abort: %v", matches)
	}
}

func TestSpoolWriterIOFailureCleanup(t *testing.T) {
	t.Run("output closed between entries", func(t *testing.T) {
		spoolDir := t.TempDir()
		out, err := os.CreateTemp(t.TempDir(), "archive-*")
		if err != nil {
			t.Fatal(err)
		}
		w, err := NewSpoolWriter(t.Context(), out, spoolDir)
		if err != nil {
			t.Fatal(err)
		}
		entry, err := w.CreateHeader(&zip.FileHeader{Name: "first", Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, "first"); err != nil {
			t.Fatal(err)
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := w.CreateHeader(&zip.FileHeader{Name: "second", Method: zip.Store}); err == nil {
			t.Fatal("entry transition succeeded on closed output")
		}
		_ = w.Abort()
		assertNoCentralSpools(t, spoolDir)
	})

	t.Run("output closed during write", func(t *testing.T) {
		spoolDir := t.TempDir()
		out, err := os.CreateTemp(t.TempDir(), "archive-*")
		if err != nil {
			t.Fatal(err)
		}
		w, err := NewSpoolWriter(t.Context(), out, spoolDir)
		if err != nil {
			t.Fatal(err)
		}
		entry, err := w.CreateHeader(&zip.FileHeader{Name: "entry", Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		_, writeErr := entry.Write([]byte("payload"))
		closeErr := w.Close()
		if writeErr == nil && closeErr == nil {
			t.Fatal("write and close succeeded on closed output")
		}
		assertNoCentralSpools(t, spoolDir)
	})

	t.Run("spool closed between entries", func(t *testing.T) {
		spoolDir := t.TempDir()
		out, err := os.CreateTemp(t.TempDir(), "archive-*")
		if err != nil {
			t.Fatal(err)
		}
		w, err := NewSpoolWriter(t.Context(), out, spoolDir)
		if err != nil {
			t.Fatal(err)
		}
		entry, err := w.CreateHeader(&zip.FileHeader{Name: "first", Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, "first"); err != nil {
			t.Fatal(err)
		}
		if err := w.spool.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := w.CreateHeader(&zip.FileHeader{Name: "second", Method: zip.Store}); err == nil {
			t.Fatal("entry transition succeeded on closed spool")
		}
		_ = w.Abort()
		assertNoCentralSpools(t, spoolDir)
	})
}

func assertNoCentralSpools(t *testing.T, dir string) {
	t.Helper()
	if matches, _ := filepath.Glob(filepath.Join(dir, ".zipguard-central-*")); len(matches) != 0 {
		t.Fatalf("spool files remain: %v", matches)
	}
}

func TestSpoolWriterRoundTripAndCleanup(t *testing.T) {
	out, err := os.CreateTemp(t.TempDir(), "archive-*")
	if err != nil {
		t.Fatal(err)
	}
	spoolDir := t.TempDir()
	w, err := NewSpoolWriter(t.Context(), out, spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		entry, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(filepath.Join(spoolDir, ".zipguard-central-*")); len(matches) != 0 {
		t.Fatalf("spool files remain: %v", matches)
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	st, _ := out.Stat()
	zr, err := zip.NewReader(out, st.Size())
	if err != nil || len(zr.File) != 2 {
		t.Fatalf("roundtrip: files=%d err=%v", len(zr.File), err)
	}
	for _, f := range zr.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(r)
		closeErr := r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil || string(body) != f.Name {
			t.Fatalf("%s: body=%q close=%v", f.Name, body, closeErr)
		}
	}
	_ = out.Close()
}

func TestSpoolWriterZIP64OffsetWithPrefix(t *testing.T) {
	out, err := os.CreateTemp(t.TempDir(), "archive-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write([]byte("prefix")); err != nil {
		t.Fatal(err)
	}
	w, err := NewSpoolWriter(t.Context(), out, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const gap = int64(1<<32 + 64)
	if err := out.Truncate(w.base + gap); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Seek(w.base+gap, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	entry, err := w.CreateHeader(&zip.FileHeader{Name: "large-offset", Method: zip.Store, Flags: 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(entry, "payload"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	st, _ := out.Stat()
	zr, err := zip.NewReader(out, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	r, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r)
	closeErr := r.Close()
	if err != nil || closeErr != nil || string(body) != "payload" {
		t.Fatalf("body=%q read=%v close=%v", body, err, closeErr)
	}
	a, err := Open(t.Context(), out, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Walk(t.Context(), func(offset int64, f *zip.File) error {
		x, err := a.FileAt(offset)
		if err != nil {
			return err
		}
		r, err := x.Open()
		if err != nil {
			return err
		}
		body, err := io.ReadAll(r)
		closeErr := r.Close()
		if err != nil || closeErr != nil || string(body) != "payload" || x.Name != f.Name {
			return errors.New("invalid large-offset file")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
}

func TestSpoolWriterSecondZIP64LocalOffset(t *testing.T) {
	out, err := os.CreateTemp(t.TempDir(), "archive-*")
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewSpoolWriter(t.Context(), out, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := w.CreateHeader(&zip.FileHeader{Name: "first", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(first, "first-body"); err != nil {
		t.Fatal(err)
	}
	if err := w.finishEntry(); err != nil {
		t.Fatal(err)
	}
	const offset = int64(1<<32 + 64)
	if err := out.Truncate(offset); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Seek(offset, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	second, err := w.CreateHeader(&zip.FileHeader{Name: "second", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(second, "second-body"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	st, _ := out.Stat()
	zr, err := zip.NewReader(out, st.Size())
	if err != nil || len(zr.File) != 2 {
		t.Fatalf("files=%d err=%v", len(zr.File), err)
	}
	for _, want := range []string{"first-body", "second-body"} {
		var found bool
		for _, f := range zr.File {
			r, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(r)
			closeErr := r.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("%s: read=%v close=%v", f.Name, readErr, closeErr)
			}
			if string(body) == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("standard reader missing %q", want)
		}
	}
	a, err := Open(t.Context(), out, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Walk(t.Context(), func(offset int64, f *zip.File) error {
		x, err := a.FileAt(offset)
		if err != nil {
			return err
		}
		r, err := x.Open()
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(r)
		closeErr := r.Close()
		if readErr != nil || closeErr != nil || x.Name != f.Name {
			return errors.New("invalid ZIP64 local offset entry")
		}
		if x.Name == "second" && string(body) != "second-body" {
			return errors.New("second entry body mismatch")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
}

func TestSpoolWriterZIP64EntryCount(t *testing.T) {
	out, err := os.CreateTemp(t.TempDir(), "archive-*")
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewSpoolWriter(t.Context(), out, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 65536 {
		entry, err := w.CreateHeader(&zip.FileHeader{Name: "x", Method: zip.Store})
		if err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
		if _, err := io.WriteString(entry, "x"); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	st, _ := out.Stat()
	zr, err := zip.NewReader(out, st.Size())
	if err != nil || len(zr.File) != 65536 {
		t.Fatalf("files=%d err=%v", len(zr.File), err)
	}
	_ = out.Close()
}

func TestArchiveZIP64EntrySizeSparsePayload(t *testing.T) {
	name := []byte("large.bin")
	const payloadSize int64 = 1<<32 + 8
	localExtra := zip64SizesExtra(payloadSize, payloadSize)
	localLen := int64(30 + len(name) + len(localExtra))
	cdStart := localLen + payloadSize
	centralExtra := zip64SizesExtra(payloadSize, payloadSize)
	central := make([]byte, 46+len(name)+len(centralExtra))
	binary.LittleEndian.PutUint32(central[:4], 0x02014b50)
	binary.LittleEndian.PutUint16(central[4:6], 45)
	binary.LittleEndian.PutUint16(central[6:8], 45)
	binary.LittleEndian.PutUint32(central[20:24], maxUint32)
	binary.LittleEndian.PutUint32(central[24:28], maxUint32)
	binary.LittleEndian.PutUint16(central[28:30], uint16(len(name)))
	binary.LittleEndian.PutUint16(central[30:32], uint16(len(centralExtra)))
	copy(central[46:], name)
	copy(central[46+len(name):], centralExtra)

	local := make([]byte, localLen)
	binary.LittleEndian.PutUint32(local[:4], 0x04034b50)
	binary.LittleEndian.PutUint16(local[4:6], 45)
	binary.LittleEndian.PutUint16(local[8:10], zip.Store)
	binary.LittleEndian.PutUint32(local[18:22], maxUint32)
	binary.LittleEndian.PutUint32(local[22:26], maxUint32)
	binary.LittleEndian.PutUint16(local[26:28], uint16(len(name)))
	binary.LittleEndian.PutUint16(local[28:30], uint16(len(localExtra)))
	copy(local[30:], name)
	copy(local[30+len(name):], localExtra)

	zip64End := cdStart + int64(len(central))
	zip64 := make([]byte, zip64EndLen)
	binary.LittleEndian.PutUint32(zip64[:4], 0x06064b50)
	binary.LittleEndian.PutUint64(zip64[4:12], 44)
	binary.LittleEndian.PutUint16(zip64[12:14], 45)
	binary.LittleEndian.PutUint16(zip64[14:16], 45)
	binary.LittleEndian.PutUint64(zip64[24:32], 1)
	binary.LittleEndian.PutUint64(zip64[32:40], 1)
	binary.LittleEndian.PutUint64(zip64[40:48], uint64(len(central)))
	binary.LittleEndian.PutUint64(zip64[48:56], uint64(cdStart))
	locator := make([]byte, zip64LocatorLen)
	binary.LittleEndian.PutUint32(locator[:4], 0x07064b50)
	binary.LittleEndian.PutUint64(locator[8:16], uint64(zip64End))
	binary.LittleEndian.PutUint32(locator[16:20], 1)
	eocd := make([]byte, endHeaderLen)
	binary.LittleEndian.PutUint32(eocd[:4], 0x06054b50)
	binary.LittleEndian.PutUint16(eocd[8:10], maxUint16)
	binary.LittleEndian.PutUint16(eocd[10:12], maxUint16)
	binary.LittleEndian.PutUint32(eocd[12:16], maxUint32)
	binary.LittleEndian.PutUint32(eocd[16:20], maxUint32)
	eocdStart := zip64End + int64(len(zip64)+len(locator))
	total := eocdStart + int64(len(eocd))
	payloadPrefix := []byte("sparse-payload")
	raw := &sparseReaderAt{size: total, segments: []sparseSegment{
		{off: 0, data: local},
		{off: localLen, data: payloadPrefix},
		{off: cdStart, data: central},
		{off: zip64End, data: zip64},
		{off: zip64End + int64(len(zip64)), data: locator},
		{off: eocdStart, data: eocd},
	}}
	standard, err := zip.NewReader(raw, total)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(t.Context(), raw, total)
	if err != nil {
		t.Fatal(err)
	}
	var offset int64
	if err := a.Walk(t.Context(), func(gotOffset int64, f *zip.File) error {
		offset = gotOffset
		if f.Name != "large.bin" || f.CompressedSize64 != uint64(payloadSize) || f.UncompressedSize64 != uint64(payloadSize) {
			return errors.New("ZIP64 size metadata mismatch")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(standard.File) != 1 || standard.File[0].CompressedSize64 != uint64(payloadSize) || standard.File[0].UncompressedSize64 != uint64(payloadSize) {
		t.Fatalf("standard metadata=%+v", standard.File[0].FileHeader)
	}
	got, err := a.FileAt(offset)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*zip.File{standard.File[0], got} {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		prefix := make([]byte, len(payloadPrefix))
		if _, err := io.ReadFull(r, prefix); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(prefix, payloadPrefix) {
			t.Fatalf("payload prefix=%q", prefix)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArchiveZIP64LocalOffsetBoundaries(t *testing.T) {
	for _, localOffset := range []int64{maxUint32 - 1, maxUint32, maxUint32 + 1} {
		t.Run(fmt.Sprintf("offset-%d", localOffset), func(t *testing.T) {
			raw, size := sparseZIP64LocalOffsetArchive(localOffset)
			standard, err := zip.NewReader(raw, size)
			if err != nil || len(standard.File) != 1 {
				t.Fatalf("standard reader: files=%d err=%v", len(standard.File), err)
			}
			got, err := standard.File[0].Open()
			if err != nil {
				t.Fatal(err)
			}
			payload, err := io.ReadAll(got)
			closeErr := got.Close()
			if err != nil {
				t.Fatal(err)
			}
			if closeErr != nil || string(payload) != "p" {
				t.Fatalf("standard payload=%q close=%v", payload, closeErr)
			}

			a, err := Open(t.Context(), raw, size)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Walk(t.Context(), func(_ int64, f *zip.File) error {
				if f.Name != "x" {
					return errors.New("unexpected file name")
				}
				r, err := f.Open()
				if err != nil {
					return err
				}
				payload, err := io.ReadAll(r)
				closeErr := r.Close()
				if err != nil {
					return err
				}
				if closeErr != nil || string(payload) != "p" {
					return fmt.Errorf("payload=%q close=%v", payload, closeErr)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParseCentralMetaRejectsMissingZIP64Values(t *testing.T) {
	cases := []struct {
		name         string
		compressed   uint32
		uncompressed uint32
		local        uint32
		extraLen     int
	}{
		{name: "missing extra", compressed: maxUint32, extraLen: -1},
		{name: "truncated compressed", compressed: maxUint32, extraLen: 4},
		{name: "truncated local", compressed: maxUint32, uncompressed: maxUint32, local: maxUint32, extraLen: 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := make([]byte, centralHeaderLen)
			binary.LittleEndian.PutUint32(rec[:4], 0x02014b50)
			binary.LittleEndian.PutUint32(rec[20:24], tc.compressed)
			binary.LittleEndian.PutUint32(rec[24:28], tc.uncompressed)
			binary.LittleEndian.PutUint32(rec[42:46], tc.local)
			if tc.extraLen >= 0 {
				extra := make([]byte, 4+tc.extraLen)
				binary.LittleEndian.PutUint16(extra[:2], 1)
				binary.LittleEndian.PutUint16(extra[2:4], uint16(tc.extraLen))
				rec = append(rec, extra...)
				binary.LittleEndian.PutUint16(rec[30:32], uint16(len(extra)))
			}
			if _, err := parseCentralMeta(rec); !errors.Is(err, zip.ErrFormat) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestParseCentralMetaAcceptsZIP64SentinelValues(t *testing.T) {
	rec := make([]byte, centralHeaderLen+4+24)
	binary.LittleEndian.PutUint32(rec[:4], 0x02014b50)
	binary.LittleEndian.PutUint32(rec[20:24], maxUint32)
	binary.LittleEndian.PutUint32(rec[24:28], maxUint32)
	binary.LittleEndian.PutUint32(rec[42:46], maxUint32)
	binary.LittleEndian.PutUint16(rec[30:32], 28)
	binary.LittleEndian.PutUint16(rec[centralHeaderLen:centralHeaderLen+2], 1)
	binary.LittleEndian.PutUint16(rec[centralHeaderLen+2:centralHeaderLen+4], 24)
	for i := range 3 {
		binary.LittleEndian.PutUint64(rec[centralHeaderLen+4+i*8:centralHeaderLen+12+i*8], maxUint32)
	}
	meta, err := parseCentralMeta(rec)
	if err != nil {
		t.Fatal(err)
	}
	if meta.uncompressed != maxUint32 || meta.compressed != maxUint32 || meta.local != maxUint32 {
		t.Fatalf("meta=%+v", meta)
	}
}

func sparseZIP64LocalOffsetArchive(localOffset int64) (*sparseReaderAt, int64) {
	name := []byte("x")
	payload := []byte("p")
	local := make([]byte, 31)
	binary.LittleEndian.PutUint32(local[:4], 0x04034b50)
	binary.LittleEndian.PutUint16(local[4:6], 20)
	binary.LittleEndian.PutUint16(local[8:10], zip.Store)
	binary.LittleEndian.PutUint32(local[14:18], 0x82079eb1)
	binary.LittleEndian.PutUint32(local[18:22], 1)
	binary.LittleEndian.PutUint32(local[22:26], 1)
	binary.LittleEndian.PutUint16(local[26:28], uint16(len(name)))
	copy(local[30:], name)

	var extra []byte
	if localOffset >= maxUint32 {
		extra = make([]byte, 12)
		binary.LittleEndian.PutUint16(extra[:2], 1)
		binary.LittleEndian.PutUint16(extra[2:4], 8)
		binary.LittleEndian.PutUint64(extra[4:12], uint64(localOffset))
	}
	central := make([]byte, centralHeaderLen+len(name)+len(extra))
	binary.LittleEndian.PutUint32(central[:4], 0x02014b50)
	binary.LittleEndian.PutUint16(central[4:6], 45)
	binary.LittleEndian.PutUint16(central[6:8], 45)
	binary.LittleEndian.PutUint32(central[16:20], 0x82079eb1)
	binary.LittleEndian.PutUint32(central[20:24], 1)
	binary.LittleEndian.PutUint32(central[24:28], 1)
	binary.LittleEndian.PutUint16(central[28:30], uint16(len(name)))
	binary.LittleEndian.PutUint16(central[30:32], uint16(len(extra)))
	if localOffset < maxUint32 {
		binary.LittleEndian.PutUint32(central[42:46], uint32(localOffset))
	} else {
		binary.LittleEndian.PutUint32(central[42:46], maxUint32)
	}
	copy(central[46:], name)
	copy(central[46+len(name):], extra)

	cdStart := localOffset + int64(len(local)+len(payload))
	zip64End := cdStart + int64(len(central))
	zip64 := make([]byte, zip64EndLen)
	binary.LittleEndian.PutUint32(zip64[:4], 0x06064b50)
	binary.LittleEndian.PutUint64(zip64[4:12], 44)
	binary.LittleEndian.PutUint16(zip64[12:14], 45)
	binary.LittleEndian.PutUint16(zip64[14:16], 45)
	binary.LittleEndian.PutUint64(zip64[24:32], 1)
	binary.LittleEndian.PutUint64(zip64[32:40], 1)
	binary.LittleEndian.PutUint64(zip64[40:48], uint64(len(central)))
	binary.LittleEndian.PutUint64(zip64[48:56], uint64(cdStart))
	locator := make([]byte, zip64LocatorLen)
	binary.LittleEndian.PutUint32(locator[:4], 0x07064b50)
	binary.LittleEndian.PutUint64(locator[8:16], uint64(zip64End))
	binary.LittleEndian.PutUint32(locator[16:20], 1)
	eocd := make([]byte, endHeaderLen)
	binary.LittleEndian.PutUint32(eocd[:4], 0x06054b50)
	binary.LittleEndian.PutUint16(eocd[8:10], maxUint16)
	binary.LittleEndian.PutUint16(eocd[10:12], maxUint16)
	binary.LittleEndian.PutUint32(eocd[12:16], maxUint32)
	binary.LittleEndian.PutUint32(eocd[16:20], maxUint32)
	eocdStart := zip64End + int64(len(zip64)+len(locator))
	total := eocdStart + int64(len(eocd))
	return &sparseReaderAt{size: total, segments: []sparseSegment{
		{off: localOffset, data: local},
		{off: localOffset + int64(len(local)), data: payload},
		{off: cdStart, data: central},
		{off: zip64End, data: zip64},
		{off: zip64End + int64(len(zip64)), data: locator},
		{off: eocdStart, data: eocd},
	}}, total
}

func zip64SizesExtra(uncompressed, compressed int64) []byte {
	extra := make([]byte, 20)
	binary.LittleEndian.PutUint16(extra[:2], 1)
	binary.LittleEndian.PutUint16(extra[2:4], 16)
	binary.LittleEndian.PutUint64(extra[4:12], uint64(uncompressed))
	binary.LittleEndian.PutUint64(extra[12:20], uint64(compressed))
	return extra
}

type sparseSegment struct {
	off  int64
	data []byte
}

type sparseReaderAt struct {
	size     int64
	segments []sparseSegment
}

type directoryLimitReader struct {
	size int64
	eocd []byte
	err  error
}

func (r *directoryLimitReader) ReadAt(p []byte, off int64) (int, error) {
	if off == 0 {
		return 0, r.err
	}
	if off < 0 {
		return 0, zip.ErrFormat
	}
	if off >= r.size {
		return 0, io.EOF
	}
	short := int64(len(p)) > r.size-off
	if short {
		p = p[:r.size-off]
	}
	clear(p)
	eocdOff := r.size - int64(len(r.eocd))
	start := maxInt64(off, eocdOff)
	end := minInt64(off+int64(len(p)), r.size)
	if start < end {
		copy(p[start-off:end-off], r.eocd[start-eocdOff:end-eocdOff])
	}
	if short {
		return len(p), io.EOF
	}
	return len(p), nil
}

func (r *sparseReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, zip.ErrFormat
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.size {
		return 0, io.EOF
	}
	short := int64(len(p)) > r.size-off
	if short {
		p = p[:r.size-off]
	}
	clear(p)
	for _, segment := range r.segments {
		start := maxInt64(off, segment.off)
		end := minInt64(off+int64(len(p)), segment.off+int64(len(segment.data)))
		if start >= end {
			continue
		}
		copy(p[start-off:end-off], segment.data[start-segment.off:end-segment.off])
	}
	if short {
		return len(p), io.EOF
	}
	return len(p), nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
