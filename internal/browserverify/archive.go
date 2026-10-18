package browserverify

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/verification"
	"github.com/prepaser/mytracker/internal/zipguard"
)

type manifest struct {
	Version   int               `json:"version"`
	Locations int               `json:"locations"`
	Runs      int               `json:"verification_runs"`
	Attempts  int               `json:"verification_attempts"`
	Entries   map[string]string `json:"entry_sha256"`
}

type archiveData struct {
	files    map[string]*zip.File
	manifest manifest
}

type record struct {
	ID           int64           `json:"id"`
	User         string          `json:"user"`
	Device       string          `json:"device"`
	EventUnix    int64           `json:"event_unix"`
	EventTimeUTC string          `json:"event_time_utc"`
	ReceivedUTC  string          `json:"received_utc"`
	Latitude     float64         `json:"latitude"`
	Longitude    float64         `json:"longitude"`
	DigestSHA256 string          `json:"digest_sha256"`
	TSANonceHex  string          `json:"tsa_nonce"`
	TsaStatus    string          `json:"tsa_status"`
	OtsStatus    string          `json:"ots_status"`
	TsaTimeUTC   string          `json:"tsa_time_utc"`
	Payload      json.RawMessage `json:"payload"`
	Proofs       struct {
		TSR string `json:"tsr"`
		OTS string `json:"ots"`
	} `json:"proofs"`
}

type locationRecord struct{ record record }

type historyRun struct {
	ID                        int64            `json:"id"`
	LocationID                int64            `json:"location_id"`
	Kind                      string           `json:"kind"`
	Trigger                   string           `json:"trigger"`
	Result                    string           `json:"result"`
	CheckedAt                 int64            `json:"checked_at"`
	TSAGenTime                *int64           `json:"tsa_gen_time,omitempty"`
	TSAPolicyOID              string           `json:"tsa_policy_oid,omitempty"`
	TSASerial                 string           `json:"tsa_serial,omitempty"`
	TSASignerSubject          string           `json:"tsa_signer_subject,omitempty"`
	TSASignerFingerprint      string           `json:"tsa_signer_fingerprint,omitempty"`
	BitcoinHeight             *int64           `json:"bitcoin_height,omitempty"`
	BitcoinBlockHash          string           `json:"bitcoin_block_hash,omitempty"`
	AgreeingSources           int              `json:"agreeing_sources"`
	SourceCount               int              `json:"source_count"`
	ErrorCode                 string           `json:"error_code,omitempty"`
	TSANonceStatus            string           `json:"tsa_nonce_status,omitempty"`
	TSAPolicyStatus           string           `json:"tsa_policy_status,omitempty"`
	TSATrustMode              string           `json:"tsa_trust_mode,omitempty"`
	TSAAccuracyMS             int64            `json:"tsa_accuracy_ms,omitempty"`
	TSATrustAnchorFingerprint string           `json:"tsa_trust_anchor_fingerprint,omitempty"`
	BitcoinPOWValid           bool             `json:"bitcoin_pow_valid,omitempty"`
	BitcoinConfirmations      int64            `json:"bitcoin_confirmations,omitempty"`
	EngineVersion             int              `json:"engine_version"`
	Proof                     string           `json:"proof_sha256"`
	Policy                    string           `json:"policy_sha256"`
	Attempts                  []historyAttempt `json:"attempts,omitempty"`
}

type historyAttempt struct {
	ID            int64  `json:"id"`
	RunID         int64  `json:"run_id"`
	Provider      string `json:"provider_id"`
	Result        string `json:"result"`
	BlockHeight   *int64 `json:"block_height,omitempty"`
	BlockHash     string `json:"block_hash,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
	ErrorCode     string `json:"error_code,omitempty"`
	TipHeight     int64  `json:"tip_height"`
	Confirmations int64  `json:"confirmations"`
	HeaderValid   bool   `json:"header_valid,omitempty"`
	POWValid      bool   `json:"pow_valid,omitempty"`
	MerkleValid   bool   `json:"merkle_valid,omitempty"`
}

func openArchive(ctx context.Context, data []byte, progress func(Progress)) (*archiveData, error) {
	if len(data) == 0 || len(data) > maxArchiveBytes {
		return nil, ErrInvalidArchive
	}
	zr, err := zipguard.NewReader(ctx, bytes.NewReader(data), int64(len(data)), maxProofEntries+6)
	if err != nil {
		return nil, ErrInvalidArchive
	}
	a := &archiveData{files: make(map[string]*zip.File, len(zr.File))}
	var total uint64
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, ok := a.files[f.Name]; ok {
			return nil, ErrInvalidArchive
		}
		if f.Flags&1 != 0 || f.FileInfo().IsDir() {
			return nil, ErrInvalidArchive
		}
		switch f.Name {
		case "manifest.json", "README.txt", "locations.jsonl", "locations.csv", "verification_runs.jsonl", "verification_attempts.jsonl":
		default:
			if !proofName(f.Name) || f.UncompressedSize64 > maxProofBytes {
				return nil, ErrInvalidArchive
			}
		}
		if f.UncompressedSize64 > maxUncompressedBytes-total {
			return nil, ErrInvalidArchive
		}
		total += f.UncompressedSize64
		a.files[f.Name] = f
	}
	for _, name := range []string{"manifest.json", "README.txt", "locations.jsonl", "locations.csv", "verification_runs.jsonl", "verification_attempts.jsonl"} {
		if a.files[name] == nil {
			return nil, ErrInvalidArchive
		}
	}
	manifestBytes, err := readEntry(ctx, a.files["manifest.json"], 64<<20)
	if err != nil || json.Unmarshal(manifestBytes, &a.manifest) != nil {
		return nil, ErrInvalidArchive
	}
	m := a.manifest
	if m.Version != 6 || m.Locations < 0 || m.Locations > maxLocations || m.Runs < 0 || m.Runs > maxVerificationRuns || m.Attempts < 0 || m.Attempts > maxVerificationTries || len(m.Entries) != len(a.files)-1 {
		return nil, ErrInvalidArchive
	}
	for i, f := range zr.File {
		if f.Name == "manifest.json" {
			continue
		}
		want, ok := m.Entries[f.Name]
		if !ok || !validBlockHash(want) {
			return nil, ErrIntegrity
		}
		r, err := f.Open()
		if err != nil {
			return nil, ErrIntegrity
		}
		h := sha256.New()
		n, copyErr := io.Copy(h, io.LimitReader(contextReader{ctx, r}, maxUncompressedBytes+1))
		closeErr := r.Close()
		if copyErr != nil || closeErr != nil || uint64(n) != f.UncompressedSize64 || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want) {
			return nil, ErrIntegrity
		}
		if progress != nil {
			progress(Progress{Phase: "integrity", Completed: i + 1, Total: len(zr.File)})
		}
	}
	return a, nil
}

func proofName(name string) bool {
	if !strings.HasPrefix(name, "proofs/") {
		return false
	}
	base := strings.TrimPrefix(name, "proofs/")
	if !strings.HasSuffix(base, ".tsr") && !strings.HasSuffix(base, ".ots") {
		return false
	}
	id, err := strconv.ParseInt(base[:len(base)-4], 10, 64)
	return err == nil && id > 0 && strconv.FormatInt(id, 10) == base[:len(base)-4]
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func readEntry(ctx context.Context, f *zip.File, limit int64) ([]byte, error) {
	if f == nil || f.UncompressedSize64 > uint64(limit) {
		return nil, ErrInvalidArchive
	}
	r, err := f.Open()
	if err != nil {
		return nil, ErrIntegrity
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, r}, limit+1))
	if err != nil || int64(len(data)) > limit || uint64(len(data)) != f.UncompressedSize64 {
		return nil, ErrIntegrity
	}
	return data, nil
}

func readProof(ctx context.Context, f *zip.File) ([]byte, error) {
	return readEntry(ctx, f, maxProofBytes)
}

func records(ctx context.Context, f *zip.File, limit int, fn func([]byte) error) (int, error) {
	r, err := f.Open()
	if err != nil {
		return 0, ErrIntegrity
	}
	defer func() { _ = r.Close() }()
	scanner := bufio.NewScanner(contextReader{ctx, r})
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	n := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if n >= limit {
			return n, ErrInvalidArchive
		}
		if err := fn(line); err != nil {
			return n, err
		}
		n++
	}
	if scanner.Err() != nil {
		return n, ErrInvalidArchive
	}
	return n, nil
}

func validateRecords(ctx context.Context, a *archiveData, progress func(Progress)) ([]locationRecord, error) {
	result := make([]locationRecord, 0, a.manifest.Locations)
	ids := make(map[int64]bool, a.manifest.Locations)
	refs := make(map[string]bool)
	n, err := records(ctx, a.files["locations.jsonl"], maxLocations, func(line []byte) error {
		var rec record
		if json.Unmarshal(line, &rec) != nil || !validRecord(rec) || ids[rec.ID] {
			return ErrInvalidArchive
		}
		ids[rec.ID] = true
		for _, proof := range []struct{ name, ext string }{{rec.Proofs.TSR, ".tsr"}, {rec.Proofs.OTS, ".ots"}} {
			if proof.name == "" {
				continue
			}
			if proof.name != fmt.Sprintf("proofs/%d%s", rec.ID, proof.ext) || a.files[proof.name] == nil || refs[proof.name] {
				return ErrInvalidArchive
			}
			refs[proof.name] = true
		}
		rec.Payload = nil
		rec.User, rec.Device = "", ""
		result = append(result, locationRecord{rec})
		if progress != nil {
			progress(Progress{Phase: "locations", Completed: len(result), Total: a.manifest.Locations})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if n != a.manifest.Locations || len(refs) != len(a.files)-6 {
		return nil, ErrInvalidArchive
	}
	return result, nil
}

func validIdentity(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validRecord(rec record) bool {
	if rec.ID <= 0 || !validIdentity(rec.User) || !validIdentity(rec.Device) || len(rec.Payload) > 1<<20 || !validBlockHash(rec.DigestSHA256) {
		return false
	}
	if rec.TSANonceHex != "" {
		b, err := hex.DecodeString(rec.TSANonceHex)
		if err != nil || len(b) > 16 {
			return false
		}
	}
	var raw map[string]any
	if json.Unmarshal(rec.Payload, &raw) != nil {
		return false
	}
	can, err := canonical.FromLocation(rec.User, rec.Device, raw)
	if err != nil || can.Tst != rec.EventUnix || can.Lat != rec.Latitude || can.Lon != rec.Longitude {
		return false
	}
	event, err := time.Parse(time.RFC3339, rec.EventTimeUTC)
	if err != nil || event.Unix() != can.Tst {
		return false
	}
	received, err := time.Parse(time.RFC3339, rec.ReceivedUTC)
	if err != nil || received.Unix() < 0 {
		return false
	}
	if rec.TsaTimeUTC != "" {
		at, err := time.Parse(time.RFC3339, rec.TsaTimeUTC)
		if err != nil || at.Unix() < 0 {
			return false
		}
	}
	if rec.TsaStatus != "pending" && rec.TsaStatus != "ok" && rec.TsaStatus != "fail" {
		return false
	}
	if rec.OtsStatus != "pending" && rec.OtsStatus != "submitted" && rec.OtsStatus != "confirmed" && rec.OtsStatus != "fail" {
		return false
	}
	digest, err := can.Digest()
	return err == nil && strings.EqualFold(hex.EncodeToString(digest[:]), rec.DigestSHA256)
}

func validateHistory(ctx context.Context, a *archiveData, locations []locationRecord, progress func(Progress)) error {
	ids := make(map[int64]bool, len(locations))
	for _, rec := range locations {
		ids[rec.record.ID] = true
	}
	runs := make(map[int64]bool)
	n, err := records(ctx, a.files["verification_runs.jsonl"], maxVerificationRuns, func(line []byte) error {
		var r historyRun
		if json.Unmarshal(line, &r) != nil || !validHistoryRun(r) || runs[r.ID] || !ids[r.LocationID] {
			return ErrInvalidArchive
		}
		if (r.Proof != "" && !validBlockHash(r.Proof)) || (r.Policy != "" && !validBlockHash(r.Policy)) {
			return ErrInvalidArchive
		}
		runs[r.ID] = true
		return nil
	})
	if err != nil {
		return err
	}
	if n != a.manifest.Runs {
		return ErrInvalidArchive
	}
	attemptIDs := make(map[int64]bool)
	providers := make(map[int64]map[string]bool)
	n, err = records(ctx, a.files["verification_attempts.jsonl"], maxVerificationTries, func(line []byte) error {
		var r historyAttempt
		if json.Unmarshal(line, &r) != nil || !validHistoryAttempt(r) || attemptIDs[r.ID] || !runs[r.RunID] {
			return ErrInvalidArchive
		}
		if providers[r.RunID] == nil {
			providers[r.RunID] = make(map[string]bool)
		}
		if providers[r.RunID][r.Provider] {
			return ErrInvalidArchive
		}
		providers[r.RunID][r.Provider] = true
		attemptIDs[r.ID] = true
		return nil
	})
	if err != nil {
		return err
	}
	if n != a.manifest.Attempts {
		return ErrInvalidArchive
	}
	return nil
}

func validResult(s string) bool {
	switch s {
	case "verified", "degraded", "pending", "inconclusive", "invalid", "error", "missing":
		return true
	}
	return false
}

func validHistoryRun(r historyRun) bool {
	return r.ID > 0 && r.LocationID > 0 && (r.Kind == "tsa" || r.Kind == "ots") &&
		validHistoryText(r.Trigger, maxHistoryTriggerText) && validHistoryText(r.Result, maxHistoryResultText) &&
		validHistoryText(r.TSAPolicyOID, maxHistoryOIDText) && validHistoryText(r.TSASerial, maxHistorySerialText) &&
		validHistoryText(r.TSASignerSubject, maxHistorySubjectText) && validHistoryText(r.TSASignerFingerprint, maxHistoryHashText) &&
		validHistoryText(r.BitcoinBlockHash, maxHistoryHashText) && validHistoryText(r.ErrorCode, maxHistoryErrorText) &&
		validHistoryText(r.TSANonceStatus, maxHistoryStatusText) && validHistoryText(r.TSAPolicyStatus, maxHistoryStatusText) &&
		validHistoryText(r.TSATrustMode, maxHistoryStatusText) && validHistoryText(r.Proof, maxHistoryHashText) &&
		validHistoryText(r.Policy, maxHistoryHashText) && validResult(r.Result) &&
		r.CheckedAt >= 0 && r.CheckedAt <= time.Now().Add(5*time.Minute).Unix() &&
		validHistoryEvidence(r) && validHistoryErrorCode(r.ErrorCode)
}

func validHistoryAttempt(r historyAttempt) bool {
	return r.ID > 0 && r.RunID > 0 && validHistoryProvider(r.Provider) &&
		validHistoryText(r.Result, maxHistoryResultText) && validHistoryText(r.BlockHash, maxHistoryHashText) &&
		validHistoryText(r.ErrorCode, maxHistoryErrorText) && r.DurationMS >= 0 && r.TipHeight >= 0 &&
		r.Confirmations >= 0 && (r.BlockHeight == nil || *r.BlockHeight >= 0) &&
		(r.Result == "verified" || r.Result == "pending" || r.Result == "invalid" || r.Result == "error") &&
		validHistoryErrorCode(r.ErrorCode)
}

func validHistoryEvidence(r historyRun) bool {
	return (r.TSAGenTime == nil || *r.TSAGenTime >= 0) &&
		(r.BitcoinHeight == nil || *r.BitcoinHeight >= 0) && r.TSAAccuracyMS >= 0 &&
		r.AgreeingSources >= 0 && r.SourceCount >= 0 && r.BitcoinConfirmations >= 0
}

func validHistoryText(value string, maximum int) bool {
	return len(value) <= maximum && utf8.ValidString(value)
}

func validHistoryProvider(value string) bool {
	return len(value) == len("esplora-1") && strings.HasPrefix(value, "esplora-") && value[len(value)-1] >= '1' && value[len(value)-1] <= '5'
}

func validHistoryErrorCode(code string) bool {
	return verification.ValidPersistedErrorCode(code)
}
