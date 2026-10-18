package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type ProofAudit struct {
	Missing []string `json:"missing"`
	Orphan  []string `json:"orphan"`
}

const auditProofReadDirBatchSize = 256

func (s *Store) AuditProofs(dir string) (ProofAudit, error) {
	var out ProofAudit
	rows, err := s.db.Query(`SELECT id,tsa_status,ots_status FROM locations`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id int64
		var tsa, ots string
		if err := rows.Scan(&id, &tsa, &ots); err != nil {
			_ = rows.Close()
			return ProofAudit{}, err
		}
		if tsa == "ok" {
			name := strconv.FormatInt(id, 10) + ".tsr"
			present, err := auditProofPathPresent(dir, name)
			if err != nil {
				_ = rows.Close()
				return ProofAudit{}, err
			}
			if !present {
				out.Missing = append(out.Missing, name)
			}
		}
		if ots == "submitted" || ots == "confirmed" {
			name := strconv.FormatInt(id, 10) + ".ots"
			present, err := auditProofPathPresent(dir, name)
			if err != nil {
				_ = rows.Close()
				return ProofAudit{}, err
			}
			if !present {
				out.Missing = append(out.Missing, name)
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return ProofAudit{}, err
	}
	if err := rows.Close(); err != nil {
		return ProofAudit{}, err
	}
	proofs, err := os.Open(dir)
	if err != nil {
		return ProofAudit{}, err
	}
	defer func() { _ = proofs.Close() }()

	for {
		entries, err := proofs.ReadDir(auditProofReadDirBatchSize)
		for _, entry := range entries {
			name := entry.Name()
			if _, _, ok := parseProofReplacement(name); ok {
				out.Orphan = append(out.Orphan, name)
				continue
			}
			if entry.IsDir() {
				continue
			}
			ext := filepath.Ext(name)
			if ext != ".tsr" && ext != ".ots" {
				continue
			}
			id, parseErr := strconv.ParseInt(strings.TrimSuffix(name, ext), 10, 64)
			if parseErr != nil || strconv.FormatInt(id, 10)+ext != name {
				out.Orphan = append(out.Orphan, name)
				continue
			}
			expected, lookupErr := s.auditProofExpected(id)
			if lookupErr != nil {
				return ProofAudit{}, lookupErr
			}
			if (ext == ".tsr" && !expected.tsa) || (ext == ".ots" && !expected.ots) {
				out.Orphan = append(out.Orphan, name)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return ProofAudit{}, err
		}
	}
	sort.Strings(out.Missing)
	sort.Strings(out.Orphan)
	return out, nil
}

type auditProofExpected struct {
	tsa bool
	ots bool
}

func (s *Store) auditProofExpected(id int64) (auditProofExpected, error) {
	var tsa, ots string
	err := s.db.QueryRow(`SELECT tsa_status,ots_status FROM locations WHERE id=?`, id).Scan(&tsa, &ots)
	if errors.Is(err, sql.ErrNoRows) {
		return auditProofExpected{}, nil
	}
	if err != nil {
		return auditProofExpected{}, err
	}
	return auditProofExpected{tsa: tsa == "ok", ots: ots == "submitted" || ots == "confirmed"}, nil
}

func auditProofPathPresent(dir, name string) (bool, error) {
	info, err := os.Lstat(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !info.IsDir(), nil
}

func (s *Store) ExpiredLeaseCount(now int64) (int, error) {
	return s.ExpiredLeaseCountContext(context.Background(), now)
}

func (s *Store) ExpiredLeaseCountContext(ctx context.Context, now int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM stamp_jobs
		WHERE state='running' AND lease_until<=?`, now).Scan(&n)
	return n, err
}

func (s *Store) SchemaVersion() (int, error) {
	var version int
	err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	return version, err
}
