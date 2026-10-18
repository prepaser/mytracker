package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/prepaser/mytracker/internal/atomicfile"
)

type ProofGCStatus struct {
	Pending int   `json:"pending"`
	Failed  int   `json:"failed"`
	Oldest  int64 `json:"oldest,omitempty"`
}

type proofGCItem struct {
	ID       int64
	Path     string
	Attempts int
}

var proofGCSyncDir = atomicfile.SyncDir

func (s *Store) enqueueProofGCWhere(tx *sql.Tx, where string, args ...any) error {
	now := time.Now().Unix()
	for _, ext := range []string{".tsr", ".ots"} {
		query := `
			INSERT OR IGNORE INTO proof_gc(path,created_at,updated_at)
			SELECT printf('%d` + ext + `',id),?,? FROM locations WHERE ` + where
		queryArgs := append([]any{now, now}, args...)
		if _, err := tx.Exec(query, queryArgs...); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ProcessProofGC(proofsDir string, limit int) (int, error) {
	return s.ProcessProofGCContext(context.Background(), proofsDir, limit)
}

func (s *Store) ProcessProofGCContext(ctx context.Context, proofsDir string, limit int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	unlockProofs, err := s.LockProofMutationContext(ctx)
	if err != nil {
		return 0, err
	}
	defer unlockProofs()
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	now := time.Now().Unix()
	rows, err := s.db.QueryContext(ctx, `
		SELECT id,path,attempts FROM proof_gc
		WHERE next_attempt_at<=? ORDER BY id LIMIT ?`, now, limit)
	if err != nil {
		return 0, err
	}
	var items []proofGCItem
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			_ = rows.Close()
			return 0, err
		}
		var item proofGCItem
		if err := rows.Scan(&item.ID, &item.Path, &item.Attempts); err != nil {
			_ = rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(items) > 0 {
		if err := proofDirectoryReady(proofsDir); err != nil {
			for _, item := range items {
				if failErr := s.failProofGCContext(ctx, item, err); failErr != nil {
					return 0, failErr
				}
			}
			return 0, err
		}
	}
	done := 0
	deleted := make([]proofGCItem, 0, len(items))
	cleanupBases := make(map[string]struct{}, len(items))
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		if filepath.Base(item.Path) != item.Path ||
			(!strings.HasSuffix(item.Path, ".tsr") && !strings.HasSuffix(item.Path, ".ots")) {
			if err := s.failProofGCContext(ctx, item, fmt.Errorf("invalid proof path")); err != nil {
				return done, err
			}
			continue
		}
		path := filepath.Join(proofsDir, item.Path)
		info, err := os.Lstat(path)
		if err == nil && (info.IsDir() || !info.Mode().IsRegular()) {
			err = fmt.Errorf("proof path is not a regular file")
		}
		if err == nil {
			err = os.Remove(path)
		}
		if err == nil {
			deleted = append(deleted, item)
			cleanupBases[item.Path] = struct{}{}
			continue
		}
		if os.IsNotExist(err) {
			if dirErr := proofDirectoryReady(proofsDir); dirErr == nil {
				deleted = append(deleted, item)
				cleanupBases[item.Path] = struct{}{}
				continue
			} else {
				err = dirErr
			}
		}
		if err := s.failProofGCContext(ctx, item, err); err != nil {
			return done, err
		}
	}
	if len(deleted) == 0 {
		return done, nil
	}
	if err := ctx.Err(); err != nil {
		return done, err
	}
	if err := removeProofReplacementArtifactsContext(ctx, proofsDir, cleanupBases); err != nil {
		return done, err
	}
	if err := proofGCSyncDir(proofsDir); err != nil {
		return done, err
	}
	for _, item := range deleted {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM proof_gc WHERE id=?`, item.ID); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

func proofDirectoryReady(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("proof directory unavailable: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("proof directory is not a directory")
	}
	return nil
}

func (s *Store) failProofGCContext(ctx context.Context, item proofGCItem, cause error) error {
	attempts := item.Attempts + 1
	delay := time.Second << min(attempts-1, 8)
	msg := cause.Error()
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `
		UPDATE proof_gc SET attempts=?,next_attempt_at=?,last_error=?,updated_at=? WHERE id=?`,
		attempts, now.Add(delay).Unix(), msg, now.Unix(), item.ID)
	return err
}

func (s *Store) ProofGCStatus() (ProofGCStatus, error) {
	return s.ProofGCStatusContext(context.Background())
}

func (s *Store) ProofGCStatusContext(ctx context.Context) (ProofGCStatus, error) {
	var out ProofGCStatus
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN attempts>0 THEN 1 ELSE 0 END),0),
			COALESCE(MIN(created_at),0)
		FROM proof_gc`).Scan(&out.Pending, &out.Failed, &out.Oldest)
	return out, err
}

func (s *Store) ProofGCReadyCount() (int, error) {
	return s.ProofGCReadyCountContext(context.Background(), time.Now().Unix())
}

func (s *Store) ProofGCReadyCountContext(ctx context.Context, now int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM proof_gc WHERE next_attempt_at<=?`, now).Scan(&n)
	return n, err
}
