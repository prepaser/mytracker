package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const MaxVerificationAttempts = 5

var (
	ErrTooManyVerificationAttempts   = errors.New("verification attempts exceed limit")
	ErrDuplicateVerificationProvider = errors.New("duplicate verification provider")
)

const verificationColumns = `id,location_id,kind,trigger,result,proof_sha256,checked_at,
	tsa_gen_time,tsa_policy_oid,tsa_serial,tsa_signer_subject,tsa_signer_fingerprint,
	bitcoin_height,bitcoin_block_hash,agreeing_sources,source_count,error_code,
	engine_version,policy_sha256,tsa_nonce_status,tsa_policy_status,tsa_trust_mode,
	tsa_accuracy_ms,tsa_trust_anchor_fingerprint,bitcoin_pow_valid,bitcoin_confirmations`
const verificationJoinColumns = `v.id,v.location_id,v.kind,v.trigger,v.result,v.proof_sha256,v.checked_at,
	v.tsa_gen_time,v.tsa_policy_oid,v.tsa_serial,v.tsa_signer_subject,v.tsa_signer_fingerprint,
	v.bitcoin_height,v.bitcoin_block_hash,v.agreeing_sources,v.source_count,v.error_code,
	v.engine_version,v.policy_sha256,v.tsa_nonce_status,v.tsa_policy_status,v.tsa_trust_mode,
	v.tsa_accuracy_ms,v.tsa_trust_anchor_fingerprint,v.bitcoin_pow_valid,v.bitcoin_confirmations`

type VerificationRun struct {
	ID                        int64                 `json:"id"`
	LocationID                int64                 `json:"location_id"`
	Kind                      string                `json:"kind"`
	Trigger                   string                `json:"trigger"`
	Result                    string                `json:"result"`
	ProofSHA256               []byte                `json:"-"`
	CheckedAt                 int64                 `json:"checked_at"`
	TSAGenTime                *int64                `json:"tsa_gen_time,omitempty"`
	TSAPolicyOID              string                `json:"tsa_policy_oid,omitempty"`
	TSASerial                 string                `json:"tsa_serial,omitempty"`
	TSASignerSubject          string                `json:"tsa_signer_subject,omitempty"`
	TSASignerFingerprint      string                `json:"tsa_signer_fingerprint,omitempty"`
	BitcoinHeight             *int64                `json:"bitcoin_height,omitempty"`
	BitcoinBlockHash          string                `json:"bitcoin_block_hash,omitempty"`
	AgreeingSources           int                   `json:"agreeing_sources"`
	SourceCount               int                   `json:"source_count"`
	ErrorCode                 string                `json:"error_code,omitempty"`
	EngineVersion             int                   `json:"engine_version"`
	PolicySHA256              []byte                `json:"-"`
	TSANonceStatus            string                `json:"tsa_nonce_status,omitempty"`
	TSAPolicyStatus           string                `json:"tsa_policy_status,omitempty"`
	TSATrustMode              string                `json:"tsa_trust_mode,omitempty"`
	TSAAccuracyMS             int64                 `json:"tsa_accuracy_ms,omitempty"`
	TSATrustAnchorFingerprint string                `json:"tsa_trust_anchor_fingerprint,omitempty"`
	BitcoinPOWValid           bool                  `json:"bitcoin_pow_valid,omitempty"`
	BitcoinConfirmations      int64                 `json:"bitcoin_confirmations,omitempty"`
	Attempts                  []VerificationAttempt `json:"attempts,omitempty"`
}

type VerificationAttempt struct {
	ID            int64  `json:"id"`
	RunID         int64  `json:"run_id"`
	ProviderID    string `json:"provider_id"`
	Result        string `json:"result"`
	BlockHeight   *int64 `json:"block_height,omitempty"`
	BlockHash     string `json:"block_hash,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
	ErrorCode     string `json:"error_code,omitempty"`
	TipHeight     int64  `json:"tip_height,omitempty"`
	Confirmations int64  `json:"confirmations,omitempty"`
	HeaderValid   bool   `json:"header_valid,omitempty"`
	POWValid      bool   `json:"pow_valid,omitempty"`
	MerkleValid   bool   `json:"merkle_valid,omitempty"`
}

type VerificationStats struct {
	Results     map[string]int `json:"results"`
	LastSuccess int64          `json:"last_success,omitempty"`
}

type VerificationProviderStatus struct {
	ProviderID    string `json:"provider_id"`
	Result        string `json:"result"`
	DurationMS    int64  `json:"duration_ms"`
	CheckedAt     int64  `json:"checked_at"`
	Confirmations int64  `json:"confirmations"`
	HeaderValid   bool   `json:"header_valid"`
	POWValid      bool   `json:"pow_valid"`
	MerkleValid   bool   `json:"merkle_valid"`
	HeaderStatus  string `json:"header_status"`
	POWStatus     string `json:"pow_status"`
	MerkleStatus  string `json:"merkle_status"`
	ErrorCode     string `json:"error_code,omitempty"`
}

func (s *Store) SaveVerification(run *VerificationRun) error {
	if err := ValidateVerification(run); err != nil {
		return err
	}
	if err := validateVerificationAttempts(run.Attempts); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertVerification(tx, run); err != nil {
		return err
	}
	return tx.Commit()
}

func insertVerification(tx *sql.Tx, run *VerificationRun) error {
	if err := ValidateVerification(run); err != nil {
		return err
	}
	if err := validateVerificationAttempts(run.Attempts); err != nil {
		return err
	}
	if run.ProofSHA256 == nil {
		run.ProofSHA256 = []byte{}
	}
	if run.PolicySHA256 == nil {
		run.PolicySHA256 = []byte{}
	}
	res, err := tx.Exec(`
		INSERT INTO proof_verification_runs (
			location_id,kind,trigger,result,proof_sha256,checked_at,tsa_gen_time,
			tsa_policy_oid,tsa_serial,tsa_signer_subject,tsa_signer_fingerprint,
			bitcoin_height,bitcoin_block_hash,agreeing_sources,source_count,error_code,
			engine_version,policy_sha256,tsa_nonce_status,tsa_policy_status,tsa_trust_mode,
			tsa_accuracy_ms,tsa_trust_anchor_fingerprint,bitcoin_pow_valid,bitcoin_confirmations
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.LocationID, run.Kind, run.Trigger, run.Result, run.ProofSHA256, run.CheckedAt,
		run.TSAGenTime, run.TSAPolicyOID, run.TSASerial, run.TSASignerSubject,
		run.TSASignerFingerprint, run.BitcoinHeight, run.BitcoinBlockHash,
		run.AgreeingSources, run.SourceCount, run.ErrorCode, run.EngineVersion,
		run.PolicySHA256, run.TSANonceStatus, run.TSAPolicyStatus, run.TSATrustMode,
		run.TSAAccuracyMS, run.TSATrustAnchorFingerprint, run.BitcoinPOWValid,
		run.BitcoinConfirmations)
	if err != nil {
		return err
	}
	run.ID, err = res.LastInsertId()
	if err != nil {
		return err
	}
	for i := range run.Attempts {
		a := &run.Attempts[i]
		res, err := tx.Exec(`
			INSERT INTO proof_verification_attempts
				(run_id,provider_id,result,block_height,block_hash,duration_ms,error_code,
				 tip_height,confirmations,header_valid,pow_valid,merkle_valid)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			run.ID, a.ProviderID, a.Result, a.BlockHeight, a.BlockHash, a.DurationMS,
			a.ErrorCode, a.TipHeight, a.Confirmations, a.HeaderValid, a.POWValid, a.MerkleValid)
		if err != nil {
			return err
		}
		a.ID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		a.RunID = run.ID
	}
	return nil
}

func (tx *ImportTx) InsertVerification(run *VerificationRun) (int64, error) {
	if err := ValidateVerification(run); err != nil {
		return 0, err
	}
	if err := validateVerificationAttempts(run.Attempts); err != nil {
		return 0, err
	}
	if run.ProofSHA256 == nil {
		run.ProofSHA256 = []byte{}
	}
	if run.PolicySHA256 == nil {
		run.PolicySHA256 = []byte{}
	}
	res, err := tx.tx.Exec(`
		INSERT INTO proof_verification_runs (
			location_id,kind,trigger,result,proof_sha256,checked_at,tsa_gen_time,
			tsa_policy_oid,tsa_serial,tsa_signer_subject,tsa_signer_fingerprint,
			bitcoin_height,bitcoin_block_hash,agreeing_sources,source_count,error_code,
			engine_version,policy_sha256,tsa_nonce_status,tsa_policy_status,tsa_trust_mode,
			tsa_accuracy_ms,tsa_trust_anchor_fingerprint,bitcoin_pow_valid,bitcoin_confirmations
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.LocationID, run.Kind, "import", run.Result, run.ProofSHA256, run.CheckedAt,
		run.TSAGenTime, run.TSAPolicyOID, run.TSASerial, run.TSASignerSubject,
		run.TSASignerFingerprint, run.BitcoinHeight, run.BitcoinBlockHash,
		run.AgreeingSources, run.SourceCount, run.ErrorCode, 0,
		run.PolicySHA256, run.TSANonceStatus, run.TSAPolicyStatus, run.TSATrustMode,
		run.TSAAccuracyMS, run.TSATrustAnchorFingerprint, run.BitcoinPOWValid,
		run.BitcoinConfirmations)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (tx *ImportTx) InsertVerificationAttempt(runID int64, attempt *VerificationAttempt) error {
	if err := validateVerificationAttempt(attempt); err != nil {
		return err
	}
	var count int
	if err := tx.tx.QueryRow(`SELECT COUNT(*) FROM proof_verification_attempts WHERE run_id=?`, runID).Scan(&count); err != nil {
		return err
	}
	if count >= MaxVerificationAttempts {
		return ErrTooManyVerificationAttempts
	}
	var exists int
	if err := tx.tx.QueryRow(`SELECT COUNT(*) FROM proof_verification_attempts WHERE run_id=? AND provider_id=?`, runID, attempt.ProviderID).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return ErrDuplicateVerificationProvider
	}
	_, err := tx.tx.Exec(`
		INSERT INTO proof_verification_attempts
			(run_id,provider_id,result,block_height,block_hash,duration_ms,error_code,
			 tip_height,confirmations,header_valid,pow_valid,merkle_valid)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		runID, attempt.ProviderID, attempt.Result, attempt.BlockHeight,
		attempt.BlockHash, attempt.DurationMS, attempt.ErrorCode, attempt.TipHeight,
		attempt.Confirmations, attempt.HeaderValid, attempt.POWValid, attempt.MerkleValid)
	return err
}

func (s *Store) LatestVerification(locationID int64, kind string) (*VerificationRun, error) {
	row := s.db.QueryRow(`
		SELECT `+verificationColumns+` FROM proof_verification_runs
		WHERE location_id=? AND kind=? ORDER BY id DESC LIMIT 1`, locationID, kind)
	run, err := scanVerification(row)
	if err != nil {
		return nil, err
	}
	run.Attempts, err = s.verificationAttempts(run.ID)
	return run, err
}

func (s *Store) VerificationHistory(locationID, before int64, limit int) ([]VerificationRun, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 201 {
		limit = 201
	}
	if before < 1 {
		before = int64(^uint64(0) >> 1)
	}
	rows, err := s.db.Query(`
		SELECT `+verificationColumns+` FROM proof_verification_runs
		WHERE location_id=? AND id<? ORDER BY id DESC LIMIT ?`, locationID, before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []VerificationRun
	for rows.Next() {
		run, err := scanVerification(rows)
		if err != nil {
			return nil, err
		}
		run.Attempts, err = s.verificationAttempts(run.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, *run)
	}
	return out, rows.Err()
}

func (s *Store) VerificationProviderStatuses() ([]VerificationProviderStatus, error) {
	return s.VerificationProviderStatusesContext(context.Background())
}

func (s *Store) VerificationProviderStatusesContext(ctx context.Context) ([]VerificationProviderStatus, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.provider_id,a.result,a.duration_ms,r.checked_at,
			a.confirmations,a.header_valid,a.pow_valid,a.merkle_valid,a.error_code
		FROM proof_verification_attempts a
		JOIN proof_verification_runs r ON r.id=a.run_id
		JOIN (
			SELECT a2.provider_id,MAX(a2.id) AS id
			FROM proof_verification_attempts a2
			JOIN proof_verification_runs r2 ON r2.id=a2.run_id
			WHERE (r2.trigger!='import' OR r2.engine_version>0 AND EXISTS (
				SELECT 1 FROM proof_verification_jobs j2
				WHERE j2.location_id=r2.location_id AND j2.kind=r2.kind AND j2.state='done'
			  ))
			GROUP BY a2.provider_id
		) latest ON latest.id=a.id
		ORDER BY a.provider_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]VerificationProviderStatus, 0)
	for rows.Next() {
		var status VerificationProviderStatus
		if err := rows.Scan(&status.ProviderID, &status.Result, &status.DurationMS,
			&status.CheckedAt, &status.Confirmations, &status.HeaderValid,
			&status.POWValid, &status.MerkleValid, &status.ErrorCode); err != nil {
			return nil, err
		}
		status.HeaderStatus = VerificationEvidenceStatus(status.HeaderValid, status.ErrorCode, "header")
		status.POWStatus = VerificationEvidenceStatus(status.POWValid, status.ErrorCode, "pow")
		status.MerkleStatus = VerificationEvidenceStatus(status.MerkleValid, status.ErrorCode, "merkle")
		out = append(out, status)
	}
	return out, rows.Err()
}

func VerificationEvidenceStatus(valid bool, errorCode, check string) string {
	if valid {
		return "valid"
	}
	if (check == "header" && errorCode == "header_hash_mismatch") ||
		(check == "pow" && errorCode == "header_pow_invalid") ||
		(check == "merkle" && errorCode == "merkle_mismatch") {
		return "invalid"
	}
	return "unknown"
}

func (s *Store) VerificationPolicyStaleCount(engine int, tsaPolicy, otsPolicy []byte) (int, error) {
	return s.VerificationPolicyStaleCountContext(context.Background(), engine, tsaPolicy, otsPolicy)
}

func (s *Store) VerificationPolicyStaleCountContext(ctx context.Context, engine int, tsaPolicy, otsPolicy []byte) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM locations l
		WHERE NOT EXISTS (
			SELECT 1 FROM proof_verification_runs v
			WHERE v.id=(SELECT MAX(v2.id) FROM proof_verification_runs v2
				WHERE v2.location_id=l.id AND v2.kind='tsa')
			  AND v.engine_version=? AND v.policy_sha256=?
			  AND (v.trigger!='import' OR EXISTS (
				SELECT 1 FROM proof_verification_jobs j
				WHERE j.location_id=l.id AND j.kind='tsa'
			  ))
		) OR NOT EXISTS (
			SELECT 1 FROM proof_verification_runs v
			WHERE v.id=(SELECT MAX(v2.id) FROM proof_verification_runs v2
				WHERE v2.location_id=l.id AND v2.kind='ots')
			  AND v.engine_version=? AND v.policy_sha256=?
			  AND (v.trigger!='import' OR EXISTS (
				SELECT 1 FROM proof_verification_jobs j
				WHERE j.location_id=l.id AND j.kind='ots'
			  ))
		)`, engine, tsaPolicy, engine, otsPolicy).Scan(&count)
	return count, err
}

func (s *Store) VerificationStaleCount(cutoff int64, engine int, tsaPolicy, otsPolicy []byte) (int, error) {
	return s.VerificationStaleCountContext(context.Background(), cutoff, engine, tsaPolicy, otsPolicy)
}

func (s *Store) VerificationStaleCountContext(ctx context.Context, cutoff int64, engine int, tsaPolicy, otsPolicy []byte) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM locations l
		WHERE EXISTS (
			SELECT 1 FROM proof_verification_jobs j
			WHERE j.location_id=l.id AND j.state!='done'
		) OR NOT EXISTS (
			SELECT 1 FROM proof_verification_runs v
			WHERE v.id=(SELECT MAX(v2.id) FROM proof_verification_runs v2
				WHERE v2.location_id=l.id AND v2.kind='tsa')
			  AND v.checked_at>=? AND v.engine_version=? AND v.policy_sha256=?
			  AND (v.trigger!='import' OR EXISTS (
				SELECT 1 FROM proof_verification_jobs j
				WHERE j.location_id=l.id AND j.kind='tsa'
			  ))
		) OR NOT EXISTS (
			SELECT 1 FROM proof_verification_runs v
			WHERE v.id=(SELECT MAX(v2.id) FROM proof_verification_runs v2
				WHERE v2.location_id=l.id AND v2.kind='ots')
			  AND v.checked_at>=? AND v.engine_version=? AND v.policy_sha256=?
			  AND (v.trigger!='import' OR EXISTS (
				SELECT 1 FROM proof_verification_jobs j
				WHERE j.location_id=l.id AND j.kind='ots'
			  ))
		)`, cutoff, engine, tsaPolicy, cutoff, engine, otsPolicy).Scan(&count)
	return count, err
}

func (s *Store) IterateVerificationRuns(f ExportFilter, batch int, fn func([]VerificationRun) error) error {
	return iterateVerificationRuns(context.Background(), s.db, f, batch, fn)
}

func (s *ExportSnapshot) IterateVerificationRuns(batch int, fn func([]VerificationRun) error) error {
	return iterateVerificationRuns(s.ctx, s.tx, s.filter, batch, fn)
}

func iterateVerificationRuns(ctx context.Context, qr contextQueryer, f ExportFilter, batch int, fn func([]VerificationRun) error) error {
	if batch < 1 {
		batch = 500
	}
	lastID := int64(0)
	for {
		q := `SELECT ` + verificationJoinColumns + `
			FROM proof_verification_runs v
			JOIN locations l ON l.id=v.location_id
			WHERE v.id>?`
		args := []any{lastID}
		if f.From > 0 {
			q += ` AND l.tst>=?`
			args = append(args, f.From)
		}
		if f.To > 0 {
			q += ` AND l.tst<=?`
			args = append(args, f.To)
		}
		q += ` ORDER BY v.id LIMIT ?`
		args = append(args, batch)
		rows, err := qr.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		var runs []VerificationRun
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				_ = rows.Close()
				return err
			}
			run, err := scanVerification(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			runs = append(runs, *run)
			lastID = run.ID
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		if len(runs) == 0 {
			return nil
		}
		if err := fn(runs); err != nil {
			return err
		}
		if len(runs) < batch {
			return nil
		}
	}
}

func (s *Store) IterateVerificationAttempts(f ExportFilter, batch int, fn func([]VerificationAttempt) error) error {
	return iterateVerificationAttempts(context.Background(), s.db, f, batch, fn)
}

func (s *ExportSnapshot) IterateVerificationAttempts(batch int, fn func([]VerificationAttempt) error) error {
	return iterateVerificationAttempts(s.ctx, s.tx, s.filter, batch, fn)
}

func iterateVerificationAttempts(ctx context.Context, qr contextQueryer, f ExportFilter, batch int, fn func([]VerificationAttempt) error) error {
	if batch < 1 {
		batch = 500
	}
	lastID := int64(0)
	for {
		q := `SELECT a.id,a.run_id,a.provider_id,a.result,a.block_height,a.block_hash,a.duration_ms,a.error_code,
			a.tip_height,a.confirmations,a.header_valid,a.pow_valid,a.merkle_valid
			FROM proof_verification_attempts a
			JOIN proof_verification_runs v ON v.id=a.run_id
			JOIN locations l ON l.id=v.location_id
			WHERE a.id>?`
		args := []any{lastID}
		if f.From > 0 {
			q += ` AND l.tst>=?`
			args = append(args, f.From)
		}
		if f.To > 0 {
			q += ` AND l.tst<=?`
			args = append(args, f.To)
		}
		q += ` ORDER BY a.id LIMIT ?`
		args = append(args, batch)
		rows, err := qr.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		var attempts []VerificationAttempt
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				_ = rows.Close()
				return err
			}
			var attempt VerificationAttempt
			if err := rows.Scan(&attempt.ID, &attempt.RunID, &attempt.ProviderID,
				&attempt.Result, &attempt.BlockHeight, &attempt.BlockHash,
				&attempt.DurationMS, &attempt.ErrorCode, &attempt.TipHeight,
				&attempt.Confirmations, &attempt.HeaderValid, &attempt.POWValid,
				&attempt.MerkleValid); err != nil {
				_ = rows.Close()
				return err
			}
			attempts = append(attempts, attempt)
			lastID = attempt.ID
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		if len(attempts) == 0 {
			return nil
		}
		if err := fn(attempts); err != nil {
			return err
		}
		if len(attempts) < batch {
			return nil
		}
	}
}

func (s *Store) DeleteVerificationHistory(before int64) (int64, error) {
	res, err := s.db.Exec(`
		DELETE FROM proof_verification_runs
		WHERE checked_at<? AND id NOT IN (
			SELECT MAX(id) FROM proof_verification_runs GROUP BY location_id,kind
		)`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) VerificationStats() (VerificationStats, error) {
	return s.VerificationStatsContext(context.Background())
}

func (s *Store) VerificationStatsContext(ctx context.Context) (VerificationStats, error) {
	out := VerificationStats{Results: make(map[string]int)}
	rows, err := s.db.QueryContext(ctx, `
		SELECT result,COUNT(*) FROM proof_verification_runs v
		WHERE id=(SELECT MAX(v2.id) FROM proof_verification_runs v2
			WHERE v2.location_id=v.location_id AND v2.kind=v.kind)
		AND (v.trigger!='import' OR v.engine_version>0 AND EXISTS (
			SELECT 1 FROM proof_verification_jobs j
			WHERE j.location_id=v.location_id AND j.kind=v.kind
		))
		AND NOT EXISTS (
			SELECT 1 FROM proof_verification_jobs j
			WHERE j.location_id=v.location_id AND j.kind=v.kind AND j.state!='done'
		)
		GROUP BY result`)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var result string
		var n int
		if err := rows.Scan(&result, &n); err != nil {
			return out, err
		}
		out.Results[result] = n
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(checked_at),0) FROM proof_verification_runs
		WHERE result IN ('verified','degraded')
		  AND (trigger!='import' OR engine_version>0 AND EXISTS (
			SELECT 1 FROM proof_verification_jobs j
			WHERE j.location_id=proof_verification_runs.location_id
			  AND j.kind=proof_verification_runs.kind AND j.state='done'
		  ))`).Scan(&out.LastSuccess)
	return out, err
}

func (s *Store) ImportVerification(run *VerificationRun, attempts []VerificationAttempt) error {
	run.Attempts = attempts
	run.Trigger = "import"
	run.EngineVersion = 0
	return s.SaveVerification(run)
}

func (s *Store) verificationAttempts(runID int64) ([]VerificationAttempt, error) {
	rows, err := s.db.Query(`
		SELECT id,run_id,provider_id,result,block_height,block_hash,duration_ms,error_code,
			tip_height,confirmations,header_valid,pow_valid,merkle_valid
		FROM proof_verification_attempts WHERE run_id=? ORDER BY id LIMIT ?`, runID, MaxVerificationAttempts+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []VerificationAttempt
	providers := make(map[string]struct{}, MaxVerificationAttempts)
	for rows.Next() {
		var a VerificationAttempt
		if err := rows.Scan(&a.ID, &a.RunID, &a.ProviderID, &a.Result, &a.BlockHeight,
			&a.BlockHash, &a.DurationMS, &a.ErrorCode, &a.TipHeight, &a.Confirmations,
			&a.HeaderValid, &a.POWValid, &a.MerkleValid); err != nil {
			return nil, err
		}
		if len(out) >= MaxVerificationAttempts {
			return nil, ErrTooManyVerificationAttempts
		}
		if _, exists := providers[a.ProviderID]; exists {
			return nil, ErrDuplicateVerificationProvider
		}
		providers[a.ProviderID] = struct{}{}
		out = append(out, a)
	}
	return out, rows.Err()
}

func validateVerificationAttempts(attempts []VerificationAttempt) error {
	if len(attempts) > MaxVerificationAttempts {
		return ErrTooManyVerificationAttempts
	}
	providers := make(map[string]struct{}, len(attempts))
	for i := range attempts {
		if err := validateVerificationAttempt(&attempts[i]); err != nil {
			return err
		}
		if _, exists := providers[attempts[i].ProviderID]; exists {
			return ErrDuplicateVerificationProvider
		}
		providers[attempts[i].ProviderID] = struct{}{}
	}
	return nil
}

func validateVerificationAttempt(attempt *VerificationAttempt) error {
	if attempt == nil || strings.TrimSpace(attempt.ProviderID) == "" {
		return errors.New("verification provider is required")
	}
	return nil
}

func scanVerification(row scannable) (*VerificationRun, error) {
	var run VerificationRun
	err := row.Scan(&run.ID, &run.LocationID, &run.Kind, &run.Trigger, &run.Result,
		&run.ProofSHA256, &run.CheckedAt, &run.TSAGenTime, &run.TSAPolicyOID,
		&run.TSASerial, &run.TSASignerSubject, &run.TSASignerFingerprint,
		&run.BitcoinHeight, &run.BitcoinBlockHash, &run.AgreeingSources,
		&run.SourceCount, &run.ErrorCode, &run.EngineVersion, &run.PolicySHA256,
		&run.TSANonceStatus, &run.TSAPolicyStatus, &run.TSATrustMode,
		&run.TSAAccuracyMS, &run.TSATrustAnchorFingerprint, &run.BitcoinPOWValid,
		&run.BitcoinConfirmations)
	return &run, err
}

func ValidateVerification(run *VerificationRun) error {
	if run == nil {
		return errors.New("verification is nil")
	}
	if run.LocationID < 1 || (run.Kind != "tsa" && run.Kind != "ots") {
		return fmt.Errorf("invalid verification identity")
	}
	if run.CheckedAt == 0 {
		run.CheckedAt = time.Now().Unix()
	}
	return nil
}
