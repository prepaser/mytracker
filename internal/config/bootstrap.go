package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/prepaser/mytracker/internal/atomicfile"
)

func InitialPasswordPath() string { return ".initial-password" }

const pendingInitialPasswordName = ".initial-password.pending"

const maxPendingInitialPasswordBytes = 16 << 10

type pendingInitialPassword struct {
	DataDir string `json:"data_dir"`
	SHA256  string `json:"sha256"`
}

func canonicalDataDir(dataDir string) (string, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func bootstrapOutsideData(dataDir string) error {
	data, err := canonicalDataDir(dataDir)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(data, filepath.Join(cwd, InitialPasswordPath()))
	if err != nil {
		return err
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("start mytracker outside data_dir so .initial-password is not stored with application data")
	}
	return nil
}

func writeInitialPassword(data []byte) error {
	f, err := os.CreateTemp(".", ".initial-password-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Publish the complete 0600 file without replacing an existing secret.
	if err := os.Link(f.Name(), InitialPasswordPath()); err != nil {
		return fmt.Errorf("create .initial-password without overwriting an existing file: %w", err)
	}
	if err := os.Remove(f.Name()); err != nil {
		return err
	}
	return syncBootstrapDir(".")
}

func prepareInitialPassword(dataDir string) ([]byte, error) {
	canonical, err := canonicalDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	pendingPath := filepath.Join(dataDir, pendingInitialPasswordName)
	pending, pendingExists, err := readPendingInitialPassword(pendingPath)
	if err != nil {
		return nil, err
	}
	initial, initialErr := readBootstrap(InitialPasswordPath())
	if pendingExists {
		if pending.DataDir != canonical {
			return nil, fmt.Errorf("pending initial password belongs to a different data directory")
		}
		if initialErr == nil {
			sum := sha256.Sum256(initial)
			if pending.SHA256 != hex.EncodeToString(sum[:]) {
				return nil, fmt.Errorf("pending initial password does not match .initial-password")
			}
			if len(initial) != 17 || initial[16] != '\n' {
				return nil, fmt.Errorf("invalid pending initial password")
			}
			return initial, nil
		}
		if !os.IsNotExist(initialErr) {
			return nil, initialErr
		}
		if err := removeBootstrap(pendingPath); err != nil {
			return nil, err
		}
	} else if initialErr == nil {
		return nil, fmt.Errorf(".initial-password already exists; refusing to overwrite an existing file")
	} else if !os.IsNotExist(initialErr) {
		return nil, initialErr
	}

	pass, err := RandToken(16)
	if err != nil {
		return nil, err
	}
	initial = []byte(pass + "\n")
	sum := sha256.Sum256(initial)
	pendingData, err := json.Marshal(pendingInitialPassword{
		DataDir: canonical,
		SHA256:  hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return nil, err
	}
	if err := atomicfile.Write(pendingPath, pendingData, 0600); err != nil {
		return nil, err
	}
	if err := writeInitialPassword(initial); err != nil {
		return nil, err
	}
	return initial, nil
}

func readPendingInitialPassword(path string) (pendingInitialPassword, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return pendingInitialPassword{}, false, nil
	}
	if err != nil {
		return pendingInitialPassword{}, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxPendingInitialPasswordBytes {
		return pendingInitialPassword{}, true, fmt.Errorf("invalid pending initial password marker")
	}
	f, err := os.Open(path)
	if err != nil {
		return pendingInitialPassword{}, true, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxPendingInitialPasswordBytes+1))
	if err != nil {
		return pendingInitialPassword{}, true, err
	}
	if len(data) > maxPendingInitialPasswordBytes {
		return pendingInitialPassword{}, true, fmt.Errorf("pending initial password marker is too large")
	}
	var pending pendingInitialPassword
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pending); err != nil || pending.DataDir == "" || len(pending.SHA256) != sha256.Size*2 {
		return pendingInitialPassword{}, true, fmt.Errorf("invalid pending initial password marker")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return pendingInitialPassword{}, true, fmt.Errorf("invalid pending initial password marker")
	}
	if _, err := hex.DecodeString(pending.SHA256); err != nil {
		return pendingInitialPassword{}, true, fmt.Errorf("invalid pending initial password marker")
	}
	return pending, true, nil
}

func removePendingInitialPassword(dataDir string) error {
	return removeBootstrap(filepath.Join(dataDir, pendingInitialPasswordName))
}

func relocateInitialPassword(dataDir string) error {
	for _, old := range []string{"initial-password", filepath.Join(dataDir, "initial-password")} {
		if err := relocateBootstrap(old); err != nil {
			return err
		}
	}
	return nil
}

func relocateBootstrap(old string) error {
	data, err := readBootstrap(old)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := writeInitialPassword(data); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		existing, readErr := readBootstrap(InitialPasswordPath())
		if readErr != nil || !bytes.Equal(existing, data) {
			return fmt.Errorf(".initial-password already exists; resolve the conflicting bootstrap files before restarting")
		}
		if err := os.Chmod(InitialPasswordPath(), 0600); err != nil {
			return err
		}
		if err := syncBootstrapDir("."); err != nil {
			return err
		}
	}
	if err := removeBootstrap(old); err != nil {
		return err
	}
	slog.Warn("initial password file relocated; remove it after reading", "path", InitialPasswordPath())
	return nil
}

func readBootstrap(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024 {
		return nil, fmt.Errorf("invalid initial password file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 1025))
	if len(data) > 1024 {
		return nil, fmt.Errorf("initial password file is too large")
	}
	return data, err
}

func RemoveInitialPassword(dataDir string) error {
	for _, path := range []string{InitialPasswordPath(), "initial-password", filepath.Join(dataDir, "initial-password")} {
		if err := removeBootstrap(path); err != nil {
			return err
		}
	}
	return removePendingInitialPassword(dataDir)
}

func removeBootstrap(path string) error {
	if err := os.Remove(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return syncBootstrapDir(filepath.Dir(path))
}

func syncBootstrapDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
