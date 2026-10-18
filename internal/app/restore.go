package app

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/prepaser/mytracker/internal/atomicfile"
	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/metrics"
	"github.com/prepaser/mytracker/internal/store"
)

func runRestoreCommand(sourcePath string, cfg *config.Config) int {
	source, err := backup.StageRestoreSource(sourcePath, cfg.DataDir)
	if err != nil {
		slog.Error("stage restore source", "err", err)
		return 1
	}
	defer func() { _ = os.Remove(source) }()

	needsRawRecovery, err := currentDatabaseNeedsRawRecovery(cfg.DBPath())
	if err != nil {
		slog.Error("inspect current database", "err", err)
		return 1
	}
	rawRecovery := ""
	if needsRawRecovery {
		rawRecovery, err = preserveRawRestoreState(cfg)
		if err != nil {
			slog.Error("preserve current database", "err", err)
			return 1
		}
		slog.Warn("current database requires raw recovery; skipping database open", "snapshot", rawRecovery)
	}

	safety := ""
	if !needsRawRecovery {
		st, openErr := store.Open(cfg.DBPath())
		if openErr != nil {
			rawRecovery, err = preserveRawRestoreState(cfg)
			if err != nil {
				slog.Error("db open and preserve current database", "open_err", openErr, "err", err)
				return 1
			}
			slog.Warn("current database is not openable; preserved raw recovery snapshot", "err", openErr, "snapshot", rawRecovery)
		} else {
			safety, err = backup.CreateSafety(cfg, st)
			if err == nil {
				err = st.Checkpoint()
			}
			closeErr := st.Close()
			err = errors.Join(err, closeErr)
			if err != nil {
				slog.Error("restore safety backup", "err", err)
				return 1
			}
		}
	}
	if err := removeSQLiteSidecars(cfg.DBPath()); err != nil {
		slog.Error("restore remove sqlite sidecars", "err", err)
		return 1
	}
	if err := backup.RestoreStaged(source, cfg); err != nil {
		metrics.Restore("fail")
		slog.Error("restore", "err", err, "safety_backup", safety, "raw_recovery", rawRecovery)
		return 1
	}
	metrics.Restore("ok")
	output := ""
	if safety != "" {
		output = fmt.Sprintf("restored %s (safety backup: %s)", sourcePath, safety)
	} else {
		output = fmt.Sprintf("restored %s (raw recovery snapshot: %s)", sourcePath, rawRecovery)
	}
	if err := writeCommandLine(output); err != nil {
		slog.Error("write output", "err", err)
		return 1
	}
	return 0
}

func currentDatabaseNeedsRawRecovery(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		for _, suffix := range []string{"-wal", "-shm"} {
			if _, err := os.Lstat(path + suffix); err == nil {
				return true, nil
			} else if !os.IsNotExist(err) {
				return false, err
			}
		}
		return directoryHasEntries(filepath.Join(filepath.Dir(path), "proofs"))
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return true, nil
	}
	dsn, err := store.SQLiteReadOnlyDSN(path)
	if err != nil {
		return false, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return true, nil
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		return true, nil
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return true, nil
	}
	if version > store.CurrentSchemaVersion {
		return true, nil
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return true, nil
	}
	return integrity != "ok", nil
}

func directoryHasEntries(path string) (bool, error) {
	dir, err := os.Open(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = dir.Close() }()
	_, err = dir.Readdirnames(1)
	if err == io.EOF {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func preserveRawRestoreState(cfg *config.Config) (path string, err error) {
	path = filepath.Join(cfg.DataDir, fmt.Sprintf(".restore-recovery-%d", time.Now().UnixNano()))
	if err := os.Mkdir(path, 0700); err != nil {
		return "", err
	}
	recoveryPath := path
	defer func() {
		if err != nil {
			_ = os.RemoveAll(recoveryPath)
		}
	}()
	for _, name := range []string{"mytracker.db", "mytracker.db-wal", "mytracker.db-shm"} {
		src := filepath.Join(cfg.DataDir, name)
		if _, statErr := os.Lstat(src); os.IsNotExist(statErr) {
			continue
		} else if statErr != nil {
			return "", statErr
		}
		if err := copyRecoveryEntry(src, filepath.Join(path, name)); err != nil {
			return "", fmt.Errorf("copy %s: %w", name, err)
		}
	}
	proofs := filepath.Join(path, "proofs")
	if _, statErr := os.Lstat(cfg.ProofsDir()); os.IsNotExist(statErr) {
		if err := os.Mkdir(proofs, 0700); err != nil {
			return "", err
		}
	} else if statErr != nil {
		return "", statErr
	} else if err := copyRecoveryEntry(cfg.ProofsDir(), proofs); err != nil {
		return "", fmt.Errorf("copy proofs: %w", err)
	}
	if err := atomicfile.SyncDir(proofs); err != nil {
		return "", err
	}
	if err := atomicfile.SyncDir(path); err != nil {
		return "", err
	}
	if err := atomicfile.SyncDir(cfg.DataDir); err != nil {
		return "", err
	}
	return path, nil
}

func copyRecoveryEntry(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("symbolic link is not supported")
	}
	if info.IsDir() {
		if err := os.Mkdir(dst, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		dir, err := os.Open(src)
		if err != nil {
			return err
		}
		for {
			entries, readErr := dir.Readdirnames(256)
			for _, name := range entries {
				if err := copyRecoveryEntry(filepath.Join(src, name), filepath.Join(dst, name)); err != nil {
					_ = dir.Close()
					return err
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				_ = dir.Close()
				return readErr
			}
		}
		if err := dir.Close(); err != nil {
			return err
		}
		return atomicfile.SyncDir(dst)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported file type")
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	return errors.Join(copyErr, closeErr)
}
