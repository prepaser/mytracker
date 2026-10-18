package zipguard

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
)

const (
	centralHeaderLen = 46
	endHeaderLen     = 22
	zip64EndLen      = 56
	zip64LocatorLen  = 20
	maxUint16        = 1<<16 - 1
	maxUint32        = 1<<32 - 1
)

var errArchiveClosed = errors.New("zipguard: writer closed")

// Archive is a ZIP archive whose central directory is walked without keeping
// all of its files in memory.
type Archive struct {
	r        io.ReaderAt
	size     int64
	base     int64
	cdStart  int64
	cdEnd    int64
	count    uint64
	cdOffset uint64
}

// Open validates the archive's end records and central directory layout.
func Open(ctx context.Context, r io.ReaderAt, size int64) (*Archive, error) {
	return open(ctx, r, size, -1)
}

// OpenWithLimits validates an archive before walking its central directory.
// The bounded form is for archive formats with a fixed entry contract; Open
// remains available for scalable formats such as backups.
func OpenWithLimits(ctx context.Context, r io.ReaderAt, size int64, maxEntries int) (*Archive, error) {
	if maxEntries < 0 {
		return nil, zip.ErrFormat
	}
	return open(ctx, r, size, maxEntries)
}

func open(ctx context.Context, r io.ReaderAt, size int64, maxEntries int) (*Archive, error) {
	if ctx == nil {
		return nil, errors.New("zipguard: nil context")
	}
	if r == nil || size < endHeaderLen {
		return nil, zip.ErrFormat
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	eocd, err := findEnd(ctx, r, size)
	if err != nil {
		return nil, err
	}
	count, cdSize, cdOffset, cdEnd, err := readEnd(ctx, r, size, eocd)
	if err != nil {
		return nil, err
	}
	if maxEntries >= 0 && (count > uint64(maxEntries) || cdSize > maxDirectoryBytes) {
		return nil, zip.ErrFormat
	}
	if cdSize > uint64(math.MaxInt64) || cdOffset > uint64(math.MaxInt64) {
		return nil, zip.ErrFormat
	}
	if cdSize > uint64(cdEnd) {
		return nil, zip.ErrFormat
	}
	cdStart := cdEnd - int64(cdSize)
	base := cdStart - int64(cdOffset)
	if base < 0 || base > size || cdStart < base || cdEnd > size {
		return nil, zip.ErrFormat
	}
	if base > 0 && cdOffset <= uint64(math.MaxInt64) {
		var sig [4]byte
		if err := readFullAt(r, sig[:], int64(cdOffset)); err == nil && binary.LittleEndian.Uint32(sig[:]) == 0x02014b50 {
			return nil, zip.ErrFormat
		}
	}
	a := &Archive{r: r, size: size, base: base, cdStart: cdStart, cdEnd: cdEnd, count: count, cdOffset: cdOffset}
	if err := a.validateDirectory(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// Walk visits central-directory records in archive order.
func (a *Archive) Walk(ctx context.Context, fn func(offset int64, f *zip.File) error) error {
	if a == nil || fn == nil {
		return zip.ErrFormat
	}
	if ctx == nil {
		return errors.New("zipguard: nil context")
	}
	pos := a.cdStart
	for n := uint64(0); n < a.count; n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, _, next, err := a.centralRecord(pos)
		if err != nil {
			return err
		}
		f, err := a.fileAtRecord(pos, rec)
		if err != nil {
			return err
		}
		if err := fn(pos, f); err != nil {
			return err
		}
		pos = next
	}
	if pos != a.cdEnd {
		return zip.ErrFormat
	}
	return nil
}

// FileAt returns a standard-library ZIP file backed by the original archive.
func (a *Archive) FileAt(offset int64) (*zip.File, error) {
	if a == nil || offset < a.cdStart || offset >= a.cdEnd {
		return nil, zip.ErrFormat
	}
	rec, _, _, err := a.centralRecord(offset)
	if err != nil {
		return nil, err
	}
	return a.fileAtRecord(offset, rec)
}

func (a *Archive) validateDirectory(ctx context.Context) error {
	pos := a.cdStart
	for n := uint64(0); n < a.count; n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, meta, next, err := a.centralRecord(pos)
		if err != nil {
			return err
		}
		if err := a.validateLocal(meta); err != nil {
			return err
		}
		_ = rec
		pos = next
	}
	if pos != a.cdEnd {
		return zip.ErrFormat
	}
	return nil
}

type centralMeta struct {
	flags        uint16
	compressed   uint64
	uncompressed uint64
	local        uint64
	zip64        bool
	zip64Sizes   bool
	zip64Local   bool
}

func (a *Archive) centralRecord(pos int64) ([]byte, centralMeta, int64, error) {
	var h [centralHeaderLen]byte
	if pos < a.cdStart || pos > a.cdEnd-int64(len(h)) {
		return nil, centralMeta{}, 0, zip.ErrFormat
	}
	if err := readFullAt(a.r, h[:], pos); err != nil {
		return nil, centralMeta{}, 0, err
	}
	if binary.LittleEndian.Uint32(h[:4]) != 0x02014b50 {
		return nil, centralMeta{}, 0, zip.ErrFormat
	}
	nameLen := int(binary.LittleEndian.Uint16(h[28:30]))
	extraLen := int(binary.LittleEndian.Uint16(h[30:32]))
	commentLen := int(binary.LittleEndian.Uint16(h[32:34]))
	step := int64(centralHeaderLen + nameLen + extraLen + commentLen)
	if step < centralHeaderLen || step > a.cdEnd-pos {
		return nil, centralMeta{}, 0, zip.ErrFormat
	}
	rec := make([]byte, step)
	if err := readFullAt(a.r, rec, pos); err != nil {
		return nil, centralMeta{}, 0, err
	}
	meta, err := parseCentralMeta(rec)
	if err != nil {
		return nil, centralMeta{}, 0, err
	}
	return rec, meta, pos + step, nil
}

func parseCentralMeta(rec []byte) (centralMeta, error) {
	if len(rec) < centralHeaderLen || binary.LittleEndian.Uint32(rec[:4]) != 0x02014b50 {
		return centralMeta{}, zip.ErrFormat
	}
	m := centralMeta{
		flags:        binary.LittleEndian.Uint16(rec[8:10]),
		compressed:   uint64(binary.LittleEndian.Uint32(rec[20:24])),
		uncompressed: uint64(binary.LittleEndian.Uint32(rec[24:28])),
		local:        uint64(binary.LittleEndian.Uint32(rec[42:46])),
	}
	needUncompressed := m.uncompressed == maxUint32
	needCompressed := m.compressed == maxUint32
	needLocal := m.local == maxUint32
	m.zip64Sizes = needUncompressed || needCompressed
	m.zip64Local = needLocal
	m.zip64 = m.zip64Sizes || needLocal
	if !m.zip64 {
		return m, nil
	}
	extraLen := int(binary.LittleEndian.Uint16(rec[30:32]))
	nameLen := int(binary.LittleEndian.Uint16(rec[28:30]))
	start := centralHeaderLen + nameLen
	end := start + extraLen
	if start < centralHeaderLen || end > len(rec) {
		return centralMeta{}, zip.ErrFormat
	}
	var found bool
	extra := rec[start:end]
	for len(extra) >= 4 {
		tag := binary.LittleEndian.Uint16(extra[:2])
		n := int(binary.LittleEndian.Uint16(extra[2:4]))
		extra = extra[4:]
		if n > len(extra) {
			break
		}
		field := extra[:n]
		extra = extra[n:]
		if tag != 1 {
			continue
		}
		found = true
		p := 0
		read := func(dst *uint64) bool {
			if p+8 > len(field) {
				return false
			}
			*dst = binary.LittleEndian.Uint64(field[p : p+8])
			p += 8
			return true
		}
		if needUncompressed && !read(&m.uncompressed) {
			return centralMeta{}, zip.ErrFormat
		}
		if needCompressed && !read(&m.compressed) {
			return centralMeta{}, zip.ErrFormat
		}
		if needLocal && !read(&m.local) {
			return centralMeta{}, zip.ErrFormat
		}
		break
	}
	if !found {
		return centralMeta{}, zip.ErrFormat
	}
	return m, nil
}

func (a *Archive) validateLocal(m centralMeta) error {
	if m.local > uint64(math.MaxInt64) || m.compressed > uint64(math.MaxInt64) || m.uncompressed > uint64(math.MaxInt64) {
		return zip.ErrFormat
	}
	local := a.base + int64(m.local)
	if local < a.base || local < 0 || local > a.cdStart-int64(30) {
		return zip.ErrFormat
	}
	var h [30]byte
	if err := readFullAt(a.r, h[:], local); err != nil {
		return err
	}
	if binary.LittleEndian.Uint32(h[:4]) != 0x04034b50 {
		return zip.ErrFormat
	}
	localLen := int64(30) + int64(binary.LittleEndian.Uint16(h[26:28])) + int64(binary.LittleEndian.Uint16(h[28:30]))
	if localLen < 30 || local > a.cdStart-localLen {
		return zip.ErrFormat
	}
	body := local + localLen
	if m.compressed > uint64(a.cdStart-body) {
		return zip.ErrFormat
	}
	return nil
}

func (a *Archive) fileAtRecord(offset int64, rec []byte) (*zip.File, error) {
	meta, err := parseCentralMeta(rec)
	if err != nil {
		return nil, err
	}
	if err := a.validateLocal(meta); err != nil {
		return nil, err
	}
	local := a.base + int64(meta.local)
	var lh [30]byte
	if err := readFullAt(a.r, lh[:], local); err != nil {
		return nil, err
	}
	localLen := int64(30) + int64(binary.LittleEndian.Uint16(lh[26:28])) + int64(binary.LittleEndian.Uint16(lh[28:30]))
	if localLen < 30 || meta.compressed > uint64(math.MaxInt64) {
		return nil, zip.ErrFormat
	}
	if localLen > math.MaxInt64-int64(meta.compressed) {
		return nil, zip.ErrFormat
	}
	descriptorLen := int64(0)
	if meta.flags&8 != 0 {
		descriptorLen = 16
		if meta.zip64Sizes {
			descriptorLen = 24
		}
	}
	payload := local + localLen
	if payload > a.size || uint64(a.size-payload) < meta.compressed {
		return nil, zip.ErrFormat
	}
	localVirtual := localLen
	payloadVirtual := localVirtual + int64(meta.compressed)
	cdVirtual := payloadVirtual + descriptorLen
	if cdVirtual < payloadVirtual || int64(len(rec)) > math.MaxInt64-cdVirtual {
		return nil, zip.ErrFormat
	}
	cd := append([]byte(nil), rec...)
	// The synthetic archive starts at this entry's local header.
	binary.LittleEndian.PutUint32(cd[42:46], 0)
	if meta.zip64Local {
		if err := patchZip64Offset(cd); err != nil {
			return nil, err
		}
	}
	tail, err := singleEnd(cdVirtual, int64(len(cd)))
	if err != nil {
		return nil, err
	}
	if int64(len(tail)) > math.MaxInt64-(cdVirtual+int64(len(cd))) {
		return nil, zip.ErrFormat
	}
	vr := &virtualReaderAt{
		src:           a.r,
		local:         local,
		localLen:      localLen,
		payload:       payload,
		payloadLen:    int64(meta.compressed),
		descriptor:    payload + int64(meta.compressed),
		descriptorLen: descriptorLen,
		central:       cd,
		centralOff:    cdVirtual,
		tail:          tail,
		tailOff:       cdVirtual + int64(len(cd)),
		total:         cdVirtual + int64(len(cd)) + int64(len(tail)),
	}
	zr, err := zip.NewReader(vr, vr.total)
	if err != nil {
		return nil, err
	}
	if len(zr.File) != 1 {
		return nil, zip.ErrFormat
	}
	return zr.File[0], nil
}

func patchZip64Offset(rec []byte) error {
	nameLen := int(binary.LittleEndian.Uint16(rec[28:30]))
	extraLen := int(binary.LittleEndian.Uint16(rec[30:32]))
	start := centralHeaderLen + nameLen
	end := start + extraLen
	if start < centralHeaderLen || end > len(rec) {
		return zip.ErrFormat
	}
	extra := rec[start:end]
	for len(extra) >= 4 {
		tag := binary.LittleEndian.Uint16(extra[:2])
		n := int(binary.LittleEndian.Uint16(extra[2:4]))
		extra = extra[4:]
		if n > len(extra) {
			return zip.ErrFormat
		}
		if tag == 1 {
			field := extra[:n]
			p := 0
			if binary.LittleEndian.Uint32(rec[24:28]) == maxUint32 {
				p += 8
			}
			if binary.LittleEndian.Uint32(rec[20:24]) == maxUint32 {
				p += 8
			}
			if p+8 > len(field) {
				return zip.ErrFormat
			}
			binary.LittleEndian.PutUint64(field[p:p+8], 0)
			return nil
		}
		extra = extra[n:]
	}
	return zip.ErrFormat
}

func findEnd(ctx context.Context, r io.ReaderAt, size int64) (int64, error) {
	n := min(size, endHeaderLen+maxUint16)
	tail := make([]byte, n)
	if err := readFullAt(r, tail, size-n); err != nil {
		return 0, err
	}
	for i := len(tail) - endHeaderLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:i+4]) != 0x06054b50 {
			continue
		}
		comment := int(binary.LittleEndian.Uint16(tail[i+20 : i+22]))
		if i+endHeaderLen+comment == len(tail) {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			return size - n + int64(i), nil
		}
	}
	return 0, zip.ErrFormat
}

func readEnd(ctx context.Context, r io.ReaderAt, size, eocd int64) (uint64, uint64, uint64, int64, error) {
	return readEndWithLocatorBase(ctx, r, size, eocd, 0)
}

func readEndWithLocatorBase(ctx context.Context, r io.ReaderAt, size, eocd, locatorBase int64) (uint64, uint64, uint64, int64, error) {
	var h [endHeaderLen]byte
	if err := readFullAt(r, h[:], eocd); err != nil {
		return 0, 0, 0, 0, err
	}
	if binary.LittleEndian.Uint32(h[:4]) != 0x06054b50 || binary.LittleEndian.Uint16(h[4:6]) != 0 || binary.LittleEndian.Uint16(h[6:8]) != 0 || binary.LittleEndian.Uint16(h[8:10]) != binary.LittleEndian.Uint16(h[10:12]) {
		return 0, 0, 0, 0, zip.ErrFormat
	}
	count := uint64(binary.LittleEndian.Uint16(h[10:12]))
	cdSize := uint64(binary.LittleEndian.Uint32(h[12:16]))
	cdOffset := uint64(binary.LittleEndian.Uint32(h[16:20]))
	cdEnd := eocd
	if count != maxUint16 && cdSize != maxUint32 && cdOffset != maxUint32 {
		return count, cdSize, cdOffset, cdEnd, nil
	}
	if eocd < zip64LocatorLen {
		return 0, 0, 0, 0, zip.ErrFormat
	}
	var loc [zip64LocatorLen]byte
	if err := readFullAt(r, loc[:], eocd-zip64LocatorLen); err != nil {
		return 0, 0, 0, 0, err
	}
	if binary.LittleEndian.Uint32(loc[:4]) != 0x07064b50 || binary.LittleEndian.Uint32(loc[4:8]) != 0 || binary.LittleEndian.Uint32(loc[16:20]) != 1 {
		return 0, 0, 0, 0, zip.ErrFormat
	}
	pos := binary.LittleEndian.Uint64(loc[8:16])
	if locatorBase < 0 || uint64(locatorBase) > math.MaxUint64-pos {
		return 0, 0, 0, 0, zip.ErrFormat
	}
	pos += uint64(locatorBase)
	if pos > uint64(math.MaxInt64) || pos > uint64(eocd-zip64LocatorLen) {
		return 0, 0, 0, 0, zip.ErrFormat
	}
	var z [zip64EndLen]byte
	if err := readFullAt(r, z[:], int64(pos)); err != nil {
		return 0, 0, 0, 0, err
	}
	recordSize := binary.LittleEndian.Uint64(z[4:12])
	if recordSize > uint64(math.MaxInt64) || binary.LittleEndian.Uint32(z[:4]) != 0x06064b50 || recordSize < 44 || int64(pos)+12+int64(recordSize) != eocd-zip64LocatorLen {
		return 0, 0, 0, 0, zip.ErrFormat
	}
	if binary.LittleEndian.Uint32(z[16:20]) != 0 || binary.LittleEndian.Uint32(z[20:24]) != 0 || binary.LittleEndian.Uint64(z[24:32]) != binary.LittleEndian.Uint64(z[32:40]) {
		return 0, 0, 0, 0, zip.ErrFormat
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, 0, 0, err
	}
	return binary.LittleEndian.Uint64(z[32:40]), binary.LittleEndian.Uint64(z[40:48]), binary.LittleEndian.Uint64(z[48:56]), int64(pos), nil
}

func readFullAt(r io.ReaderAt, p []byte, off int64) error {
	if off < 0 {
		return zip.ErrFormat
	}
	n, err := r.ReadAt(p, off)
	if n != len(p) {
		if err == nil {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	return nil
}

type virtualReaderAt struct {
	src           io.ReaderAt
	local         int64
	localLen      int64
	payload       int64
	payloadLen    int64
	descriptor    int64
	descriptorLen int64
	central       []byte
	centralOff    int64
	tail          []byte
	tailOff       int64
	total         int64
}

func (r *virtualReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, zip.ErrFormat
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.total {
		return 0, io.EOF
	}
	short := false
	if int64(len(p)) > r.total-off {
		p = p[:r.total-off]
		short = true
	}
	got := 0
	for len(p) > 0 {
		pos := off + int64(got)
		var n int64
		var err error
		switch {
		case pos < r.localLen:
			n = minInt64(int64(len(p)), r.localLen-pos)
			var gotN int
			gotN, err = readFromAt(r.src, p[:n], r.local+pos)
			n = int64(gotN)
		case pos < r.localLen+r.payloadLen:
			start := pos - r.localLen
			n = minInt64(int64(len(p)), r.payloadLen-start)
			var gotN int
			gotN, err = readFromAt(r.src, p[:n], r.payload+start)
			n = int64(gotN)
		case pos < r.centralOff:
			start := pos - (r.localLen + r.payloadLen)
			n = minInt64(int64(len(p)), r.descriptorLen-start)
			var gotN int
			gotN, err = readFromAt(r.src, p[:n], r.descriptor+start)
			n = int64(gotN)
		case pos < r.tailOff:
			start := pos - r.centralOff
			n = minInt64(int64(len(p)), int64(len(r.central))-start)
			copy(p[:n], r.central[start:start+n])
		case pos < r.total:
			start := pos - r.tailOff
			n = minInt64(int64(len(p)), int64(len(r.tail))-start)
			copy(p[:n], r.tail[start:start+n])
		}
		if n <= 0 {
			return got, io.ErrUnexpectedEOF
		}
		got += int(n)
		p = p[n:]
		if err != nil {
			return got, err
		}
	}
	if short {
		return got, io.EOF
	}
	return got, nil
}

func readFromAt(r io.ReaderAt, p []byte, off int64) (int, error) {
	n, err := r.ReadAt(p, off)
	if n != len(p) {
		if err == nil {
			return n, io.ErrUnexpectedEOF
		}
		return n, err
	}
	return n, nil
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func singleEnd(cdOffset, cdSize int64) ([]byte, error) {
	if cdOffset < 0 || cdSize < 0 {
		return nil, zip.ErrFormat
	}
	zip64 := cdOffset >= maxUint32 || cdSize >= maxUint32
	if !zip64 {
		var e [endHeaderLen]byte
		binary.LittleEndian.PutUint32(e[:4], 0x06054b50)
		binary.LittleEndian.PutUint16(e[8:10], 1)
		binary.LittleEndian.PutUint16(e[10:12], 1)
		binary.LittleEndian.PutUint32(e[12:16], uint32(cdSize))
		binary.LittleEndian.PutUint32(e[16:20], uint32(cdOffset))
		return e[:], nil
	}
	var out [zip64EndLen + zip64LocatorLen + endHeaderLen]byte
	pos := uint64(cdOffset + cdSize)
	binary.LittleEndian.PutUint32(out[:4], 0x06064b50)
	binary.LittleEndian.PutUint64(out[4:12], 44)
	binary.LittleEndian.PutUint16(out[12:14], 45)
	binary.LittleEndian.PutUint16(out[14:16], 45)
	binary.LittleEndian.PutUint64(out[24:32], 1)
	binary.LittleEndian.PutUint64(out[32:40], 1)
	binary.LittleEndian.PutUint64(out[40:48], uint64(cdSize))
	binary.LittleEndian.PutUint64(out[48:56], uint64(cdOffset))
	loc := zip64EndLen
	binary.LittleEndian.PutUint32(out[loc:loc+4], 0x07064b50)
	binary.LittleEndian.PutUint64(out[loc+8:loc+16], pos)
	binary.LittleEndian.PutUint32(out[loc+16:loc+20], 1)
	e := zip64EndLen + zip64LocatorLen
	binary.LittleEndian.PutUint32(out[e:e+4], 0x06054b50)
	binary.LittleEndian.PutUint16(out[e+8:e+10], maxUint16)
	binary.LittleEndian.PutUint16(out[e+10:e+12], maxUint16)
	binary.LittleEndian.PutUint32(out[e+12:e+16], maxUint32)
	binary.LittleEndian.PutUint32(out[e+16:e+20], maxUint32)
	return out[:], nil
}

func singleDirectory(ctx context.Context, r io.ReaderAt, size, base int64) (int64, int64, error) {
	if size < endHeaderLen {
		return 0, 0, zip.ErrFormat
	}
	var h [endHeaderLen]byte
	if err := readFullAt(r, h[:], size-endHeaderLen); err != nil {
		return 0, 0, err
	}
	if binary.LittleEndian.Uint32(h[:4]) != 0x06054b50 || binary.LittleEndian.Uint16(h[20:22]) != 0 {
		return 0, 0, zip.ErrFormat
	}
	count, cdSize, cdOffset, cdEnd, err := readEndWithLocatorBase(ctx, r, size, size-endHeaderLen, base)
	if err != nil || count != 1 || cdSize > uint64(cdEnd) || cdSize > uint64(math.MaxInt64) || cdOffset > uint64(math.MaxInt64) {
		if err != nil {
			return 0, 0, err
		}
		return 0, 0, zip.ErrFormat
	}
	start := cdEnd - int64(cdSize)
	if start < 0 || cdEnd > size {
		return 0, 0, zip.ErrFormat
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	return start, cdEnd, nil
}

func centralRecordEnd(r io.ReaderAt, start, end int64) (int64, error) {
	var h [centralHeaderLen]byte
	if start < 0 || end < start || start > end-int64(len(h)) {
		return 0, zip.ErrFormat
	}
	if err := readFullAt(r, h[:], start); err != nil {
		return 0, err
	}
	if binary.LittleEndian.Uint32(h[:4]) != 0x02014b50 {
		return 0, zip.ErrFormat
	}
	step := int64(centralHeaderLen) + int64(binary.LittleEndian.Uint16(h[28:30])) + int64(binary.LittleEndian.Uint16(h[30:32])) + int64(binary.LittleEndian.Uint16(h[32:34]))
	if step < centralHeaderLen || step > end-start {
		return 0, zip.ErrFormat
	}
	return start + step, nil
}

// SpoolWriter writes ZIP payloads directly to out and keeps central records in
// a temporary file until Close.
type SpoolWriter struct {
	ctx        context.Context
	out        *os.File
	spool      *os.File
	base       int64
	current    *zip.Writer
	currentOut *countWriter
	count      uint64
	closed     bool
	failed     bool
	buf        []byte
}

// NewSpoolWriter creates a writer using spoolDir for central-directory data.
func NewSpoolWriter(ctx context.Context, out *os.File, spoolDir string) (*SpoolWriter, error) {
	if ctx == nil {
		return nil, errors.New("zipguard: nil context")
	}
	if out == nil {
		return nil, errors.New("zipguard: nil output")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base, err := out.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	spool, err := os.CreateTemp(spoolDir, ".zipguard-central-*")
	if err != nil {
		return nil, err
	}
	return &SpoolWriter{ctx: ctx, out: out, spool: spool, base: base, buf: make([]byte, 32<<10)}, nil
}

// CreateHeader starts one ZIP file entry.
func (w *SpoolWriter) CreateHeader(fh *zip.FileHeader) (io.Writer, error) {
	if w == nil || w.closed || w.failed {
		return nil, errArchiveClosed
	}
	if fh == nil {
		return nil, errors.New("zipguard: nil file header")
	}
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	if err := w.finishEntry(); err != nil {
		return nil, err
	}
	pos, err := w.out.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	cw := &countWriter{w: w.out, n: pos}
	zw := zip.NewWriter(cw)
	if pos < w.base {
		return nil, zip.ErrFormat
	}
	zw.SetOffset(pos - w.base)
	data, err := zw.CreateHeader(fh)
	if err != nil {
		return nil, err
	}
	w.current, w.currentOut = zw, cw
	return &contextWriter{ctx: w.ctx, w: data}, nil
}

// Close writes the spooled central directory and final ZIP end records.
func (w *SpoolWriter) Close() error {
	if w == nil {
		return errArchiveClosed
	}
	if w.closed {
		return errArchiveClosed
	}
	if w.failed {
		return errArchiveClosed
	}
	if err := w.ctx.Err(); err != nil {
		w.failed = true
		_ = w.cleanup()
		return err
	}
	if err := w.finishEntry(); err != nil {
		w.failed = true
		_ = w.cleanup()
		return err
	}
	if err := w.ctx.Err(); err != nil {
		w.failed = true
		_ = w.cleanup()
		return err
	}
	st, err := w.spool.Stat()
	if err != nil {
		w.failed = true
		_ = w.cleanup()
		return err
	}
	cdStart, err := w.out.Seek(0, io.SeekEnd)
	if err != nil {
		w.failed = true
		_ = w.cleanup()
		return err
	}
	if err := w.copySpool(st.Size()); err != nil {
		w.failed = true
		_ = w.cleanup()
		return err
	}
	cdOffset := cdStart - w.base
	if cdOffset < 0 {
		w.failed = true
		_ = w.cleanup()
		return zip.ErrFormat
	}
	tail, err := finalEnd(w.count, st.Size(), cdOffset, w.base)
	if err == nil {
		var n int
		n, err = w.out.Write(tail)
		if err == nil && n != len(tail) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		w.failed = true
		_ = w.cleanup()
		return err
	}
	w.closed = true
	return w.cleanup()
}

// Abort discards the temporary central directory without writing end records.
func (w *SpoolWriter) Abort() error {
	if w == nil {
		return errArchiveClosed
	}
	if w.closed {
		return nil
	}
	w.failed = true
	w.closed = true
	return w.cleanup()
}

func (w *SpoolWriter) finishEntry() error {
	if w.current == nil {
		return nil
	}
	if err := w.ctx.Err(); err != nil {
		w.failed = true
		_ = w.cleanup()
		return err
	}
	if err := w.current.Close(); err != nil {
		w.failed = true
		w.current = nil
		_ = w.cleanup()
		return err
	}
	end := w.currentOut.n
	cdStart, cdEnd, err := singleDirectory(w.ctx, w.out, end, w.base)
	if err != nil {
		w.failed = true
		w.current = nil
		_ = w.cleanup()
		return err
	}
	recordEnd, err := centralRecordEnd(w.out, cdStart, cdEnd)
	if err != nil || recordEnd != cdEnd {
		w.failed = true
		w.current = nil
		_ = w.cleanup()
		if err != nil {
			return err
		}
		return zip.ErrFormat
	}
	if err := copyRange(w.ctx, w.spool, w.out, cdStart, recordEnd, w.buf); err != nil {
		w.failed = true
		w.current = nil
		_ = w.cleanup()
		return err
	}
	if err := w.out.Truncate(cdStart); err != nil {
		w.failed = true
		w.current = nil
		_ = w.cleanup()
		return err
	}
	if _, err := w.out.Seek(cdStart, io.SeekStart); err != nil {
		w.failed = true
		w.current = nil
		_ = w.cleanup()
		return err
	}
	w.current, w.currentOut = nil, nil
	w.count++
	return nil
}

func (w *SpoolWriter) copySpool(size int64) error {
	if _, err := w.spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return copyWithContext(w.ctx, w.out, w.spool, size, w.buf)
}

func copyRange(ctx context.Context, dst *os.File, src io.ReaderAt, start, end int64, buf []byte) error {
	if start < 0 || end < start {
		return zip.ErrFormat
	}
	return copyWithContext(ctx, dst, io.NewSectionReader(src, start, end-start), end-start, buf)
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader, size int64, buf []byte) error {
	if len(buf) == 0 {
		buf = make([]byte, 32<<10)
	}
	for size > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(int64(len(buf)), size)
		got, err := io.ReadFull(src, buf[:n])
		if got > 0 {
			wn, werr := dst.Write(buf[:got])
			if werr != nil {
				return werr
			}
			if wn != got {
				return io.ErrShortWrite
			}
			size -= int64(got)
		}
		if err != nil {
			if err == io.EOF && size == 0 {
				return nil
			}
			return err
		}
	}
	return nil
}

func finalEnd(count uint64, cdSize, cdOffset, base int64) ([]byte, error) {
	if cdSize < 0 || cdOffset < 0 || count > math.MaxInt64 {
		return nil, zip.ErrFormat
	}
	if base < 0 || uint64(base) > math.MaxUint64-uint64(cdOffset)-uint64(cdSize) {
		return nil, zip.ErrFormat
	}
	zip64 := count >= maxUint16 || uint64(cdSize) >= maxUint32 || uint64(cdOffset) >= maxUint32
	if !zip64 {
		var e [endHeaderLen]byte
		binary.LittleEndian.PutUint32(e[:4], 0x06054b50)
		binary.LittleEndian.PutUint16(e[8:10], uint16(count))
		binary.LittleEndian.PutUint16(e[10:12], uint16(count))
		binary.LittleEndian.PutUint32(e[12:16], uint32(cdSize))
		binary.LittleEndian.PutUint32(e[16:20], uint32(cdOffset))
		return e[:], nil
	}
	var out [zip64EndLen + zip64LocatorLen + endHeaderLen]byte
	binary.LittleEndian.PutUint32(out[:4], 0x06064b50)
	binary.LittleEndian.PutUint64(out[4:12], 44)
	binary.LittleEndian.PutUint16(out[12:14], 45)
	binary.LittleEndian.PutUint16(out[14:16], 45)
	binary.LittleEndian.PutUint64(out[24:32], count)
	binary.LittleEndian.PutUint64(out[32:40], count)
	binary.LittleEndian.PutUint64(out[40:48], uint64(cdSize))
	binary.LittleEndian.PutUint64(out[48:56], uint64(cdOffset))
	loc := zip64EndLen
	binary.LittleEndian.PutUint32(out[loc:loc+4], 0x07064b50)
	binary.LittleEndian.PutUint64(out[loc+8:loc+16], uint64(base)+uint64(cdOffset)+uint64(cdSize))
	binary.LittleEndian.PutUint32(out[loc+16:loc+20], 1)
	e := zip64EndLen + zip64LocatorLen
	binary.LittleEndian.PutUint32(out[e:e+4], 0x06054b50)
	binary.LittleEndian.PutUint16(out[e+8:e+10], maxUint16)
	binary.LittleEndian.PutUint16(out[e+10:e+12], maxUint16)
	binary.LittleEndian.PutUint32(out[e+12:e+16], maxUint32)
	binary.LittleEndian.PutUint32(out[e+16:e+20], maxUint32)
	return out[:], nil
}

func (w *SpoolWriter) cleanup() error {
	if w.spool == nil {
		return nil
	}
	err := w.spool.Close()
	if removeErr := os.Remove(w.spool.Name()); err == nil {
		err = removeErr
	}
	w.spool = nil
	return err
}

type countWriter struct {
	w io.Writer
	n int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 {
		if int64(n) > math.MaxInt64-w.n {
			return n, errors.New("zipguard: output too large")
		}
		w.n += int64(n)
	}
	if err == nil && n != len(p) {
		return n, io.ErrShortWrite
	}
	return n, err
}

type contextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w *contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}
