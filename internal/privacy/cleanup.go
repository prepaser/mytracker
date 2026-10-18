package privacy

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/prepaser/mytracker/internal/atomicfile"
)

const SafetyBackupName = ".privacy-safety.mtbackup"

const cleanupMarkerName = ".privacy-purge.pending"

func Pending(dataDir string) (bool, error) {
	for _, name := range []string{SafetyBackupName, cleanupMarkerName} {
		_, err := os.Lstat(filepath.Join(dataDir, name))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func RemoveBackupsExcept(dir, keep string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var removeErr error
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if path == keep || entry.IsDir() ||
			(!strings.HasSuffix(entry.Name(), ".mtbackup") && !strings.HasSuffix(entry.Name(), ".db")) {
			continue
		}
		if err := os.Remove(path); err != nil {
			removeErr = err
			break
		}
	}
	return errors.Join(removeErr, atomicfile.SyncDir(dir))
}

// The caller must hold the backup lock and verify the replacement and proof cleanup first.
func ClearSafetyBackup(dataDir string) error {
	return clearSafetyBackup(dataDir, atomicfile.SyncDir)
}

func clearSafetyBackup(dataDir string, sync func(string) error) error {
	pending, err := Pending(dataDir)
	if err != nil || !pending {
		return err
	}
	marker := filepath.Join(dataDir, cleanupMarkerName)
	if err := atomicfile.WriteFunc(marker, 0600, func(*os.File) error { return nil }); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dataDir, SafetyBackupName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := sync(dataDir); err != nil {
		return err
	}
	if err := os.Remove(marker); err != nil {
		return err
	}
	// The backup removal is durable; a resurrected marker only repeats cleanup.
	if err := sync(dataDir); err != nil {
		slog.Warn("privacy cleanup completed; pending marker removal was not synced")
	}
	return nil
}
