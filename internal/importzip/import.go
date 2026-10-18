package importzip

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/export"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/verification"
	"github.com/prepaser/mytracker/internal/zipguard"
)

const (
	maxJSONLBytes    = export.MaxEntryBytes
	maxManifestBytes = export.MaxManifestBytes
	maxProofBytes    = export.MaxProofBytes
	maxProofTotal    = export.MaxProofTotalBytes
	maxProofEntries  = export.MaxProofEntries
	maxLocations     = export.MaxLocations
	maxRuns          = export.MaxVerificationRuns
	maxAttempts      = export.MaxVerificationAttempts
	maxRecordJSON    = 2 << 20
	maxPayloadBytes  = 1 << 20
	maxIdentity      = 128
	maxTimeText      = 64
	maxProofPath     = 64
	maxResultText    = 32
	maxTriggerText   = 64
	maxOIDText       = 128
	maxSerialText    = 256
	maxSubjectText   = 1024
	maxHashText      = 64
	maxStatusText    = 32
	maxErrorText     = 64
)

type Stamper interface {
	Process(id int64, digest [32]byte)
}

var providerIDPattern = regexp.MustCompile(`^esplora-[1-5]$`)

type Result struct {
	Imported       int  `json:"imported"`
	Skipped        int  `json:"skipped"`
	Proofs         int  `json:"proofs"`
	Verifications  int  `json:"verifications"`
	Committed      bool `json:"committed,omitempty"`
	CleanupPending bool `json:"cleanup_pending,omitempty"`
}

type CommittedError struct{ Err error }

func (e *CommittedError) Error() string {
	return fmt.Sprintf("import committed: %v", e.Err)
}

func (e *CommittedError) Unwrap() error { return e.Err }

func IsCommitted(err error) bool {
	var committed *CommittedError
	return errors.As(err, &committed)
}

type archiveData struct {
	files    archiveFiles
	jsonl    *zip.File
	runs     *zip.File
	attempts *zip.File
	index    *archiveIndex
	valid    int
	verified int
	manifest manifestInfo
}

type manifestInfo struct {
	Version              int
	Locations            int
	VerificationRuns     int
	VerificationAttempts int
}

type archiveFiles interface {
	Walk(context.Context, func(int64, *zip.File) error) error
	FileAt(int64) (*zip.File, error)
}

type ValidatedArchive struct {
	data *archiveData
}

func ValidateArchiveContext(ctx context.Context, r io.ReaderAt, size int64) (*ValidatedArchive, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	files, err := zipguard.OpenWithLimits(ctx, r, size, maxProofEntries+6)
	if err != nil {
		return nil, err
	}
	data, err := validateFilesContext(ctx, files)
	if err != nil {
		return nil, err
	}
	return &ValidatedArchive{data: &data}, nil
}

func (a *ValidatedArchive) Close() error {
	if a == nil || a.data == nil {
		return nil
	}
	data := a.data
	a.data = nil
	return data.index.Close()
}

func Import(r io.ReaderAt, size int64, st *store.Store, proofsDir string, stamp Stamper, maxAttempts ...int) (res Result, err error) {
	return ImportContext(context.Background(), r, size, st, proofsDir, stamp, maxAttempts...)
}

func ImportContext(ctx context.Context, r io.ReaderAt, size int64, st *store.Store, proofsDir string, stamp Stamper, maxAttempts ...int) (res Result, err error) {
	archive, err := ValidateArchiveContext(ctx, r, size)
	if err != nil {
		return res, err
	}
	defer func() { _ = archive.Close() }()
	return ImportValidatedContext(ctx, archive, st, proofsDir, stamp, maxAttempts...)
}

func ImportValidatedContext(ctx context.Context, archive *ValidatedArchive, st *store.Store, proofsDir string, stamp Stamper, maxAttempts ...int) (res Result, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if archive == nil || archive.data == nil {
		return res, fmt.Errorf("validated archive is closed")
	}
	data := *archive.data
	if err := st.RecoverProofImportsContext(ctx, proofsDir); err != nil {
		return res, err
	}
	unlockProofs, err := st.LockProofMutationContext(ctx)
	if err != nil {
		return res, err
	}
	defer unlockProofs()
	proofImport, err := store.BeginProofImport(proofsDir)
	if err != nil {
		return res, err
	}
	committed := false
	defer func() {
		if !committed {
			cleanupCtx := ctx
			if ctx.Err() != nil {
				cleanupCtx = context.WithoutCancel(ctx)
			}
			_ = proofImport.ReconcileContext(cleanupCtx, st)
		}
	}()
	if err := data.index.Begin(ctx); err != nil {
		return res, err
	}
	err = st.ImportAtomicContext(ctx, func(tx *store.ImportTx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := eachRecordContext(ctx, data, func(rec export.Record) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			digest, _ := hex.DecodeString(rec.DigestSHA256)
			tsaNonce, _ := hex.DecodeString(rec.TSANonceHex)
			var raw map[string]any
			_ = json.Unmarshal(rec.Payload, &raw)
			canRec, _ := canonical.FromLocation(rec.User, rec.Device, raw)
			created := time.Now().Unix()
			if t, parseErr := time.Parse(time.RFC3339, rec.ReceivedUTC); parseErr == nil {
				created = t.Unix()
			}
			tsaStatus, otsStatus, statusErr := normalizedStatusContext(ctx, data, rec)
			if statusErr != nil {
				return statusErr
			}
			var tsaTime sql.NullInt64
			if t, parseErr := time.Parse(time.RFC3339, rec.TsaTimeUTC); parseErr == nil {
				tsaTime = sql.NullInt64{Int64: t.Unix(), Valid: true}
			}
			loc := &store.Location{
				User: rec.User, Device: rec.Device, Tst: canRec.Tst,
				Lat: canRec.Lat, Lon: canRec.Lon, Payload: rec.Payload, Digest: digest,
				TSANonce: tsaNonce,
				TsaTime:  tsaTime, TsaStatus: tsaStatus, OtsStatus: otsStatus, CreatedAt: created,
			}
			id, insertErr := tx.InsertLocation(loc, maxAttempts...)
			if insertErr == store.ErrDuplicate {
				existingID, existingDigest, lookupErr := tx.ExistingLocation(loc.User, loc.Device, loc.Tst)
				if lookupErr != nil {
					return lookupErr
				}
				if !bytes.Equal(existingDigest, digest) {
					return fmt.Errorf("duplicate location digest mismatch")
				}
				restored, restoreErr := restoreDuplicateProofs(ctx, tx, data, rec, existingID, digest, proofsDir, proofImport)
				if restoreErr != nil {
					return restoreErr
				}
				res.Proofs += restored
				res.Skipped++
				return nil
			}
			if insertErr != nil {
				return insertErr
			}
			res.Imported++
			verify := false
			for archivePath, ext := range map[string]string{rec.Proofs.TSR: ".tsr", rec.Proofs.OTS: ".ots"} {
				if proof, proofErr := data.file(ctx, archivePath); proofErr != nil {
					return proofErr
				} else if proof != nil {
					b, readErr := readFileContext(ctx, proof, maxProofBytes)
					if readErr != nil {
						return readErr
					}
					if writeErr := proofImport.Add(id, ext, b, digest); writeErr != nil {
						return writeErr
					}
					verify = true
					res.Proofs++
				}
			}
			var queuedDigest []byte
			if stamp != nil && needsStamp(tsaStatus, otsStatus) {
				queuedDigest = digest
			}
			return data.index.mapLocation(ctx, rec.ID, id, verify, queuedDigest)
		})
		if err != nil {
			return err
		}
		if data.runs != nil {
			for _, latestPass := range []bool{false, true} {
				if err := eachVerificationRunContext(ctx, data.runs, func(rec export.VerificationRunRecord) error {
					latest, err := data.index.isLatestRun(ctx, rec.ID, rec.LocationID, rec.Kind)
					if err != nil {
						return err
					}
					if latest != latestPass {
						return nil
					}
					return importVerificationRun(ctx, tx, rec, data.index, &res)
				}); err != nil {
					return err
				}
			}
		}
		if data.attempts != nil {
			if err := eachVerificationAttemptContext(ctx, data.attempts, func(attempt store.VerificationAttempt) error {
				newRunID, ok, err := data.index.runID(ctx, attempt.RunID)
				if err != nil {
					return err
				}
				if !ok {
					return nil
				}
				if err := tx.InsertVerificationAttempt(newRunID, &attempt); err != nil {
					return err
				}
				res.Verifications++
				return nil
			}); err != nil {
				return err
			}
		}
		now := time.Now().Unix()
		if err := data.index.eachVerificationLocation(ctx, func(id int64) error {
			for _, kind := range []string{"tsa", "ots"} {
				if err := tx.EnqueueVerification(id, kind, "import", now); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := proofImport.InstallContext(ctx); err != nil {
			return err
		}
		if err := tx.MarkProofImport(proofImport); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return data.index.Commit()
	})
	if err != nil {
		data.index.Rollback()
		return res, err
	}
	committed = true
	if err := proofImport.ReconcileContext(context.WithoutCancel(ctx), st); err != nil {
		res.Committed = true
		res.CleanupPending = true
		return res, &CommittedError{Err: err}
	}
	if stamp != nil {
		if err := data.index.eachQueuedStamp(ctx, func(id int64, digest []byte) {
			var digestArray [32]byte
			copy(digestArray[:], digest)
			stamp.Process(id, digestArray)
		}); err != nil {
			res.Committed = true
			return res, &CommittedError{Err: err}
		}
	}
	return res, nil
}

func restoreDuplicateProofs(ctx context.Context, tx *store.ImportTx, data archiveData, rec export.Record, locationID int64, digest []byte, proofsDir string, proofImport *store.ProofImport) (int, error) {
	restored := 0
	tsaStatus, otsStatus, err := normalizedStatusContext(ctx, data, rec)
	if err != nil {
		return restored, err
	}
	tsaNonce, _ := hex.DecodeString(rec.TSANonceHex)
	var tsaTime sql.NullInt64
	if t, err := time.Parse(time.RFC3339, rec.TsaTimeUTC); err == nil {
		tsaTime = sql.NullInt64{Int64: t.Unix(), Valid: true}
	}
	for _, proof := range []struct {
		archivePath string
		ext         string
		kind        string
	}{{rec.Proofs.TSR, ".tsr", "tsa"}, {rec.Proofs.OTS, ".ots", "ots"}} {
		entry, err := data.file(ctx, proof.archivePath)
		if err != nil {
			return restored, err
		}
		if entry == nil {
			continue
		}
		destination := filepath.Join(proofsDir, fmt.Sprintf("%d%s", locationID, proof.ext))
		info, err := os.Lstat(destination)
		if err == nil {
			if !info.Mode().IsRegular() {
				return restored, fmt.Errorf("existing proof is not a regular file")
			}
			continue
		}
		if !os.IsNotExist(err) {
			return restored, err
		}
		body, err := readFileContext(ctx, entry, maxProofBytes)
		if err != nil {
			return restored, err
		}
		if err := proofImport.Add(locationID, proof.ext, body, digest); err != nil {
			return restored, err
		}
		status := tsaStatus
		if proof.kind == "ots" {
			status = otsStatus
		}
		if err := tx.RestoreImportedProof(locationID, proof.kind, status, tsaTime, tsaNonce); err != nil {
			return restored, err
		}
		if err := tx.EnqueueVerification(locationID, proof.kind, "import", time.Now().Unix()); err != nil {
			return restored, err
		}
		restored++
	}
	return restored, nil
}

func importVerificationRun(ctx context.Context, tx *store.ImportTx, rec export.VerificationRunRecord, index *archiveIndex, res *Result) error {
	oldLocationID := rec.LocationID
	newLocationID, ok, err := index.locationID(ctx, oldLocationID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if rec.ProofSHA256Hex != "" {
		want, err := hex.DecodeString(rec.ProofSHA256Hex)
		if err != nil || len(want) != sha256.Size {
			return fmt.Errorf("verification proof digest is invalid")
		}
		rec.ProofSHA256 = want
	}
	if rec.PolicySHA256Hex != "" {
		policyHash, err := hex.DecodeString(rec.PolicySHA256Hex)
		if err != nil || len(policyHash) != sha256.Size {
			return fmt.Errorf("verification policy digest is invalid")
		}
		rec.PolicySHA256 = policyHash
	}
	oldRunID := rec.ID
	rec.LocationID = newLocationID
	newRunID, err := tx.InsertVerification(&rec.VerificationRun)
	if err != nil {
		return err
	}
	if err := index.mapRun(ctx, oldRunID, newRunID); err != nil {
		return err
	}
	if err := index.markVerify(ctx, oldLocationID); err != nil {
		return err
	}
	res.Verifications++
	return nil
}

func Verify(r io.ReaderAt, size int64) (Result, error) {
	return VerifyContext(context.Background(), r, size)
}

func VerifyContext(ctx context.Context, r io.ReaderAt, size int64) (Result, error) {
	archive, err := ValidateArchiveContext(ctx, r, size)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = archive.Close() }()
	data := archive.data
	proofs, err := data.index.entryCount(ctx, "proofs/")
	if err != nil {
		return Result{}, err
	}
	return Result{
		Imported: data.valid, Proofs: proofs,
		Verifications: data.verified,
	}, nil
}

func validateFilesContext(ctx context.Context, files archiveFiles) (data archiveData, err error) {
	index, err := newArchiveIndex(ctx)
	if err != nil {
		return data, err
	}
	data = archiveData{files: files, index: index}
	defer func() {
		if err != nil {
			_ = index.Close()
		}
	}()
	if err = index.Begin(ctx); err != nil {
		return data, err
	}
	seen := make(map[string]bool)
	totalProofs := uint64(0)
	entryCount := 0
	proofCount := 0
	err = files.Walk(ctx, func(offset int64, f *zip.File) error {
		if f == nil {
			return zip.ErrFormat
		}
		name := filepath.ToSlash(f.Name)
		if _, exists, lookupErr := index.entry(ctx, name); lookupErr != nil {
			return lookupErr
		} else if exists {
			if name == "manifest.json" {
				return fmt.Errorf("duplicate manifest.json")
			}
			return fmt.Errorf("duplicate archive entry %q", name)
		}
		entryCount++
		if entryCount > maxProofEntries+6 {
			return fmt.Errorf("too many archive entries")
		}
		if name == "manifest.json" && f.UncompressedSize64 > maxManifestBytes {
			return fmt.Errorf("manifest too large")
		}
		if strings.HasPrefix(name, "proofs/") {
			if filepath.ToSlash(filepath.Clean(name)) != name {
				return fmt.Errorf("unsafe proof path %q", name)
			}
			if name != "proofs/"+filepath.Base(name) {
				return fmt.Errorf("invalid proof path %q", name)
			}
			if _, ok := ParseProofID(name); !ok {
				return fmt.Errorf("invalid proof path %q", name)
			}
			if proofCount >= maxProofEntries {
				return fmt.Errorf("too many proof entries")
			}
			proofCount++
			if f.UncompressedSize64 > maxProofBytes || totalProofs+f.UncompressedSize64 > maxProofTotal {
				return fmt.Errorf("proof data too large")
			}
			totalProofs += f.UncompressedSize64
		}
		switch {
		case name == "locations.jsonl":
			if data.jsonl != nil {
				return fmt.Errorf("duplicate locations.jsonl")
			}
			if f.UncompressedSize64 > maxJSONLBytes {
				return fmt.Errorf("locations.jsonl too large")
			}
			data.jsonl = f
		case name == "verification_runs.jsonl":
			if data.runs != nil || f.UncompressedSize64 > maxJSONLBytes {
				return fmt.Errorf("invalid verification_runs.jsonl")
			}
			data.runs = f
		case name == "verification_attempts.jsonl":
			if data.attempts != nil || f.UncompressedSize64 > maxJSONLBytes {
				return fmt.Errorf("invalid verification_attempts.jsonl")
			}
			data.attempts = f
		case strings.HasPrefix(name, "proofs/"):
		case name == "manifest.json", name == "locations.csv", name == "README.txt":
			if seen[name] {
				return fmt.Errorf("duplicate %s", name)
			}
			seen[name] = true
			if f.UncompressedSize64 > maxJSONLBytes {
				return fmt.Errorf("%s too large", name)
			}
		default:
			return fmt.Errorf("unexpected archive entry %q", name)
		}
		digest, digestErr := zipEntrySHA256Context(ctx, f)
		if digestErr != nil {
			return digestErr
		}
		if err := index.addEntry(ctx, indexedEntry{Name: name, Offset: offset, Size: f.UncompressedSize64, Digest: digest}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return data, err
	}
	manifestFile, ok, err := index.entry(ctx, "manifest.json")
	if err != nil {
		return data, err
	}
	if !ok {
		return data, fmt.Errorf("manifest.json required")
	}
	manifest, err := parseManifestContext(ctx, files, manifestFile.Offset, index)
	if err != nil {
		return data, err
	}
	data.manifest = manifest
	if data.jsonl == nil {
		return data, fmt.Errorf("locations.jsonl not found")
	}
	if manifest.Version >= 3 && (data.runs == nil || data.attempts == nil) {
		return data, fmt.Errorf("verification JSONL files are required")
	}
	if manifest.Version >= 4 && (manifest.Locations > maxLocations ||
		manifest.VerificationRuns > maxRuns || manifest.VerificationAttempts > maxAttempts) {
		return data, fmt.Errorf("archive record count exceeds limit")
	}
	if err = validateProofManifestContext(ctx, data); err != nil {
		return data, err
	}
	if err = validateEntryManifestContext(ctx, data); err != nil {
		return data, err
	}
	err = eachRecordContext(ctx, data, func(rec export.Record) error {
		if err := recordLimit(data.valid, maxLocations, "locations"); err != nil {
			return err
		}
		if err := index.addLocation(ctx, rec.ID); err != nil {
			return fmt.Errorf("duplicate location id %d", rec.ID)
		}
		if err := index.addProofReference(ctx, rec.Proofs.TSR); err != nil {
			return fmt.Errorf("duplicate TSA proof reference for location %d", rec.ID)
		}
		if err := index.addProofReference(ctx, rec.Proofs.OTS); err != nil {
			return fmt.Errorf("duplicate OTS proof reference for location %d", rec.ID)
		}
		data.valid++
		return nil
	})
	if err != nil {
		return data, err
	}
	proofRefs, err := index.proofReferenceCount(ctx)
	if err != nil {
		return data, err
	}
	proofCount, err = index.entryCount(ctx, "proofs/")
	if err != nil {
		return data, err
	}
	if proofRefs != proofCount {
		return data, fmt.Errorf("archive contains unreferenced proofs")
	}
	runCount := 0
	if data.runs != nil {
		if err := eachVerificationRunContext(ctx, data.runs, func(rec export.VerificationRunRecord) error {
			if err := recordLimit(runCount, maxRuns, "verification runs"); err != nil {
				return err
			}
			hasLocation, err := index.hasLocation(ctx, rec.LocationID)
			if err != nil {
				return err
			}
			if !validVerificationRun(rec) || !hasLocation {
				return fmt.Errorf("invalid verification run")
			}
			if err := index.addRun(ctx, rec.ID, rec.LocationID, rec.Kind); err != nil {
				return fmt.Errorf("invalid verification run")
			}
			runCount++
			data.verified++
			if rec.ProofSHA256Hex == "" {
				if rec.PolicySHA256Hex == "" {
					return nil
				}
			} else {
				want, err := hex.DecodeString(rec.ProofSHA256Hex)
				if err != nil || len(want) != sha256.Size {
					return fmt.Errorf("invalid verification proof digest")
				}
			}
			if rec.PolicySHA256Hex != "" {
				policyHash, err := hex.DecodeString(rec.PolicySHA256Hex)
				if err != nil || len(policyHash) != sha256.Size {
					return fmt.Errorf("invalid verification policy digest")
				}
			}
			return nil
		}); err != nil {
			return data, err
		}
	}
	if data.attempts != nil {
		attemptCount := 0
		if err := eachVerificationAttemptContext(ctx, data.attempts, func(attempt store.VerificationAttempt) error {
			if err := recordLimit(attemptCount, maxAttempts, "verification attempts"); err != nil {
				return err
			}
			hasRun, err := index.hasRun(ctx, attempt.RunID)
			if err != nil {
				return err
			}
			if !hasRun || !validVerificationAttempt(attempt) {
				return fmt.Errorf("invalid verification attempt")
			}
			count, err := index.providerCount(ctx, attempt.RunID)
			if err != nil {
				return err
			}
			if count >= store.MaxVerificationAttempts {
				return fmt.Errorf("too many verification attempts for run")
			}
			if err := index.addProvider(ctx, attempt.RunID, attempt.ProviderID); err != nil {
				return fmt.Errorf("duplicate verification provider")
			}
			attemptCount++
			data.verified++
			return nil
		}); err != nil {
			return data, err
		}
	}
	if manifest.Version >= 4 && (data.valid != manifest.Locations ||
		runCount != manifest.VerificationRuns || data.verified-runCount != manifest.VerificationAttempts) {
		return data, fmt.Errorf("archive record counts do not match manifest")
	}
	if err := index.Commit(); err != nil {
		return data, err
	}
	return data, nil
}

func recordLimit(count, limit int, label string) error {
	if count >= limit {
		return fmt.Errorf("too many %s", label)
	}
	return nil
}

func (data archiveData) file(ctx context.Context, name string) (*zip.File, error) {
	entry, ok, err := data.index.entry(ctx, name)
	if err != nil || !ok {
		return nil, err
	}
	return data.files.FileAt(entry.Offset)
}

func normalizedStatusContext(ctx context.Context, data archiveData, rec export.Record) (string, string, error) {
	proofs := make(map[string]*zip.File, 2)
	for _, name := range []string{rec.Proofs.TSR, rec.Proofs.OTS} {
		if name == "" {
			continue
		}
		_, ok, err := data.index.entry(ctx, name)
		if err != nil {
			return "", "", err
		}
		if ok {
			proofs[name] = &zip.File{}
		}
	}
	tsa, ots := normalizedStatus(rec, proofs)
	return tsa, ots, nil
}

func validateProofManifestContext(ctx context.Context, data archiveData) error {
	if data.manifest.Version != 5 {
		return nil
	}
	expected, err := data.index.manifestDigestCount(ctx, "proof")
	if err != nil {
		return err
	}
	proofs, err := data.index.entryCount(ctx, "proofs/")
	if err != nil {
		return err
	}
	if expected != proofs {
		return fmt.Errorf("proof manifest count mismatch")
	}
	return data.index.eachEntryWithManifest(ctx, "proof", func(entry indexedEntry, want string, ok bool) error {
		if !strings.HasPrefix(entry.Name, "proofs/") {
			return nil
		}
		if !ok {
			return fmt.Errorf("invalid proof manifest digest")
		}
		decoded, err := hex.DecodeString(want)
		if err != nil || len(decoded) != sha256.Size {
			return fmt.Errorf("invalid proof manifest digest")
		}
		if !strings.EqualFold(entry.Digest, want) {
			return fmt.Errorf("proof manifest digest mismatch")
		}
		return nil
	})
}

func validateEntryManifestContext(ctx context.Context, data archiveData) error {
	if data.manifest.Version < 6 {
		return nil
	}
	expected, err := data.index.manifestDigestCount(ctx, "entry")
	if err != nil {
		return err
	}
	actual, err := data.index.entryCount(ctx, "")
	if err != nil {
		return err
	}
	if expected != actual-1 {
		return fmt.Errorf("entry manifest count mismatch")
	}
	return data.index.eachEntryWithManifest(ctx, "entry", func(entry indexedEntry, want string, ok bool) error {
		if !ok {
			return fmt.Errorf("invalid entry manifest digest")
		}
		decoded, err := hex.DecodeString(want)
		if err != nil || len(decoded) != sha256.Size {
			return fmt.Errorf("invalid entry manifest digest")
		}
		if !strings.EqualFold(entry.Digest, want) {
			return fmt.Errorf("entry manifest digest mismatch")
		}
		return nil
	})
}

func zipEntrySHA256Context(ctx context.Context, f *zip.File) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	if err := copyContext(ctx, h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var errRecordTooLarge = errors.New("JSON record too large")

type recordReader struct {
	r         *bufio.Reader
	total     int64
	remaining int64
	skipSpace bool
}

func (r *recordReader) Read(p []byte) (int, error) {
	if r.skipSpace {
		for {
			b, err := r.r.ReadByte()
			if err != nil {
				return 0, err
			}
			if jsonSpace(b) {
				continue
			}
			if err := r.r.UnreadByte(); err != nil {
				return 0, err
			}
			r.skipSpace = false
			break
		}
	}
	if r.remaining <= 0 {
		return 0, errRecordTooLarge
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.r.Read(p)
	r.remaining -= int64(n)
	r.total += int64(n)
	return n, err
}

func jsonSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }

func bufferedJSONSpace(dec *json.Decoder) (int64, bool) {
	reader := dec.Buffered()
	var count int64
	var buf [512]byte
	for {
		n, err := reader.Read(buf[:])
		for _, b := range buf[:n] {
			if !jsonSpace(b) {
				return count, false
			}
			count++
		}
		if err != nil {
			return count, true
		}
	}
}

func eachRecordContext(ctx context.Context, data archiveData, fn func(export.Record) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rc, err := data.jsonl.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	reader := &recordReader{r: bufio.NewReader(io.LimitReader(contextReader{ctx: ctx, r: rc}, maxJSONLBytes+1))}
	dec := json.NewDecoder(reader)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var rec export.Record
		space, onlySpace := bufferedJSONSpace(dec)
		start := dec.InputOffset() + space
		// Account for bytes the decoder has already buffered for the next value.
		reader.remaining = maxRecordJSON + 1 - (reader.total - start)
		reader.skipSpace = onlySpace
		if err := dec.Decode(&rec); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		if dec.InputOffset()-start > maxRecordJSON {
			return errRecordTooLarge
		}
		if !validRecord(rec, data.manifest.Version) {
			return fmt.Errorf("invalid location record")
		}
		if rec.Proofs.TSR != "" {
			_, proofExists, err := data.index.entry(ctx, rec.Proofs.TSR)
			if err != nil {
				return err
			}
			if rec.Proofs.TSR != fmt.Sprintf("proofs/%d.tsr", rec.ID) || !proofExists {
				return fmt.Errorf("invalid TSA proof reference for location %d", rec.ID)
			}
		}
		if rec.Proofs.OTS != "" {
			_, proofExists, err := data.index.entry(ctx, rec.Proofs.OTS)
			if err != nil {
				return err
			}
			if rec.Proofs.OTS != fmt.Sprintf("proofs/%d.ots", rec.ID) || !proofExists {
				return fmt.Errorf("invalid OTS proof reference for location %d", rec.ID)
			}
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return nil
}

func eachVerificationRunContext(ctx context.Context, file *zip.File, fn func(export.VerificationRunRecord) error) error {
	return eachJSONLContext(ctx, file, func(dec *json.Decoder) error {
		var rec export.VerificationRunRecord
		if err := dec.Decode(&rec); err != nil {
			return err
		}
		return fn(rec)
	})
}

func eachVerificationAttemptContext(ctx context.Context, file *zip.File, fn func(store.VerificationAttempt) error) error {
	return eachJSONLContext(ctx, file, func(dec *json.Decoder) error {
		var attempt store.VerificationAttempt
		if err := dec.Decode(&attempt); err != nil {
			return err
		}
		return fn(attempt)
	})
}

func eachJSONLContext(ctx context.Context, file *zip.File, fn func(*json.Decoder) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rc, err := file.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	br := bufio.NewReader(io.LimitReader(contextReader{ctx: ctx, r: rc}, maxJSONLBytes+1))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := readJSONLine(br)
		if err != nil {
			return err
		}
		if line == nil {
			return nil
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		if err := fn(dec); err != nil {
			return err
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			if err == nil {
				return fmt.Errorf("multiple JSON values in record")
			}
			return err
		}
	}
}

func readJSONLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, prefix, err := br.ReadLine()
		if err != nil {
			if err == io.EOF {
				line = append(line, part...)
				if len(line) == 0 {
					return nil, nil
				}
				if len(line) > maxRecordJSON {
					return nil, fmt.Errorf("JSONL record too large")
				}
				return line, nil
			}
			return nil, err
		}
		line = append(line, part...)
		if len(line) > maxRecordJSON {
			return nil, fmt.Errorf("JSONL record too large")
		}
		if !prefix {
			return line, nil
		}
	}
}

func validVerificationRun(rec export.VerificationRunRecord) bool {
	validResult := map[string]bool{
		"verified": true, "degraded": true, "pending": true, "inconclusive": true,
		"invalid": true, "error": true, "missing": true,
	}
	return rec.ID > 0 && rec.LocationID > 0 && (rec.Kind == "tsa" || rec.Kind == "ots") &&
		validText(rec.Trigger, maxTriggerText) && validText(rec.Result, maxResultText) &&
		validText(rec.TSAPolicyOID, maxOIDText) && validText(rec.TSASerial, maxSerialText) &&
		validText(rec.TSASignerSubject, maxSubjectText) && validText(rec.TSASignerFingerprint, maxHashText) &&
		validText(rec.BitcoinBlockHash, maxHashText) && validText(rec.ErrorCode, maxErrorText) &&
		validText(rec.TSANonceStatus, maxStatusText) &&
		validText(rec.TSAPolicyStatus, maxStatusText) && validText(rec.TSATrustMode, maxStatusText) &&
		validText(rec.ProofSHA256Hex, maxHashText) && validText(rec.PolicySHA256Hex, maxHashText) &&
		validEvidenceNumbers(rec) &&
		validResult[rec.Result] && rec.CheckedAt >= 0 && rec.CheckedAt <= time.Now().Add(5*time.Minute).Unix() &&
		validVerificationErrorCode(rec.ErrorCode)
}

func validVerificationAttempt(attempt store.VerificationAttempt) bool {
	validResult := map[string]bool{"verified": true, "pending": true, "invalid": true, "error": true}
	return attempt.ID > 0 && attempt.RunID > 0 && providerIDPattern.MatchString(attempt.ProviderID) &&
		validText(attempt.Result, maxResultText) && validText(attempt.BlockHash, maxHashText) &&
		validText(attempt.ErrorCode, maxErrorText) && attempt.DurationMS >= 0 && attempt.TipHeight >= 0 &&
		attempt.Confirmations >= 0 && validBlockHeight(attempt.BlockHeight) && validResult[attempt.Result] &&
		validVerificationErrorCode(attempt.ErrorCode)
}

func validBlockHeight(height *int64) bool {
	return height == nil || *height >= 0
}

func validEvidenceNumbers(rec export.VerificationRunRecord) bool {
	return (rec.TSAGenTime == nil || *rec.TSAGenTime >= 0) &&
		(rec.BitcoinHeight == nil || *rec.BitcoinHeight >= 0) && rec.TSAAccuracyMS >= 0 &&
		rec.AgreeingSources >= 0 && rec.SourceCount >= 0 && rec.BitcoinConfirmations >= 0
}

func validText(value string, max int) bool {
	return len(value) <= max && utf8.ValidString(value)
}

func validVerificationErrorCode(code string) bool {
	return verification.ValidPersistedErrorCode(code)
}

func validRecord(rec export.Record, version int) bool {
	if rec.ID <= 0 || !validIdentity(rec.User) || !validIdentity(rec.Device) ||
		!validText(rec.EventTimeUTC, maxTimeText) || !validText(rec.ReceivedUTC, maxTimeText) ||
		!validText(rec.TsaTimeUTC, maxTimeText) || !validText(rec.TSANonceHex, maxHashText) ||
		!validText(rec.TsaStatus, maxStatusText) || !validText(rec.OtsStatus, maxStatusText) ||
		!validText(rec.Proofs.TSR, maxProofPath) || !validText(rec.Proofs.OTS, maxProofPath) ||
		len(rec.Payload) == 0 || len(rec.Payload) > maxPayloadBytes {
		return false
	}
	digest, err := hex.DecodeString(rec.DigestSHA256)
	if err != nil || len(digest) != 32 {
		return false
	}
	if rec.TSANonceHex != "" {
		nonce, err := hex.DecodeString(rec.TSANonceHex)
		if err != nil || len(nonce) > 16 {
			return false
		}
	}
	var raw map[string]any
	if json.Unmarshal(rec.Payload, &raw) != nil {
		return false
	}
	canRec, err := canonical.FromLocation(rec.User, rec.Device, raw)
	if err != nil || canRec.Tst != rec.EventUnix {
		return false
	}
	if !validRFC3339Unix(canRec.Tst) {
		return false
	}
	if version >= 6 && !validCurrentRecordMetadata(rec, canRec) {
		return false
	}
	want, err := canRec.Digest()
	return err == nil && bytes.Equal(want[:], digest)
}

func validRFC3339Unix(value int64) bool {
	text := time.Unix(value, 0).UTC().Format(time.RFC3339)
	parsed, err := time.Parse(time.RFC3339, text)
	return err == nil && parsed.Unix() == value
}

func validIdentity(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxIdentity || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validCurrentRecordMetadata(rec export.Record, canonicalRecord canonical.Record) bool {
	eventTime, err := time.Parse(time.RFC3339, rec.EventTimeUTC)
	if err != nil || eventTime.Unix() != rec.EventUnix || rec.Latitude != canonicalRecord.Lat || rec.Longitude != canonicalRecord.Lon {
		return false
	}
	if received, err := time.Parse(time.RFC3339, rec.ReceivedUTC); err != nil || received.Unix() < 0 {
		return false
	}
	if rec.TsaTimeUTC != "" {
		if tsaTime, err := time.Parse(time.RFC3339, rec.TsaTimeUTC); err != nil || tsaTime.Unix() < 0 {
			return false
		}
	}
	validTSA := rec.TsaStatus == "pending" || rec.TsaStatus == "ok" || rec.TsaStatus == "fail"
	validOTS := rec.OtsStatus == "pending" || rec.OtsStatus == "submitted" || rec.OtsStatus == "confirmed" || rec.OtsStatus == "fail"
	return validTSA && validOTS
}

func normalizedStatus(rec export.Record, proofs map[string]*zip.File) (string, string) {
	tsa, ots := rec.TsaStatus, rec.OtsStatus
	if tsa == "ok" && proofs[rec.Proofs.TSR] == nil {
		tsa = "pending"
	}
	if (ots == "submitted" || ots == "confirmed") && proofs[rec.Proofs.OTS] == nil {
		ots = "pending"
	}
	if tsa != "ok" && tsa != "fail" {
		tsa = "pending"
	}
	if ots != "submitted" && ots != "confirmed" && ots != "fail" {
		ots = "pending"
	}
	return tsa, ots
}

func readFileContext(ctx context.Context, f *zip.File, max int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var b bytes.Buffer
	if f.UncompressedSize64 < uint64(max+1) {
		b.Grow(int(f.UncompressedSize64))
	}
	err = copyContext(ctx, &b, io.LimitReader(rc, max+1))
	if err != nil {
		return nil, err
	}
	if int64(b.Len()) > max {
		return nil, fmt.Errorf("%s too large", f.Name)
	}
	return b.Bytes(), nil
}

func needsStamp(tsa, ots string) bool {
	return tsa == "pending" || tsa == "fail" || ots == "pending" || ots == "fail"
}

func parseManifestContext(ctx context.Context, files archiveFiles, offset int64, index *archiveIndex) (manifestInfo, error) {
	var manifest manifestInfo
	f, err := files.FileAt(offset)
	if err != nil {
		return manifest, err
	}
	if f.UncompressedSize64 > maxManifestBytes {
		return manifest, fmt.Errorf("manifest too large")
	}
	rc, err := f.Open()
	if err != nil {
		return manifest, err
	}
	dec := json.NewDecoder(io.LimitReader(contextReader{ctx: ctx, r: rc}, maxManifestBytes+1))
	value, err := dec.Token()
	if err == nil {
		if delim, ok := value.(json.Delim); !ok || delim != '{' {
			err = fmt.Errorf("manifest must be an object")
		}
	}
	if err == nil {
		for dec.More() {
			value, tokenErr := dec.Token()
			if tokenErr != nil {
				err = tokenErr
				break
			}
			key := value.(string)
			switch key {
			case "version":
				err = dec.Decode(&manifest.Version)
			case "created_at", "format":
				var ignored string
				err = dec.Decode(&ignored)
			case "locations":
				err = dec.Decode(&manifest.Locations)
			case "verification_runs":
				err = dec.Decode(&manifest.VerificationRuns)
			case "verification_attempts":
				err = dec.Decode(&manifest.VerificationAttempts)
			case "proof_sha256":
				err = decodeManifestDigests(dec, ctx, index, "proof")
			case "entry_sha256":
				err = decodeManifestDigests(dec, ctx, index, "entry")
			default:
				err = skipManifestValue(dec, 0)
			}
			if err != nil {
				break
			}
		}
	}
	if err == nil {
		if _, tokenErr := dec.Token(); tokenErr != nil {
			err = tokenErr
		}
	}
	if err == nil {
		var extra json.RawMessage
		if decodeErr := dec.Decode(&extra); decodeErr != io.EOF {
			if decodeErr == nil {
				err = fmt.Errorf("manifest contains multiple JSON values")
			} else {
				err = decodeErr
			}
		}
	}
	closeErr := rc.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return manifest, fmt.Errorf("manifest: %w", err)
	}
	if manifest.Version < 1 || manifest.Version > export.Version {
		return manifest, fmt.Errorf("unsupported export version %d", manifest.Version)
	}
	return manifest, nil
}

func decodeManifestDigests(dec *json.Decoder, ctx context.Context, index *archiveIndex, kind string) error {
	value, err := dec.Token()
	if err != nil {
		return err
	}
	if value == nil {
		return nil
	}
	delim, ok := value.(json.Delim)
	if !ok || delim != '{' {
		return fmt.Errorf("manifest digest map is not an object")
	}
	for dec.More() {
		value, err := dec.Token()
		if err != nil {
			return err
		}
		name := value.(string)
		if len(name) > maxProofPath {
			return fmt.Errorf("manifest digest name too long")
		}
		var digest string
		if err := dec.Decode(&digest); err != nil {
			return err
		}
		if len(digest) > maxHashText {
			return fmt.Errorf("manifest digest too long")
		}
		if err := index.setManifestDigest(ctx, kind, name, digest); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

func skipManifestValue(dec *json.Decoder, depth int) error {
	if depth > 32 {
		return fmt.Errorf("manifest nesting too deep")
	}
	value, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := value.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return nil
	}
	if delim == '{' {
		for dec.More() {
			if _, err := dec.Token(); err != nil {
				return err
			}
			if err := skipManifestValue(dec, depth+1); err != nil {
				return err
			}
		}
	} else {
		for dec.More() {
			if err := skipManifestValue(dec, depth+1); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token()
	return err
}

func ImportBytes(data []byte, st *store.Store, proofsDir string, stamp Stamper) (Result, error) {
	return ImportContext(context.Background(), bytes.NewReader(data), int64(len(data)), st, proofsDir, stamp)
}

func ImportBytesContext(ctx context.Context, data []byte, st *store.Store, proofsDir string, stamp Stamper) (Result, error) {
	return ImportContext(ctx, bytes.NewReader(data), int64(len(data)), st, proofsDir, stamp)
}

func ParseProofID(name string) (int64, bool) {
	base := filepath.Base(name)
	ext := filepath.Ext(base)
	if ext != ".tsr" && ext != ".ots" {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimSuffix(base, ext), 10, 64)
	return id, err == nil && id > 0
}

func VerifyFile(path string) (Result, error) {
	return VerifyFileContext(context.Background(), path)
}

func VerifyFileContext(ctx context.Context, path string) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Result{}, err
	}
	return VerifyContext(ctx, f, info.Size())
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
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
