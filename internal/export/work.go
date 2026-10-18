package export

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type exportWork struct {
	dir  string
	lock *os.File
}

func newExportWork() (*exportWork, error) {
	dir, err := os.MkdirTemp("", ".mytracker-export-work-*")
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	work := &exportWork{dir: dir, lock: lock}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = work.Close()
		return nil, err
	}
	return work, nil
}

func (w *exportWork) Close() error {
	if w == nil {
		return nil
	}
	var errs []error
	if w.lock != nil {
		if err := unix.Flock(int(w.lock.Fd()), unix.LOCK_UN); err != nil {
			errs = append(errs, err)
		}
		if err := w.lock.Close(); err != nil {
			errs = append(errs, err)
		}
		w.lock = nil
	}
	if w.dir != "" {
		if err := os.RemoveAll(w.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		w.dir = ""
	}
	return errors.Join(errs...)
}
