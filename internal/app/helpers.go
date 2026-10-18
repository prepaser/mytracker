package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/prepaser/mytracker/internal/atomicfile"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/privacy"
	"github.com/prepaser/mytracker/internal/store"
)

var commandOutput io.Writer = os.Stdout

const privacySafetyBackupName = privacy.SafetyBackupName

func privacySafetyBackupPath(dataDir string) string {
	return filepath.Join(dataDir, privacySafetyBackupName)
}

func privacySafetyBackupExists(dataDir string) bool {
	pending, err := privacy.Pending(dataDir)
	return pending || err != nil
}

func removePrivacySafetyBackup(dataDir string) error {
	return privacy.ClearSafetyBackup(dataDir)
}

func writeCommandJSON(value any) error {
	return json.NewEncoder(commandOutput).Encode(value)
}

func writeCommandLine(value string) error {
	_, err := fmt.Fprintln(commandOutput, value)
	return err
}

func removeSQLiteSidecars(path string) error {
	var errs []error
	removed := false
	for _, suffix := range []string{"-wal", "-shm"} {
		err := os.Remove(path + suffix)
		switch {
		case err == nil:
			removed = true
		case os.IsNotExist(err):
		default:
			errs = append(errs, err)
		}
	}
	if removed {
		errs = append(errs, atomicfile.SyncDir(filepath.Dir(path)))
	}
	return errors.Join(errs...)
}

func configFlagFirst(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		if (args[i] == "-config" || args[i] == "--config" || args[i] == "-format" || args[i] == "--format" ||
			args[i] == "-user" || args[i] == "--user" || args[i] == "-device" || args[i] == "--device" ||
			args[i] == "-from" || args[i] == "--from" || args[i] == "-to" || args[i] == "--to") && i+1 < len(args) {
			flags = append(flags, args[i], args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-config=") || strings.HasPrefix(args[i], "--config=") ||
			strings.HasPrefix(args[i], "-format=") || strings.HasPrefix(args[i], "--format=") ||
			strings.HasPrefix(args[i], "-user=") || strings.HasPrefix(args[i], "--user=") ||
			strings.HasPrefix(args[i], "-device=") || strings.HasPrefix(args[i], "--device=") ||
			strings.HasPrefix(args[i], "-from=") || strings.HasPrefix(args[i], "--from=") ||
			strings.HasPrefix(args[i], "-to=") || strings.HasPrefix(args[i], "--to=") ||
			args[i] == "-proofs" || args[i] == "--proofs" {
			flags = append(flags, args[i])
			continue
		}
		rest = append(rest, args[i])
	}
	return append(flags, rest...)
}

func loadAuditConfig(path string) (*config.Config, error) {
	if path == "" {
		path = config.DefaultConfigPath
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		cfg := config.Default()
		if err := cfg.Normalize(); err != nil {
			return nil, err
		}
		return loadAuditSettings(cfg)
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return loadAuditSettings(cfg)
}

func loadAuditSettings(cfg *config.Config) (*config.Config, error) {
	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return cfg, nil
	} else if err != nil {
		return nil, err
	}
	data, err := store.LoadSettingsReadOnly(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	if err := cfg.ApplySettings(data); err != nil {
		return nil, err
	}
	return cfg, nil
}

func loadSettings(cfg *config.Config, st *store.Store) (bool, error) {
	data, err := st.LoadSettings()
	if err != nil {
		return false, err
	}
	if err := cfg.ApplySettings(data); err != nil {
		return false, err
	}
	return len(data) > 0, nil
}
