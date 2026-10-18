package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/prepaser/mytracker/internal/atomicfile"
)

type PrivacyStats struct {
	LegacyRawPayloads int `json:"legacy_raw_payloads"`
	RiskyShares       int `json:"risky_shares"`
}

type privacyCache struct {
	generation       uint64
	cachedGeneration uint64
	initialized      bool
	legacy           int
}

func legacyPayload(payload []byte) bool {
	var raw map[string]json.RawMessage
	if json.Unmarshal(payload, &raw) != nil {
		return true
	}
	for key := range raw {
		switch key {
		case "_type", "tst", "lat", "lon", "acc", "alt", "vel", "cog", "batt":
		default:
			return true
		}
	}
	return false
}

func (s *Store) commitPrivacyInsert(tx *sql.Tx, payload []byte) error {
	legacy := legacyPayload(payload)
	// Scans must not cache a committed row before its incremental update.
	_ = s.privacyMu.Acquire(context.Background(), 1)
	defer s.privacyMu.Release(1)
	if err := tx.Commit(); err != nil {
		return err
	}
	generation := s.privacy.generation
	s.privacy.generation++
	if s.privacy.initialized && s.privacy.cachedGeneration == generation {
		if legacy {
			s.privacy.legacy++
		}
		s.privacy.cachedGeneration = s.privacy.generation
	}
	return nil
}

func (s *Store) PrivacyStats() (PrivacyStats, error) {
	return s.PrivacyStatsContext(context.Background())
}

func (s *Store) PrivacyStatsContext(ctx context.Context) (PrivacyStats, error) {
	var out PrivacyStats
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if err := s.privacyMu.Acquire(ctx, 1); err != nil {
		return out, err
	}
	if s.privacy.initialized && s.privacy.cachedGeneration == s.privacy.generation {
		out.LegacyRawPayloads = s.privacy.legacy
		s.privacyMu.Release(1)
		return out, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM share_links WHERE expires_at IS NULL OR precision_m=0`).Scan(&out.RiskyShares)
	}
	s.privacyMu.Release(1)
	select {
	case s.privacyScan <- struct{}{}:
		defer func() { <-s.privacyScan }()
	case <-ctx.Done():
		return out, ctx.Err()
	}
	if err := s.privacyMu.Acquire(ctx, 1); err != nil {
		return out, err
	}
	if s.privacy.initialized && s.privacy.cachedGeneration == s.privacy.generation {
		out.LegacyRawPayloads = s.privacy.legacy
		s.privacyMu.Release(1)
		return out, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM share_links WHERE expires_at IS NULL OR precision_m=0`).Scan(&out.RiskyShares)
	}
	generation := s.privacy.generation
	s.privacyMu.Release(1)
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM locations`)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return out, err
		}
		if legacyPayload(payload) {
			out.LegacyRawPayloads++
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if err := s.privacyMu.Acquire(ctx, 1); err != nil {
		return out, err
	}
	if s.privacy.generation == generation {
		s.privacy.legacy = out.LegacyRawPayloads
		s.privacy.cachedGeneration = generation
		s.privacy.initialized = true
	}
	s.privacyMu.Release(1)
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM share_links WHERE expires_at IS NULL OR precision_m=0`).
		Scan(&out.RiskyShares)
	return out, err
}

func (s *Store) Vacuum() error {
	if err := s.Checkpoint(); err != nil {
		return err
	}
	_, err := s.db.Exec(`VACUUM`)
	return err
}

func RemoveOrphanProofs(dir string, names []string) error {
	removed := false
	for _, name := range names {
		err := os.Remove(filepath.Join(dir, name))
		if err == nil {
			removed = true
			continue
		}
		if os.IsNotExist(err) {
			continue
		}
		if removed {
			return errors.Join(err, atomicfile.SyncDir(dir))
		}
		return err
	}
	if removed {
		return atomicfile.SyncDir(dir)
	}
	return nil
}
