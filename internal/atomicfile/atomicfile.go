package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type CommittedError struct{ Err error }

func (e *CommittedError) Error() string {
	return fmt.Sprintf("file replaced but directory sync failed: %v", e.Err)
}
func (e *CommittedError) Unwrap() error { return e.Err }

func IsCommitted(err error) bool {
	var committed *CommittedError
	return errors.As(err, &committed)
}

func SyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func Write(path string, data []byte, mode os.FileMode) (err error) {
	err = WriteFunc(path, mode, func(f *os.File) error { _, err := f.Write(data); return err })
	if IsCommitted(err) {
		return nil
	}
	return err
}

func WriteFunc(path string, mode os.FileMode, write func(*os.File) error) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if err = write(f); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if err := SyncDir(dir); err != nil {
		return &CommittedError{Err: err}
	}
	return nil
}
