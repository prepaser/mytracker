package backup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prepaser/mytracker/internal/atomicfile"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/metrics"
	"github.com/prepaser/mytracker/internal/privacy"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/zipguard"
	"golang.org/x/sys/unix"
)

const FormatVersion = 1

type FileInfo struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	FormatVersion int                 `json:"format_version"`
	SchemaVersion int                 `json:"schema_version"`
	CreatedAt     string              `json:"created_at"`
	Files         map[string]FileInfo `json:"files"`
}

type restoreJournal struct {
	Phase     string `json:"phase"`
	StageDir  string `json:"stage_dir"`
	OldDB     string `json:"old_db"`
	OldProofs string `json:"old_proofs"`
	HadDB     bool   `json:"had_db"`
	HadProofs bool   `json:"had_proofs"`
}

func Create(cfg *config.Config, st *store.Store) (path string, err error) {
	return CreateContext(context.Background(), cfg, st)
}

func CreateContext(ctx context.Context, cfg *config.Config, st *store.Store) (path string, err error) {
	return createContext(ctx, cfg, st, true)
}

func CreateSafety(cfg *config.Config, st *store.Store) (string, error) {
	return createContext(context.Background(), cfg, st, false)
}

func createContext(ctx context.Context, cfg *config.Config, st *store.Store, pruneBackups bool) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	unlock, err := st.LockBackupContext(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	return createLockedContext(ctx, cfg, st, pruneBackups)
}

// CreateLockedContext requires the caller to hold Store.LockBackupContext.
func CreateLockedContext(ctx context.Context, cfg *config.Config, st *store.Store) (path string, err error) {
	return createLockedContext(ctx, cfg, st, true)
}

// CreateSafetyLockedContext preserves existing backups and requires Store.LockBackupContext.
func CreateSafetyLockedContext(ctx context.Context, cfg *config.Config, st *store.Store) (string, error) {
	return createLockedContext(ctx, cfg, st, false)
}

func createLockedContext(ctx context.Context, cfg *config.Config, st *store.Store, pruneBackups bool) (path string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	unlockProofs, err := st.LockProofSnapshotContext(ctx)
	if err != nil {
		return "", err
	}
	defer unlockProofs()
	if err := os.MkdirAll(cfg.BackupsDir(), 0700); err != nil {
		return "", err
	}
	if err := Scavenge(cfg); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(cfg.BackupsDir(), ".bundle-work-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(work) }()
	workLock, err := os.OpenFile(filepath.Join(work, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	defer func() { _ = workLock.Close() }()
	if err := unix.Flock(int(workLock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return "", err
	}
	snapshot, err := st.BackupContext(ctx, work)
	if err != nil {
		return "", err
	}
	schema, err := schemaVersion(snapshot)
	if err != nil {
		return "", err
	}
	if err := validateSourceSnapshot(ctx, snapshot, cfg.ProofsDir()); err != nil {
		return "", err
	}
	index, err := newBackupIndex(ctx, work)
	if err != nil {
		return "", err
	}
	defer func() { _ = index.Close() }()
	if err := index.Begin(ctx); err != nil {
		return "", err
	}
	name := fmt.Sprintf("mytracker-%s.mtbackup", time.Now().UTC().Format("20060102-150405.000000000"))
	path = filepath.Join(cfg.BackupsDir(), name)
	out, err := os.CreateTemp(cfg.BackupsDir(), "."+name+".tmp-*")
	if err != nil {
		return "", err
	}
	tmp := out.Name()
	defer func() {
		_ = out.Close()
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	tmpLock, err := lockScavengeCandidate(tmp)
	if err != nil {
		return "", err
	}
	defer func() { _ = tmpLock.Close() }()
	if err = out.Chmod(0600); err != nil {
		return "", err
	}
	zw, err := zipguard.NewSpoolWriter(ctx, out, work)
	if err != nil {
		return "", err
	}
	defer func() { _ = zw.Abort() }()
	manifest := Manifest{
		FormatVersion: FormatVersion,
		SchemaVersion: schema,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err = addIndexedFile(ctx, zw, snapshot, "mytracker.db", index); err != nil {
		return "", err
	}
	if err = eachReferencedProof(ctx, snapshot, false, func(name string) error {
		return addIndexedFile(ctx, zw, filepath.Join(cfg.ProofsDir(), name), "proofs/"+name, index)
	}); err != nil {
		return "", err
	}
	if err = index.Commit(); err != nil {
		return "", err
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "manifest.json", Method: zip.Deflate})
	if err != nil {
		return "", err
	}
	if err = writeManifest(ctx, w, manifest, index); err != nil {
		return "", err
	}
	if err = zw.Close(); err != nil {
		return "", err
	}
	if err = out.Sync(); err != nil {
		return "", err
	}
	if err = out.Close(); err != nil {
		return "", err
	}
	if err = CheckFileContext(ctx, tmp); err != nil {
		return "", fmt.Errorf("verify backup: %w", err)
	}
	metrics.Backup("verified")
	path, err = installNoClobber(tmp, path)
	if err != nil {
		return "", err
	}
	if err = atomicfile.SyncDir(cfg.BackupsDir()); err != nil {
		return "", err
	}
	if !pruneBackups {
		return path, nil
	}
	gc, pruneErr := st.ProofGCStatusContext(ctx)
	if pruneErr == nil && gc.Pending > 0 {
		return path, nil
	}
	if pruneErr == nil {
		pruneErr = st.CheckpointContext(ctx)
	}
	if pruneErr == nil {
		pending, err := privacySafetyBackupPending(cfg.DataDir)
		if err != nil {
			pruneErr = fmt.Errorf("check privacy safety backup: %w", err)
		} else if !pending {
			pruneErr = prune(cfg.BackupsDir(), cfg.BackupKeep, path)
		}
	}
	if pruneErr != nil {
		metrics.Backup("prune_failed")
		slog.Warn("backup installed but pruning failed", "err", pruneErr)
	}
	return path, nil
}

func VerifyFile(path string) (Manifest, error) {
	return VerifyFileContext(context.Background(), path)
}

func VerifyFileContext(ctx context.Context, path string) (Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if err := ScavengeTemp(); err != nil {
		return Manifest{}, err
	}
	return inspectFileContext(ctx, path)
}

func InspectFile(path string) (Manifest, error) {
	return inspectFileContext(context.Background(), path)
}

func inspectFileContext(ctx context.Context, path string) (Manifest, error) {
	archive, err := openArchiveContext(ctx, path)
	if err != nil {
		return Manifest{}, err
	}
	defer archive.Close()
	m, err := verifyArchive(ctx, archive)
	if err != nil {
		return m, err
	}
	return collectManifest(ctx, m, archive.index)
}

func StageRestoreSource(path, dataDir string) (staged string, err error) {
	if err := ScavengeDataDir(dataDir); err != nil {
		return "", err
	}
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	if err := preflightRestoreSource(src, dataDir); err != nil {
		return "", err
	}
	out, err := os.CreateTemp(dataDir, ".restore-source-*.mtbackup")
	if err != nil {
		return "", err
	}
	staged = out.Name()
	defer func() {
		_ = out.Close()
		if err != nil {
			_ = os.Remove(staged)
		}
	}()
	if err = out.Chmod(0600); err != nil {
		return "", err
	}
	if _, err = copyBounded(out, src, maxBackupUncompressedSize); err != nil {
		return "", err
	}
	if err = out.Sync(); err != nil {
		return "", err
	}
	if err = out.Close(); err != nil {
		return "", err
	}
	if err = CheckFile(staged); err != nil {
		return "", err
	}
	return staged, nil
}

func preflightRestoreSource(src *os.File, dataDir string) error {
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxBackupUncompressedSize {
		return fmt.Errorf("restore source exceeds size limit")
	}
	zr, err := zipguard.Open(context.Background(), src, info.Size())
	if err != nil {
		return err
	}
	archive, err := indexArchive(context.Background(), zr)
	if err != nil {
		return err
	}
	defer archive.Close()
	uncompressed := archive.declared
	var fs unix.Statfs_t
	if err := unix.Statfs(dataDir, &fs); err != nil {
		return err
	}
	available := uint64(fs.Bavail) * uint64(fs.Bsize)
	required := uint64(info.Size()) + uncompressed + uint64(64<<20)
	if available < required {
		return fmt.Errorf("insufficient space to stage and extract backup")
	}
	_, err = src.Seek(0, io.SeekStart)
	return err
}

func Restore(path string, cfg *config.Config) (err error) {
	if err := Scavenge(cfg); err != nil {
		return err
	}
	if err := RecoverRestore(cfg); err != nil {
		return err
	}
	source, err := StageRestoreSource(path, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(source) }()
	return restoreStaged(source, cfg)
}

func RestoreStaged(path string, cfg *config.Config) (err error) {
	if cfg == nil {
		return fmt.Errorf("config is required")
	}
	if err := Scavenge(cfg); err != nil {
		return err
	}
	if err := RecoverRestore(cfg); err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Clean(cfg.DataDir), filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) ||
		filepath.Dir(rel) != "." || !strings.HasPrefix(filepath.Base(path), ".restore-source-") {
		return fmt.Errorf("restore source must be a private staged file")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("restore source is not private")
	}
	return restoreStaged(path, cfg)
}

func restoreStaged(source string, cfg *config.Config) (err error) {
	archive, err := openArchive(source)
	if err != nil {
		return err
	}
	defer archive.Close()
	if _, err := verifyArchive(context.Background(), archive); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(cfg.DataDir, ".restore-stage-*")
	if err != nil {
		return err
	}
	if err := writeJournal(cfg, restoreJournal{Phase: "staging", StageDir: stage}); err != nil {
		if !atomicfile.IsCommitted(err) {
			_ = os.RemoveAll(stage)
		}
		return err
	}
	staging := true
	defer func() {
		if staging {
			err = errors.Join(err, discardStagingRestore(cfg, restoreJournal{Phase: "staging", StageDir: stage}))
		}
	}()
	if err := extractArchive(context.Background(), archive, stage); err != nil {
		return err
	}
	if err := store.CheckDB(filepath.Join(stage, "mytracker.db")); err != nil {
		return err
	}
	if err := prepareStagedDatabase(filepath.Join(stage, "mytracker.db"), cfg); err != nil {
		return err
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	hadDB, err := pathExists(cfg.DBPath())
	if err != nil {
		return err
	}
	hadProofs, err := pathExists(cfg.ProofsDir())
	if err != nil {
		return err
	}
	j := restoreJournal{
		Phase:     "prepared",
		StageDir:  stage,
		OldDB:     filepath.Join(cfg.DataDir, ".restore-old-"+suffix+".db"),
		OldProofs: filepath.Join(cfg.DataDir, ".restore-old-proofs-"+suffix),
		HadDB:     hadDB,
		HadProofs: hadProofs,
	}
	if err := writeJournal(cfg, j); err != nil {
		if atomicfile.IsCommitted(err) {
			staging = false
		}
		return err
	}
	staging = false
	if j.HadDB {
		if err := os.Rename(cfg.DBPath(), j.OldDB); err != nil {
			return rollbackRestore(cfg, j, err)
		}
	}
	if j.HadProofs {
		if err := os.Rename(cfg.ProofsDir(), j.OldProofs); err != nil {
			return rollbackRestore(cfg, j, err)
		}
	}
	if err := atomicfile.SyncDir(cfg.DataDir); err != nil {
		return rollbackRestore(cfg, j, err)
	}
	if err := os.Rename(filepath.Join(stage, "mytracker.db"), cfg.DBPath()); err != nil {
		return rollbackRestore(cfg, j, err)
	}
	if err := os.Rename(filepath.Join(stage, "proofs"), cfg.ProofsDir()); err != nil {
		return rollbackRestore(cfg, j, err)
	}
	if err := atomicfile.SyncDir(cfg.DataDir); err != nil {
		return rollbackRestore(cfg, j, err)
	}
	j.Phase = "committed"
	if err := writeJournal(cfg, j); err != nil {
		return err
	}
	return finishRestore(cfg, j)
}

// prepareStagedDatabase deliberately does not use store.Open: restore must not
// migrate a backup before it is installed. Server startup performs that step.
func prepareStagedDatabase(path string, cfg *config.Config) (err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(abs),
		RawQuery: "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := db.Ping(); err != nil {
		return err
	}
	if exists, err := stagedTableExists(db, "app_settings"); err != nil {
		return err
	} else if exists {
		var settings []byte
		err := db.QueryRow(`SELECT json FROM app_settings WHERE id=1`).Scan(&settings)
		if errors.Is(err, sql.ErrNoRows) {
			settings = nil
		} else if err != nil {
			return err
		}
		if len(settings) > 0 {
			candidate := cfg.Clone()
			if err := candidate.ApplySettings(settings); err != nil {
				return fmt.Errorf("restore settings: %w", err)
			}
		}
	}
	if exists, err := stagedTableExists(db, "admin_sessions"); err != nil {
		return err
	} else if exists {
		if _, err := db.Exec(`DELETE FROM admin_sessions`); err != nil {
			return err
		}
	}
	var busy, logFrames, checkpointed int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("wal checkpoint busy (%d/%d frames)", checkpointed, logFrames)
	}
	return nil
}

func stagedTableExists(db *sql.DB, name string) (bool, error) {
	var exists int
	err := db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && exists == 1, err
}

func removeRestoredBootstrap(cfg *config.Config) error {
	return config.RemoveInitialPassword(cfg.DataDir)
}

func RecoverRestore(cfg *config.Config) error {
	if err := Scavenge(cfg); err != nil {
		return err
	}
	b, err := os.ReadFile(restoreJournalPath(cfg))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	j, err := parseRestoreJournal(b, cfg.DataDir)
	if err != nil {
		return err
	}
	if j.Phase == "staging" {
		return discardStagingRestore(cfg, j)
	}
	if j.Phase == "committed" {
		return finishRestore(cfg, j)
	}
	return rollbackRestore(cfg, j, nil)
}

func discardStagingRestore(cfg *config.Config, j restoreJournal) error {
	if err := removeTree(j.StageDir); err != nil {
		return fmt.Errorf("discard restore staging: %w", err)
	}
	if err := atomicfile.SyncDir(cfg.DataDir); err != nil {
		return err
	}
	if err := removeFile(restoreJournalPath(cfg)); err != nil {
		return fmt.Errorf("remove restore journal: %w", err)
	}
	return atomicfile.SyncDir(cfg.DataDir)
}

func rollbackRestore(cfg *config.Config, j restoreJournal, cause error) error {
	var rollbackErrs []error
	if exists, err := pathExists(j.OldDB); err != nil {
		rollbackErrs = append(rollbackErrs, err)
	} else if exists {
		if err := removeFile(cfg.DBPath()); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		} else if err := os.Rename(j.OldDB, cfg.DBPath()); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
	} else if !j.HadDB {
		rollbackErrs = appendError(rollbackErrs, removeFile(cfg.DBPath()))
	}
	if exists, err := pathExists(j.OldProofs); err != nil {
		rollbackErrs = append(rollbackErrs, err)
	} else if exists {
		if err := removeTree(cfg.ProofsDir()); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		} else if err := os.Rename(j.OldProofs, cfg.ProofsDir()); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
	} else if !j.HadProofs {
		if err := removeTree(cfg.ProofsDir()); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		} else if err := os.MkdirAll(cfg.ProofsDir(), 0700); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
	}
	rollbackErrs = appendError(rollbackErrs, removeTree(j.StageDir))
	rollbackErrs = appendError(rollbackErrs, atomicfile.SyncDir(cfg.DataDir))
	if len(rollbackErrs) > 0 {
		return errors.Join(cause, fmt.Errorf("restore rollback: %w", errors.Join(rollbackErrs...)))
	}
	if err := removeFile(restoreJournalPath(cfg)); err != nil {
		return errors.Join(cause, fmt.Errorf("remove restore journal: %w", err))
	}
	if err := atomicfile.SyncDir(cfg.DataDir); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func finishRestore(cfg *config.Config, j restoreJournal) error {
	var cleanupErrs []error
	cleanupErrs = appendError(cleanupErrs, removeFile(j.OldDB))
	cleanupErrs = appendError(cleanupErrs, removeTree(j.OldProofs))
	cleanupErrs = appendError(cleanupErrs, removeTree(j.StageDir))
	cleanupErrs = appendError(cleanupErrs, atomicfile.SyncDir(cfg.DataDir))
	if len(cleanupErrs) > 0 {
		return fmt.Errorf("finish restore: %w", errors.Join(cleanupErrs...))
	}
	if err := removeRestoredBootstrap(cfg); err != nil {
		return fmt.Errorf("finish restore bootstrap cleanup: %w", err)
	}
	if err := removeFile(restoreJournalPath(cfg)); err != nil {
		return err
	}
	return atomicfile.SyncDir(cfg.DataDir)
}

func pathExists(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func removeFile(path string) error {
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func removeTree(path string) error {
	if path == "" {
		return nil
	}
	return os.RemoveAll(path)
}

func appendError(errs []error, err error) []error {
	if err != nil {
		return append(errs, err)
	}
	return errs
}

func installNoClobber(tmp, base string) (string, error) {
	for attempt := range 10000 {
		path := base
		if attempt > 0 {
			ext := filepath.Ext(base)
			path = strings.TrimSuffix(base, ext) + fmt.Sprintf("-%d%s", attempt, ext)
		}
		if err := os.Link(tmp, path); err == nil {
			if err := os.Remove(tmp); err != nil {
				return "", err
			}
			return path, nil
		} else if !os.IsExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("unable to allocate backup filename")
}

func writeJournal(cfg *config.Config, j restoreJournal) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return atomicfile.WriteFunc(restoreJournalPath(cfg), 0600, func(f *os.File) error {
		_, err := f.Write(b)
		return err
	})
}

func restoreJournalPath(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, ".restore-journal.json")
}

func extract(path, stage string) error {
	archive, err := openArchive(path)
	if err != nil {
		return err
	}
	defer archive.Close()
	return extractArchive(context.Background(), archive, stage)
}

func extractArchive(ctx context.Context, a *openArchiveResult, stage string) error {
	if err := os.MkdirAll(filepath.Join(stage, "proofs"), 0700); err != nil {
		return err
	}
	actualTotal := uint64(0)
	if err := a.index.Each(ctx, func(item indexedFile) error {
		name := item.Name
		dst := filepath.Join(stage, filepath.FromSlash(name))
		f, err := a.files.FileAt(item.Offset)
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			_ = rc.Close()
			return err
		}
		var copied int64
		copied, err = copyBoundedContext(ctx, out, rc, entryLimit(name))
		_ = rc.Close()
		if err == nil {
			if uint64(copied) > a.declared || uint64(copied) > uint64(maxBackupUncompressedSize)-actualTotal {
				err = fmt.Errorf("backup exceeds uncompressed size limit")
			} else {
				actualTotal += uint64(copied)
			}
		}
		if err == nil {
			err = out.Sync()
		}
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	if err := atomicfile.SyncDir(filepath.Join(stage, "proofs")); err != nil {
		return err
	}
	return atomicfile.SyncDir(stage)
}

func schemaVersion(dbPath string) (int, error) {
	dsn, err := store.SQLiteReadOnlyDSN(dbPath)
	if err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	var version int
	err = db.QueryRow(`PRAGMA user_version`).Scan(&version)
	return version, err
}

func validateSourceSnapshot(ctx context.Context, snapshot, proofsDir string) error {
	total := int64(1024)
	if err := validateSource(ctx, snapshot, "mytracker.db", &total); err != nil {
		return err
	}
	return eachReferencedProof(ctx, snapshot, false, func(name string) error {
		return validateSource(ctx, filepath.Join(proofsDir, name), "proofs/"+name, &total)
	})
}

func validateSource(ctx context.Context, path, name string, total *int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > entryLimit(name) {
		return fmt.Errorf("backup entry %q exceeds size limit", name)
	}
	// Reserve enough JSON metadata for this entry as well as its payload.
	if info.Size() > maxBackupUncompressedSize-*total-192 {
		return fmt.Errorf("backup exceeds uncompressed size limit")
	}
	*total += info.Size() + 192
	return nil
}

func addIndexedFile(ctx context.Context, zw *zipguard.SpoolWriter, path, name string, index *backupIndex) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: info.ModTime()}
	header.SetMode(0600)
	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, err := copyBoundedContext(ctx, io.MultiWriter(w, hash), f, entryLimit(name))
	if err != nil {
		return err
	}
	if err := index.Add(ctx, name, 0, n); err != nil {
		return err
	}
	return index.SetInfo(ctx, name, FileInfo{SHA256: hex.EncodeToString(hash.Sum(nil)), Size: n})
}

func entryLimit(name string) int64 {
	switch {
	case name == "manifest.json":
		return maxBackupManifestSize
	case name == "mytracker.db":
		return maxBackupDatabaseSize
	case strings.HasPrefix(name, "proofs/"):
		return maxBackupProofSize
	default:
		return maxBackupUncompressedSize
	}
}

func copyBounded(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	return copyBoundedContext(context.Background(), dst, src, limit)
}

func copyBoundedContext(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (int64, error) {
	if limit < 0 {
		return 0, fmt.Errorf("invalid backup size limit")
	}
	n, err := copyContext(ctx, dst, io.LimitReader(src, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, fmt.Errorf("backup entry exceeds size limit")
	}
	return n, nil
}

var backupCopyBuffers = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	buffer := backupCopyBuffers.Get().(*[]byte)
	defer backupCopyBuffers.Put(buffer)
	return io.CopyBuffer(dst, contextReader{ctx: ctx, reader: src}, *buffer)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func prune(dir string, keep int, preserve ...string) error {
	if keep < 1 {
		keep = 1
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var backups []os.DirEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "mytracker-") {
			continue
		}
		if strings.HasSuffix(e.Name(), ".mtbackup") || strings.HasSuffix(e.Name(), ".db") {
			backups = append(backups, e)
		}
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].Name() < backups[j].Name() })
	protected := make(map[string]struct{}, len(preserve))
	for _, path := range preserve {
		protected[filepath.Clean(path)] = struct{}{}
	}
	var removeErr error
	remaining := len(backups)
	for _, entry := range backups {
		if remaining <= keep {
			break
		}
		path := filepath.Join(dir, entry.Name())
		if _, ok := protected[filepath.Clean(path)]; ok {
			continue
		}
		if err := os.Remove(path); err != nil {
			removeErr = err
			break
		}
		remaining--
	}
	return errors.Join(removeErr, atomicfile.SyncDir(dir))
}

func privacySafetyBackupPending(dataDir string) (bool, error) {
	return privacy.Pending(dataDir)
}
