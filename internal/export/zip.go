package export

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode"

	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/zipguard"
)

const Version = 6

const (
	MaxLocations            = 250000
	MaxProofEntries         = 2 * MaxLocations
	MaxVerificationRuns     = 500000
	MaxVerificationAttempts = 1000000
	MaxEntryBytes           = 256 << 20
	MaxManifestBytes        = 64 << 20
	MaxProofBytes           = 8 << 20
	MaxProofTotalBytes      = 1 << 30
)

type Manifest struct {
	Version              int               `json:"version"`
	CreatedAt            string            `json:"created_at"`
	Locations            int               `json:"locations"`
	VerificationRuns     int               `json:"verification_runs"`
	VerificationAttempts int               `json:"verification_attempts"`
	Format               string            `json:"format"`
	ProofSHA256          map[string]string `json:"proof_sha256,omitempty"`
	EntrySHA256          map[string]string `json:"entry_sha256,omitempty"`
}

type VerificationRunRecord struct {
	store.VerificationRun
	ProofSHA256Hex  string `json:"proof_sha256"`
	PolicySHA256Hex string `json:"policy_sha256,omitempty"`
}

type Record struct {
	ID           int64           `json:"id"`
	User         string          `json:"user"`
	Device       string          `json:"device"`
	EventTimeUTC string          `json:"event_time_utc"`
	EventUnix    int64           `json:"event_unix"`
	Latitude     float64         `json:"latitude"`
	Longitude    float64         `json:"longitude"`
	DigestSHA256 string          `json:"digest_sha256"`
	TSANonceHex  string          `json:"tsa_nonce,omitempty"`
	TsaStatus    string          `json:"tsa_status"`
	TsaTimeUTC   string          `json:"tsa_time_utc,omitempty"`
	OtsStatus    string          `json:"ots_status"`
	ReceivedUTC  string          `json:"received_utc"`
	Payload      json.RawMessage `json:"payload"`
	Proofs       ProofPaths      `json:"proofs"`
}

type ProofPaths struct {
	TSR string `json:"tsr,omitempty"`
	OTS string `json:"ots,omitempty"`
}

type Options struct {
	Filter    store.ExportFilter
	ProofsDir string
	Store     *store.Store
	OnCount   func(int)
}

func WriteZip(w io.Writer, opt Options) error {
	return WriteZipContext(context.Background(), w, opt)
}

func WriteZipContext(ctx context.Context, w io.Writer, opt Options) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	unlockProofs, err := opt.Store.LockProofSnapshotContext(ctx)
	if err != nil {
		return err
	}
	defer unlockProofs()
	snapshot, err := opt.Store.BeginExportSnapshotContext(ctx, opt.Filter)
	if err != nil {
		return err
	}
	defer func() { _ = snapshot.Close() }()
	count, err := snapshot.Count()
	if err != nil {
		return err
	}
	if count > MaxLocations {
		return fmt.Errorf("export contains more than %d locations", MaxLocations)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if opt.OnCount != nil {
		opt.OnCount(count)
	}
	work, err := newExportWork()
	if err != nil {
		return err
	}
	defer func() { _ = work.Close() }()
	archive, err := os.CreateTemp(work.dir, ".archive-*.zip")
	if err != nil {
		return err
	}
	defer func() { _ = archive.Close() }()
	index, err := newDigestIndex(ctx, work.dir)
	if err != nil {
		return err
	}
	defer func() { _ = index.Close() }()
	if err := index.Begin(ctx); err != nil {
		return err
	}
	zw, err := zipguard.NewSpoolWriter(ctx, archive, work.dir)
	if err != nil {
		index.Rollback()
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = zw.Abort()
		}
	}()

	readmeBytes := []byte(readme)
	if err := writeBytesContext(ctx, zw, "README.txt", readmeBytes); err != nil {
		return err
	}
	if err := index.Put(ctx, "README.txt", digestBytes(readmeBytes)); err != nil {
		return err
	}
	proofCount := 0
	proofBytes := int64(0)
	err = snapshot.Iterate(500, func(locs []store.Location) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, loc := range locs {
			if err := writeProofFilesContext(ctx, zw, opt.ProofsDir, loc.ID, index, &proofCount, &proofBytes); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	jsonW, err := createEntry(zw, "locations.jsonl")
	if err != nil {
		return err
	}
	jsonHash := sha256.New()
	jsonLimit := &limitWriter{w: jsonW, max: MaxEntryBytes}
	jsonEnc := json.NewEncoder(io.MultiWriter(jsonLimit, jsonHash))
	err = snapshot.Iterate(500, func(locs []store.Location) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, loc := range locs {
			rec, err := toRecord(ctx, loc, index)
			if err != nil {
				return err
			}
			if err := jsonEnc.Encode(rec); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := index.Put(ctx, "locations.jsonl", hex.EncodeToString(jsonHash.Sum(nil))); err != nil {
		return err
	}
	csvW, err := createEntry(zw, "locations.csv")
	if err != nil {
		return err
	}
	csvHash := sha256.New()
	csvLimit := &limitWriter{w: csvW, max: MaxEntryBytes}
	cw := csv.NewWriter(io.MultiWriter(csvLimit, csvHash))
	if err := cw.Write([]string{
		"id", "user", "device", "event_time_utc", "event_unix",
		"latitude", "longitude", "digest_sha256",
		"tsa_status", "tsa_time_utc", "ots_status", "received_utc",
		"proof_tsr", "proof_ots",
	}); err != nil {
		return err
	}
	err = snapshot.Iterate(500, func(locs []store.Location) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, loc := range locs {
			rec, err := toRecord(ctx, loc, index)
			if err != nil {
				return err
			}
			if err := cw.Write([]string{
				fmt.Sprint(rec.ID), csvText(rec.User), csvText(rec.Device), rec.EventTimeUTC, fmt.Sprint(rec.EventUnix),
				fmt.Sprintf("%.6f", rec.Latitude), fmt.Sprintf("%.6f", rec.Longitude), rec.DigestSHA256,
				rec.TsaStatus, rec.TsaTimeUTC, rec.OtsStatus, rec.ReceivedUTC,
				rec.Proofs.TSR, rec.Proofs.OTS,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	if err := index.Put(ctx, "locations.csv", hex.EncodeToString(csvHash.Sum(nil))); err != nil {
		return err
	}
	runsW, err := createEntry(zw, "verification_runs.jsonl")
	if err != nil {
		return err
	}
	runsHash := sha256.New()
	runsLimit := &limitWriter{w: runsW, max: MaxEntryBytes}
	runsEnc := json.NewEncoder(io.MultiWriter(runsLimit, runsHash))
	runCount := 0
	err = snapshot.IterateVerificationRuns(500, func(runs []store.VerificationRun) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for i := range runs {
			if runCount >= MaxVerificationRuns {
				return fmt.Errorf("export contains more than %d verification runs", MaxVerificationRuns)
			}
			rec := VerificationRunRecord{
				VerificationRun: runs[i],
				ProofSHA256Hex:  hex.EncodeToString(runs[i].ProofSHA256),
				PolicySHA256Hex: hex.EncodeToString(runs[i].PolicySHA256),
			}
			if err := runsEnc.Encode(rec); err != nil {
				return err
			}
			runCount++
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := index.Put(ctx, "verification_runs.jsonl", hex.EncodeToString(runsHash.Sum(nil))); err != nil {
		return err
	}
	attemptsW, err := createEntry(zw, "verification_attempts.jsonl")
	if err != nil {
		return err
	}
	attemptsHash := sha256.New()
	attemptsLimit := &limitWriter{w: attemptsW, max: MaxEntryBytes}
	attemptsEnc := json.NewEncoder(io.MultiWriter(attemptsLimit, attemptsHash))
	attemptCount := 0
	err = snapshot.IterateVerificationAttempts(500, func(attempts []store.VerificationAttempt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for i := range attempts {
			if attemptCount >= MaxVerificationAttempts {
				return fmt.Errorf("export contains more than %d verification attempts", MaxVerificationAttempts)
			}
			if err := attemptsEnc.Encode(attempts[i]); err != nil {
				return err
			}
			attemptCount++
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := index.Put(ctx, "verification_attempts.jsonl", hex.EncodeToString(attemptsHash.Sum(nil))); err != nil {
		return err
	}
	if err := writeManifestContext(ctx, zw, index, count, runCount, attemptCount); err != nil {
		return err
	}
	if err := index.Commit(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	closed = true
	if err := archive.Sync(); err != nil {
		return err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return copyContext(ctx, w, archive)
}

func toRecord(ctx context.Context, loc store.Location, entries *digestIndex) (Record, error) {
	rec := Record{
		ID:           loc.ID,
		User:         loc.User,
		Device:       loc.Device,
		EventUnix:    loc.Tst,
		EventTimeUTC: time.Unix(loc.Tst, 0).UTC().Format(time.RFC3339),
		Latitude:     loc.Lat,
		Longitude:    loc.Lon,
		DigestSHA256: hex.EncodeToString(loc.Digest),
		TSANonceHex:  hex.EncodeToString(loc.TSANonce),
		TsaStatus:    loc.TsaStatus,
		OtsStatus:    loc.OtsStatus,
		ReceivedUTC:  time.Unix(loc.CreatedAt, 0).UTC().Format(time.RFC3339),
		Payload:      loc.Payload,
	}
	if loc.TsaTime.Valid {
		rec.TsaTimeUTC = time.Unix(loc.TsaTime.Int64, 0).UTC().Format(time.RFC3339)
	}
	hasTSR, err := entries.Has(ctx, fmt.Sprintf("proofs/%d.tsr", loc.ID))
	if err != nil {
		return Record{}, err
	}
	if hasTSR {
		rec.Proofs.TSR = fmt.Sprintf("proofs/%d.tsr", loc.ID)
	}
	hasOTS, err := entries.Has(ctx, fmt.Sprintf("proofs/%d.ots", loc.ID))
	if err != nil {
		return Record{}, err
	}
	if hasOTS {
		rec.Proofs.OTS = fmt.Sprintf("proofs/%d.ots", loc.ID)
	}
	return rec, nil
}

func csvText(value string) string {
	for _, r := range value {
		if unicode.IsSpace(r) || r == '\ufeff' {
			continue
		}
		switch r {
		case '=', '+', '-', '@':
			return "'" + value
		default:
			return value
		}
	}
	return value
}

func writeProofFilesContext(ctx context.Context, zw zipEntryWriter, dir string, id int64, index *digestIndex, count *int, total *int64) error {
	for _, ext := range []string{".tsr", ".ots"} {
		path := filepath.Join(dir, fmt.Sprintf("%d%s", id, ext))
		name := fmt.Sprintf("proofs/%d%s", id, ext)
		digest, size, written, err := writeFileContext(ctx, zw, name, path)
		if err != nil {
			return err
		}
		if written {
			*count++
			*total += size
			if *count > MaxProofEntries || *total > MaxProofTotalBytes {
				return fmt.Errorf("export proof data exceeds archive limits")
			}
			if err := index.Put(ctx, name, digest); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeManifestContext(ctx context.Context, zw zipEntryWriter, index *digestIndex, locations, runs, attempts int) error {
	w, err := createEntry(zw, "manifest.json")
	if err != nil {
		return err
	}
	limit := &limitWriter{w: contextWriter{ctx: ctx, w: w}, max: MaxManifestBytes}
	write := func(value string) error {
		_, err := limit.Write([]byte(value))
		return err
	}
	createdAt, _ := json.Marshal(time.Now().UTC().Format(time.RFC3339))
	format, _ := json.Marshal("locations.jsonl + locations.csv + proofs/ + verification_*.jsonl")
	if err := write(fmt.Sprintf("{\n  \"version\": %d,\n  \"created_at\": %s,\n  \"locations\": %d,\n  \"verification_runs\": %d,\n  \"verification_attempts\": %d,\n  \"format\": %s,\n  \"entry_sha256\": {\n", Version, createdAt, locations, runs, attempts, format)); err != nil {
		return err
	}
	rows, err := index.Rows(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	first := true
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var name, digest string
		if err := rows.Scan(&name, &digest); err != nil {
			return err
		}
		nameJSON, _ := json.Marshal(name)
		digestJSON, _ := json.Marshal(digest)
		prefix := "    "
		if !first {
			prefix = ",\n    "
		}
		if err := write(fmt.Sprintf("%s%s: %s", prefix, nameJSON, digestJSON)); err != nil {
			return err
		}
		first = false
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return write("\n  }\n}")
}

const readme = `mytracker location export
=========================

For submitting location records elsewhere — not a server backup.

locations.jsonl  — one location per line (JSON, includes raw OwnTracks payload)
locations.csv    — same fields, spreadsheet-friendly
proofs/          — RFC 3161 (.tsr) and OpenTimestamps (.ots) files
verification_runs.jsonl      — persisted proof verification history
verification_attempts.jsonl  — provider attempts for verification runs

Each record has digest_sha256: SHA-256 of the canonical location record used for
timestamping. Proof paths in each record point to files in proofs/.

manifest.json describes this archive.
`

func Filename(t time.Time) string {
	return fmt.Sprintf("mytracker-export-%s.zip", t.UTC().Format("20060102-150405"))
}

func digestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type zipEntryWriter interface {
	CreateHeader(*zip.FileHeader) (io.Writer, error)
}

func createEntry(zw zipEntryWriter, name string) (io.Writer, error) {
	hdr := &zip.FileHeader{Name: filepath.ToSlash(name), Method: zip.Deflate}
	return zw.CreateHeader(hdr)
}

func writeFileContext(ctx context.Context, zw zipEntryWriter, name, path string) (string, int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, false, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, false, nil
		}
		return "", 0, false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", 0, false, err
	}
	if info.IsDir() {
		return "", 0, false, nil
	}
	if info.Size() > MaxProofBytes {
		return "", 0, false, fmt.Errorf("proof %s exceeds size limit", filepath.Base(path))
	}
	hdr, err := zip.FileInfoHeader(info)
	if err != nil {
		return "", 0, false, err
	}
	hdr.Name = filepath.ToSlash(name)
	hdr.Method = zip.Deflate
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return "", 0, false, err
	}
	h := sha256.New()
	counted := &countWriter{w: io.MultiWriter(w, h)}
	if err := copyContext(ctx, counted, io.LimitReader(f, MaxProofBytes+1)); err != nil {
		return "", 0, false, err
	}
	if counted.n > MaxProofBytes {
		return "", 0, false, fmt.Errorf("proof %s exceeds size limit", filepath.Base(path))
	}
	return hex.EncodeToString(h.Sum(nil)), counted.n, true, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

type limitWriter struct {
	w   io.Writer
	n   int64
	max int64
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.max-w.n {
		return 0, fmt.Errorf("archive entry exceeds %d bytes", w.max)
	}
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

func writeBytesContext(ctx context.Context, zw zipEntryWriter, name string, b []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w, err := createEntry(zw, name)
	if err != nil {
		return err
	}
	for len(b) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(len(b), 32<<10)
		written, err := w.Write(b[:n])
		if err != nil {
			return err
		}
		if written != n {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

type contextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}

func copyContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
