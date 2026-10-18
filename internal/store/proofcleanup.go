package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/prepaser/mytracker/internal/atomicfile"
)

type proofReplacementArtifact struct {
	path  string
	base  string
	phase string
	mtime int64
}

const proofReplacementReadDirBatchSize = 256

func parseProofReplacement(name string) (base, phase string, ok bool) {
	if !strings.HasPrefix(name, ".") {
		return "", "", false
	}
	for _, candidate := range []string{".old-", ".new-"} {
		at := strings.Index(name, candidate)
		if at <= 1 || at+len(candidate) >= len(name) {
			continue
		}
		base = name[1:at]
		if ext := filepath.Ext(base); ext != ".tsr" && ext != ".ots" {
			continue
		}
		if _, err := strconv.ParseInt(strings.TrimSuffix(base, filepath.Ext(base)), 10, 64); err != nil {
			continue
		}
		return base, strings.TrimSuffix(strings.TrimPrefix(candidate, "."), "-"), true
	}
	return "", "", false
}

func (s *Store) recoverProofReplacements(dir string) error {
	file, err := os.Open(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	artifacts := make(map[string][]proofReplacementArtifact)
	for {
		entries, readErr := file.ReadDir(proofReplacementReadDirBatchSize)
		for _, entry := range entries {
			base, phase, ok := parseProofReplacement(entry.Name())
			if !ok || entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			artifacts[base] = append(artifacts[base], proofReplacementArtifact{
				path: filepath.Join(dir, entry.Name()), base: base, phase: phase, mtime: info.ModTime().UnixNano(),
			})
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	for base, candidates := range artifacts {
		if err := s.recoverProofReplacement(dir, base, candidates); err != nil {
			return err
		}
	}
	if len(artifacts) > 0 {
		return atomicfile.SyncDir(dir)
	}
	return nil
}

func (s *Store) recoverProofReplacement(dir, base string, candidates []proofReplacementArtifact) error {
	id, ext, err := proofBaseID(base)
	if err != nil {
		return nil
	}
	var locationID int64
	err = s.db.QueryRow(`SELECT id FROM locations WHERE id=?`, id).Scan(&locationID)
	locationExists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var jobState string
	if locationExists {
		kind := "ots"
		if ext == ".tsr" {
			kind = "tsa"
		}
		err = s.db.QueryRow(`SELECT state FROM stamp_jobs WHERE location_id=? AND kind=?`, id, kind).Scan(&jobState)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}

	old := make([]proofReplacementArtifact, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.phase != "old" {
			continue
		}
		info, err := os.Lstat(candidate.path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("proof replacement is not a regular file: %s", candidate.path)
		}
		if info.Size() == 0 {
			continue
		}
		old = append(old, candidate)
	}
	sort.SliceStable(old, func(i, j int) bool {
		if old[i].mtime == old[j].mtime {
			return old[i].path < old[j].path
		}
		return old[i].mtime < old[j].mtime
	})
	live := filepath.Join(dir, base)
	liveInfo, liveErr := os.Lstat(live)
	if liveErr != nil && !os.IsNotExist(liveErr) {
		return liveErr
	}
	liveExists := liveErr == nil
	if liveExists && !liveInfo.Mode().IsRegular() {
		return fmt.Errorf("live proof path is not a regular file: %s", live)
	}
	if !locationExists {
		return removeReplacementArtifacts(candidates)
	}
	restore := !liveExists || jobState != "done"
	if restore && len(old) > 0 {
		if err := os.Rename(old[0].path, live); err != nil {
			return err
		}
	}
	return removeReplacementArtifacts(candidates)
}

func proofBaseID(base string) (int64, string, error) {
	ext := filepath.Ext(base)
	if ext != ".tsr" && ext != ".ots" {
		return 0, "", errors.New("invalid proof extension")
	}
	id, err := strconv.ParseInt(strings.TrimSuffix(base, ext), 10, 64)
	if err != nil || id < 1 {
		return 0, "", errors.New("invalid proof id")
	}
	return id, ext, nil
}

func removeReplacementArtifacts(candidates []proofReplacementArtifact) error {
	for _, candidate := range candidates {
		if err := os.Remove(candidate.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func removeProofReplacementArtifactsContext(ctx context.Context, dir string, bases map[string]struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := file.ReadDir(proofReplacementReadDirBatchSize)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			base, _, ok := parseProofReplacement(entry.Name())
			if !ok || entry.IsDir() {
				continue
			}
			if _, ok := bases[base]; !ok {
				continue
			}
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
