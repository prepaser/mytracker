package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

func (s *Store) lockProofs(ctx context.Context, weight int64, exclusive bool) (func(), error) {
	if err := s.proofSem.Acquire(ctx, weight); err != nil {
		return nil, err
	}
	unlock, err := lockFileContext(ctx, s.proofLockPath, exclusive)
	if err != nil {
		s.proofSem.Release(weight)
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			unlock()
			s.proofSem.Release(weight)
		})
	}, nil
}

func (s *Store) LockBackupContext(ctx context.Context) (func(), error) {
	return lockFileContext(ctx, filepath.Join(filepath.Dir(s.proofLockPath), ".mytracker-backups.lock"), true)
}

func (s *Store) LockBackupSnapshotContext(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(filepath.Dir(s.proofLockPath), ".mytracker-backups.lock"))
	if os.IsNotExist(err) {
		proof, proofErr := os.Open(s.proofLockPath)
		if proofErr != nil {
			return nil, proofErr
		}
		unlockProof, proofErr := lockOpenFileContext(ctx, proof, false)
		if proofErr != nil {
			return nil, proofErr
		}
		// Writable stores initialize the backup lock before recovering proofs.
		f, err = os.Open(filepath.Join(filepath.Dir(s.proofLockPath), ".mytracker-backups.lock"))
		if os.IsNotExist(err) {
			return unlockProof, nil
		}
		unlockProof()
	}
	if err != nil {
		return nil, err
	}
	return lockOpenFileContext(ctx, f, false)
}

func lockFileContext(ctx context.Context, path string, exclusive bool) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	return lockOpenFileContext(ctx, f, exclusive)
}

func lockOpenFileContext(ctx context.Context, f *os.File, exclusive bool) (func(), error) {
	mode := unix.LOCK_SH | unix.LOCK_NB
	if exclusive {
		mode = unix.LOCK_EX | unix.LOCK_NB
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err := unix.Flock(int(f.Fd()), mode)
		if err == nil {
			var once sync.Once
			return func() {
				once.Do(func() {
					_ = f.Close()
				})
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
