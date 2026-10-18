package zipguard

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"io"
)

const maxDirectoryBytes = 64 << 20

func NewReader(ctx context.Context, r io.ReaderAt, size int64, maxEntries int) (*zip.Reader, error) {
	if err := checkDirectory(ctx, r, size, maxEntries); err != nil {
		return nil, err
	}
	return zip.NewReader(r, size)
}

// Count actual directory records before archive/zip allocates metadata; EOCD counts alone are not authoritative.
func checkDirectory(ctx context.Context, r io.ReaderAt, size int64, maxEntries int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if size < 22 || maxEntries < 0 {
		return zip.ErrFormat
	}
	tail := make([]byte, min(size, 22+65535))
	if _, err := r.ReadAt(tail, size-int64(len(tail))); err != nil {
		return err
	}
	i := len(tail) - 22
	for ; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:i+4]) == 0x06054b50 {
			break
		}
	}
	if i < 0 || i+22+int(binary.LittleEndian.Uint16(tail[i+20:i+22])) != len(tail) {
		return zip.ErrFormat
	}
	e := tail[i:]
	if binary.LittleEndian.Uint32(e[4:8]) != 0 ||
		binary.LittleEndian.Uint16(e[8:10]) != binary.LittleEndian.Uint16(e[10:12]) {
		return zip.ErrFormat
	}
	end := size - int64(len(tail)) + int64(i)
	count := uint64(binary.LittleEndian.Uint16(e[10:12]))
	length := uint64(binary.LittleEndian.Uint32(e[12:16]))
	offset := uint64(binary.LittleEndian.Uint32(e[16:20]))
	if count == 0xffff || length == 0xffffffff || offset == 0xffffffff {
		var loc [20]byte
		if end < int64(len(loc)) {
			return zip.ErrFormat
		}
		if _, err := r.ReadAt(loc[:], end-20); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(loc[:4]) != 0x07064b50 ||
			binary.LittleEndian.Uint32(loc[4:8]) != 0 || binary.LittleEndian.Uint32(loc[16:]) != 1 {
			return zip.ErrFormat
		}
		pos := binary.LittleEndian.Uint64(loc[8:16])
		if end < 76 || pos > uint64(end-76) {
			return zip.ErrFormat
		}
		var h [56]byte
		if _, err := r.ReadAt(h[:], int64(pos)); err != nil {
			return err
		}
		recordSize := binary.LittleEndian.Uint64(h[4:12])
		if binary.LittleEndian.Uint32(h[:4]) != 0x06064b50 ||
			recordSize < 44 || recordSize != uint64(end-20)-pos-12 ||
			binary.LittleEndian.Uint64(h[16:24]) != 0 ||
			binary.LittleEndian.Uint64(h[24:32]) != binary.LittleEndian.Uint64(h[32:40]) {
			return zip.ErrFormat
		}
		count = binary.LittleEndian.Uint64(h[32:40])
		length = binary.LittleEndian.Uint64(h[40:48])
		offset = binary.LittleEndian.Uint64(h[48:56])
		end = int64(pos)
	}
	if count > uint64(maxEntries) || length > maxDirectoryBytes || length > uint64(end) || offset > uint64(end)-length {
		return zip.ErrFormat
	}
	start := end - int64(length)
	if uint64(start) != offset {
		// Reject ambiguous offsets instead of letting archive/zip select an unchecked directory.
		var signature [4]byte
		if _, err := r.ReadAt(signature[:], int64(offset)); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(signature[:]) == 0x02014b50 {
			return zip.ErrFormat
		}
	}
	var h [46]byte
	n := uint64(0)
	for pos := start; pos < end; {
		if err := ctx.Err(); err != nil {
			return err
		}
		if end-pos < int64(len(h)) || n >= uint64(maxEntries) {
			return zip.ErrFormat
		}
		if _, err := r.ReadAt(h[:], pos); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(h[:4]) != 0x02014b50 {
			return zip.ErrFormat
		}
		step := int64(len(h)) + int64(binary.LittleEndian.Uint16(h[28:30])) +
			int64(binary.LittleEndian.Uint16(h[30:32])) + int64(binary.LittleEndian.Uint16(h[32:34]))
		if step > end-pos {
			return zip.ErrFormat
		}
		pos += step
		n++
	}
	if n != count {
		return zip.ErrFormat
	}
	return nil
}
