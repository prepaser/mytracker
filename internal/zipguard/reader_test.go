package zipguard

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

func fixture(t *testing.T, count int) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for range count {
		if _, err := w.Create("entry"); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func zip64(data []byte) []byte {
	end := len(data) - 22
	e := append([]byte(nil), data[end:]...)
	h := make([]byte, 76)
	binary.LittleEndian.PutUint32(h, 0x06064b50)
	binary.LittleEndian.PutUint64(h[4:], 44)
	binary.LittleEndian.PutUint64(h[24:], uint64(binary.LittleEndian.Uint16(e[8:])))
	binary.LittleEndian.PutUint64(h[32:], uint64(binary.LittleEndian.Uint16(e[10:])))
	binary.LittleEndian.PutUint64(h[40:], uint64(binary.LittleEndian.Uint32(e[12:])))
	binary.LittleEndian.PutUint64(h[48:], uint64(binary.LittleEndian.Uint32(e[16:])))
	binary.LittleEndian.PutUint32(h[56:], 0x07064b50)
	binary.LittleEndian.PutUint64(h[64:], uint64(end))
	binary.LittleEndian.PutUint32(h[72:], 1)
	binary.LittleEndian.PutUint16(e[8:], 0xffff)
	binary.LittleEndian.PutUint16(e[10:], 0xffff)
	return append(append(append([]byte(nil), data[:end]...), h...), e...)
}

func TestValidDirectories(t *testing.T) {
	data := fixture(t, 2)
	for _, input := range [][]byte{data, zip64(data), append([]byte("prefix"), data...), fixture(t, 0)} {
		if _, err := NewReader(t.Context(), bytes.NewReader(input), int64(len(input)), 2); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRejectsForgedAndOversizedDirectories(t *testing.T) {
	data := fixture(t, 4)
	for _, use64 := range []bool{false, true} {
		input := append([]byte(nil), data...)
		if use64 {
			input = zip64(input)
			binary.LittleEndian.PutUint64(input[len(input)-98+24:], 1)
			binary.LittleEndian.PutUint64(input[len(input)-98+32:], 1)
		} else {
			binary.LittleEndian.PutUint16(input[len(input)-22+8:], 1)
			binary.LittleEndian.PutUint16(input[len(input)-22+10:], 1)
		}
		for _, limit := range []int{2, 10} {
			if _, err := NewReader(t.Context(), bytes.NewReader(input), int64(len(input)), limit); !errors.Is(err, zip.ErrFormat) {
				t.Fatalf("zip64=%v limit=%d err=%v", use64, limit, err)
			}
		}
	}
	if _, err := NewReader(t.Context(), bytes.NewReader(data), int64(len(data)), 3); !errors.Is(err, zip.ErrFormat) {
		t.Fatalf("entry limit: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewReader(ctx, bytes.NewReader(data), int64(len(data)), 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestRejectsWrappedDirectoryCount(t *testing.T) {
	data := fixture(t, 1)
	end := data[len(data)-22:]
	offset := int(binary.LittleEndian.Uint32(end[16:]))
	directory := bytes.Repeat(data[offset:len(data)-22], 65537)
	input := append(append(append([]byte(nil), data[:offset]...), directory...), end...)
	binary.LittleEndian.PutUint32(input[len(input)-22+12:], uint32(len(directory)))
	if _, err := NewReader(t.Context(), bytes.NewReader(input), int64(len(input)), 100000); !errors.Is(err, zip.ErrFormat) {
		t.Fatalf("wrapped count accepted: %v", err)
	}
}
