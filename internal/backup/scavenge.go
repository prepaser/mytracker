package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"golang.org/x/sys/unix"
)

const staleArtifactAge = 24 * time.Hour

func Scavenge(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("config is required")
	}
	preserve, err := restoreJournalPaths(cfg)
	if err != nil {
		return err
	}
	if err := scavengeDir(cfg.DataDir, preserve, func(name string) bool {
		return strings.HasPrefix(name, ".restore-source-") ||
			strings.HasPrefix(name, ".restore-stage-") ||
			strings.HasPrefix(name, ".restore-old-") ||
			strings.HasPrefix(name, ".export-")
	}); err != nil {
		return err
	}
	if err := scavengeDir(cfg.BackupsDir(), preserve, func(name string) bool {
		return strings.HasPrefix(name, ".bundle-work-") ||
			(strings.HasPrefix(name, ".mytracker-") && strings.Contains(name, ".tmp-"))
	}); err != nil {
		return err
	}
	return ScavengeTemp()
}

func ScavengeDataDir(dataDir string) error {
	preserve, err := restoreJournalPathsForDir(dataDir)
	if err != nil {
		return err
	}
	return scavengeDir(dataDir, preserve, func(name string) bool {
		return strings.HasPrefix(name, ".restore-source-") ||
			strings.HasPrefix(name, ".restore-stage-") ||
			strings.HasPrefix(name, ".restore-old-") ||
			strings.HasPrefix(name, ".export-")
	})
}

func ScavengeTemp() error {
	return scavengeDir(os.TempDir(), nil, func(name string) bool {
		return strings.HasPrefix(name, "mytracker-verify-") ||
			strings.HasPrefix(name, ".mytracker-audit-") ||
			strings.HasPrefix(name, ".mytracker-export-work-") ||
			strings.HasPrefix(name, "mytracker-import-index-") ||
			strings.HasPrefix(name, "mytracker-backup-index-")
	})
}

func scavengeDir(dir string, preserve map[string]struct{}, match func(string) bool) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-staleArtifactAge)
	for _, entry := range entries {
		if !match(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if _, ok := preserve[filepath.Clean(path)]; ok {
			continue
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		var lock *os.File
		if strings.HasPrefix(entry.Name(), ".mytracker-export-work-") || strings.HasPrefix(entry.Name(), ".bundle-work-") {
			if !info.IsDir() || !ownedByCurrentUser(info) {
				continue
			}
			lockPath := filepath.Join(path, ".lock")
			lockInfo, statErr := os.Lstat(lockPath)
			if statErr == nil {
				if !lockInfo.Mode().IsRegular() || !ownedByCurrentUser(lockInfo) {
					continue
				}
				lock, err = lockScavengeCandidate(lockPath)
				if err != nil {
					continue
				}
			} else if !os.IsNotExist(statErr) {
				continue
			}
		} else if strings.HasPrefix(entry.Name(), "mytracker-import-index-") ||
			strings.HasPrefix(entry.Name(), "mytracker-backup-index-") ||
			strings.HasPrefix(entry.Name(), "mytracker-verify-") ||
			(strings.HasPrefix(entry.Name(), ".mytracker-") && strings.Contains(entry.Name(), ".tmp-")) {
			if !info.Mode().IsRegular() || !ownedByCurrentUser(info) {
				continue
			}
			lock, err = lockScavengeCandidate(path)
			if err != nil {
				continue
			}
		} else if strings.HasPrefix(entry.Name(), ".mytracker-audit-") && !ownedByCurrentUser(info) {
			continue
		}
		removeErr := os.RemoveAll(path)
		if lock != nil {
			_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
			_ = lock.Close()
		}
		if removeErr != nil {
			return removeErr
		}
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func lockScavengeCandidate(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func restoreJournalPaths(cfg *config.Config) (map[string]struct{}, error) {
	return restoreJournalPathsForDir(cfg.DataDir)
}

func restoreJournalPathsForDir(dataDir string) (map[string]struct{}, error) {
	preserve := make(map[string]struct{})
	b, err := os.ReadFile(filepath.Join(dataDir, ".restore-journal.json"))
	if os.IsNotExist(err) {
		return preserve, nil
	}
	if err != nil {
		return nil, err
	}
	j, err := parseRestoreJournal(b, dataDir)
	if err != nil {
		return nil, err
	}
	for _, path := range []string{j.StageDir, j.OldDB, j.OldProofs} {
		if path != "" {
			preserve[filepath.Clean(path)] = struct{}{}
		}
	}
	return preserve, nil
}
