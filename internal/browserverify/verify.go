package browserverify

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/prepaser/mytracker/internal/ots"
	"github.com/prepaser/mytracker/internal/providerurl"
	"github.com/prepaser/mytracker/internal/tsa"
	"github.com/prepaser/mytracker/internal/verification"
)

const (
	maxArchiveBytes       = 64 << 20
	maxUncompressedBytes  = 256 << 20
	maxProofBytes         = 8 << 20
	maxRootsPEMBytes      = 1 << 20
	maxLocations          = 250000
	maxProofEntries       = 2 * maxLocations
	maxVerificationRuns   = 500000
	maxVerificationTries  = 1000000
	maxVerificationSource = 5
	maxHistoryResultText  = 32
	maxHistoryTriggerText = 64
	maxHistoryOIDText     = 128
	maxHistorySerialText  = 256
	maxHistorySubjectText = 1024
	maxHistoryHashText    = 64
	maxHistoryStatusText  = 32
	maxHistoryErrorText   = 64
)

var (
	ErrInvalidOptions = errors.New("invalid verification options")
	ErrInvalidArchive = errors.New("invalid archive")
	ErrIntegrity      = errors.New("archive integrity check failed")
)

type Options struct {
	RootsPEM         string   `json:"roots_pem"`
	PolicyOIDs       []string `json:"policy_oids"`
	Providers        []string `json:"providers"`
	MinConfirmations int      `json:"min_confirmations"`
	Online           bool     `json:"online"`
}

type Progress struct {
	Phase     string `json:"phase"`
	Completed int    `json:"completed"`
	Total     int    `json:"total"`
}

type Report struct {
	GeneratedAt    string           `json:"generated_at"`
	Version        int              `json:"version"`
	ArchiveVersion int              `json:"archive_version"`
	Integrity      string           `json:"integrity"`
	Overall        string           `json:"overall"`
	Entries        int              `json:"entries"`
	Policy         PolicySummary    `json:"policy"`
	Locations      []LocationResult `json:"locations"`
}

type PolicySummary struct {
	TSARootsConfigured  bool     `json:"tsa_roots_configured"`
	TSAPolicyOIDs       []string `json:"tsa_policy_oids"`
	OTSProviders        int      `json:"ots_providers"`
	OTSMinConfirmations int      `json:"ots_min_confirmations"`
	Online              bool     `json:"online"`
}

type LocationResult struct {
	ID      int64  `json:"id,string"`
	Digest  string `json:"digest_sha256"`
	Overall string `json:"overall"`
	TSA     Proof  `json:"tsa"`
	OTS     Proof  `json:"ots"`
}

type Proof struct {
	Result    string         `json:"result"`
	ErrorCode string         `json:"error_code,omitempty"`
	Evidence  map[string]any `json:"evidence,omitempty"`
}

type preparedLocation struct {
	archiveRecord locationRecord
	result        LocationResult
}

func Verify(ctx context.Context, data []byte, opts Options, progress func(Progress)) (Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateOptions(opts); err != nil {
		return Report{}, ErrInvalidOptions
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if progress != nil {
		progress(Progress{Phase: "validate", Total: 1})
	}
	archive, err := openArchive(ctx, data, progress)
	if err != nil {
		return Report{}, err
	}
	records, err := validateRecords(ctx, archive, progress)
	if err != nil {
		return Report{}, err
	}
	if err := validateHistory(ctx, archive, records, progress); err != nil {
		return Report{}, err
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}

	rootsPEM := make([]byte, len(opts.RootsPEM))
	copy(rootsPEM, opts.RootsPEM)
	prepared := make([]preparedLocation, 0, len(records))
	if progress != nil {
		progress(Progress{Phase: "proofs", Total: len(records)})
	}
	for i, rec := range records {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		digestBytes, _ := hex.DecodeString(rec.record.DigestSHA256)
		var digest [sha256.Size]byte
		copy(digest[:], digestBytes)
		item := LocationResult{ID: rec.record.ID, Digest: rec.record.DigestSHA256}
		item.TSA, err = verifyTSA(ctx, archive, rec, digest, rootsPEM, opts.PolicyOIDs)
		if err != nil {
			return Report{}, err
		}
		item.OTS, err = verifyOTSLocal(ctx, archive, rec, digest)
		if err != nil {
			return Report{}, err
		}
		item.Overall = locationOverall(item.TSA.Result, item.OTS.Result)
		prepared = append(prepared, preparedLocation{archiveRecord: rec, result: item})
		if progress != nil {
			progress(Progress{Phase: "proofs", Completed: i + 1, Total: len(records)})
		}
	}

	if opts.Online {
		for i := range prepared {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			rec := prepared[i].archiveRecord
			if rec.record.Proofs.OTS != "" {
				proof, err := readProof(ctx, archive.files[rec.record.Proofs.OTS])
				if err != nil {
					return Report{}, err
				}
				digestBytes, _ := hex.DecodeString(rec.record.DigestSHA256)
				var digest [sha256.Size]byte
				copy(digest[:], digestBytes)
				verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				result, verifyErr := ots.VerifyPool(verifyCtx, proof, digest, opts.Providers, opts.MinConfirmations)
				cancel()
				if err := ctx.Err(); err != nil {
					return Report{}, err
				}
				prepared[i].result.OTS = otsProof(result, verifyErr, true)
				prepared[i].result.Overall = locationOverall(prepared[i].result.TSA.Result, prepared[i].result.OTS.Result)
			}
			if progress != nil {
				progress(Progress{Phase: "verify", Completed: i + 1, Total: len(prepared)})
			}
		}
	}

	report := Report{
		Version: 1, ArchiveVersion: 6, Integrity: "valid", Overall: "verified", GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Entries: len(archive.files), Locations: make([]LocationResult, len(prepared)),
		Policy: PolicySummary{
			TSARootsConfigured: len(opts.RootsPEM) != 0,
			TSAPolicyOIDs:      append([]string{}, opts.PolicyOIDs...), OTSProviders: len(opts.Providers),
			OTSMinConfirmations: opts.MinConfirmations, Online: opts.Online,
		},
	}
	if len(prepared) == 0 {
		report.Overall = "inconclusive"
	}
	for i := range prepared {
		report.Locations[i] = prepared[i].result
		report.Overall = mergeOverall(report.Overall, prepared[i].result.Overall)
	}
	if progress != nil {
		progress(Progress{Phase: "complete", Completed: len(prepared), Total: len(prepared)})
	}
	return report, nil
}

func ValidateOptions(opts Options) error {
	if len(opts.RootsPEM) > maxRootsPEMBytes || opts.MinConfirmations < 1 || opts.MinConfirmations > 144 ||
		len(opts.PolicyOIDs) > 16 || len(opts.Providers) > maxVerificationSource {
		return ErrInvalidOptions
	}
	if opts.Online && len(opts.Providers) == 0 {
		return ErrInvalidOptions
	}
	seenOIDs := make(map[string]struct{}, len(opts.PolicyOIDs))
	for _, oid := range opts.PolicyOIDs {
		if !verification.ValidPolicyOID(oid) {
			return ErrInvalidOptions
		}
		if _, ok := seenOIDs[oid]; ok {
			return ErrInvalidOptions
		}
		seenOIDs[oid] = struct{}{}
	}
	seenProviders := make(map[string]struct{}, len(opts.Providers))
	for _, raw := range opts.Providers {
		provider, err := cleanProvider(raw)
		if err != nil {
			return ErrInvalidOptions
		}
		if _, ok := seenProviders[provider]; ok {
			return ErrInvalidOptions
		}
		seenProviders[provider] = struct{}{}
	}
	if len(opts.RootsPEM) != 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(opts.RootsPEM)) {
			return ErrInvalidOptions
		}
	}
	return nil
}

func validateOptions(opts Options) error { return ValidateOptions(opts) }

func cleanProvider(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", ErrInvalidOptions
	}
	clean, err := providerurl.NormalizeLoopbackHTTP(raw)
	if err != nil {
		return "", ErrInvalidOptions
	}
	return clean, nil
}

func verifyTSA(ctx context.Context, archive *archiveData, rec locationRecord, digest [sha256.Size]byte, rootsPEM []byte, policyOIDs []string) (Proof, error) {
	name := rec.record.Proofs.TSR
	if name == "" {
		return Proof{Result: "missing", ErrorCode: "proof_missing"}, nil
	}
	proof, err := readProof(ctx, archive.files[name])
	if err != nil {
		return Proof{}, err
	}
	nonce, _ := hex.DecodeString(rec.record.TSANonceHex)
	verified, verifyErr := tsa.VerifyTokenPolicy(proof, digest, tsa.TrustPolicy{
		RootsPEM: rootsPEM, PolicyOIDs: policyOIDs, ExpectedNonce: nonce,
	})
	if verifyErr != nil {
		code := "tsa_invalid"
		result := "invalid"
		switch {
		case errors.Is(verifyErr, tsa.ErrNonceMismatch):
			code = "tsa_nonce_mismatch"
		case errors.Is(verifyErr, tsa.ErrPolicyMismatch):
			code = "tsa_policy_mismatch"
		case errors.Is(verifyErr, tsa.ErrTrustUnavailable):
			result, code = "error", "tsa_trust_unavailable"
		case strings.Contains(verifyErr.Error(), "digest mismatch"):
			code = "digest_mismatch"
		}
		return Proof{Result: result, ErrorCode: code}, nil
	}
	evidence := map[string]any{
		"signer_subject": verified.SignerSubject, "signer_fingerprint": verified.SignerFingerprint,
		"trust_anchor_fingerprint": verified.TrustAnchorFingerprint, "serial": verified.Serial,
		"gen_time": verified.GenTime.Unix(), "policy_oid": verified.PolicyOID,
		"nonce_status": verified.NonceStatus, "policy_status": verified.PolicyStatus,
		"trust_mode": verified.TrustMode, "accuracy_ms": verified.Accuracy.Milliseconds(),
	}
	result := "degraded"
	if verified.Pinned && verified.NonceStatus == "matched" && verified.PolicyStatus == "matched" {
		result = "verified"
	}
	return Proof{Result: result, Evidence: evidence}, nil
}

func verifyOTSLocal(ctx context.Context, archive *archiveData, rec locationRecord, digest [sha256.Size]byte) (Proof, error) {
	name := rec.record.Proofs.OTS
	if name == "" {
		return Proof{Result: "missing", ErrorCode: "proof_missing"}, nil
	}
	proof, err := readProof(ctx, archive.files[name])
	if err != nil {
		return Proof{}, err
	}
	result, verifyErr := ots.VerifyPool(ctx, proof, digest, nil)
	return otsProof(result, verifyErr, false), nil
}

func otsProof(result *ots.PoolResult, verifyErr error, online bool) Proof {
	if result == nil {
		return Proof{Result: "error", ErrorCode: "proof_parse"}
	}
	if verifyErr != nil {
		code := "provider_unavailable"
		switch verifyErr.Error() {
		case "proof_parse":
			return Proof{Result: "invalid", ErrorCode: "proof_parse"}
		case "digest_mismatch":
			return Proof{Result: "invalid", ErrorCode: "digest_mismatch"}
		case "no_providers":
			if !online {
				return Proof{Result: "inconclusive", ErrorCode: "offline"}
			}
			code = "provider_unavailable"
		}
		if result.Result == "invalid" {
			return Proof{Result: "invalid", ErrorCode: code}
		}
		return Proof{Result: "error", ErrorCode: code}
	}
	evidence := otsEvidence(result)
	proof := Proof{Result: result.Result, Evidence: evidence}
	proof.ErrorCode = safeOTSError(result.ErrorCode)
	return proof
}

func otsEvidence(result *ots.PoolResult) map[string]any {
	evidence := make(map[string]any)
	anchor := result.Height >= 0 && result.Height <= 100_000_000 && validBlockHash(result.BlockHash)
	if anchor {
		evidence["height"] = result.Height
		evidence["block_hash"] = strings.ToLower(result.BlockHash)
	}
	if powValid, known := otsPOWValidity(result); known {
		evidence["pow_valid"] = powValid
	}
	if anchor {
		confirmations := result.Confirmations
		for _, attempt := range result.Attempts {
			if attempt.Height == result.Height && strings.EqualFold(attempt.BlockHash, result.BlockHash) && attempt.Confirmations > confirmations {
				confirmations = attempt.Confirmations
			}
		}
		if confirmations > 0 && confirmations <= 100_000_000 {
			evidence["confirmations"] = confirmations
		}
	}
	if anchor && result.Agreeing >= 0 && result.Agreeing <= maxVerificationSource {
		evidence["agreeing_sources"] = result.Agreeing
	}
	if len(result.Attempts) != 0 {
		attempts := make([]map[string]any, 0, len(result.Attempts))
		for _, attempt := range result.Attempts {
			if len(attempts) == maxVerificationSource {
				break
			}
			providerID := attempt.ProviderID
			if !validProviderID(providerID) {
				continue
			}
			item := map[string]any{"provider_id": providerID, "result": safeOTSResult(attempt.Result)}
			if code := safeOTSError(attempt.ErrorCode); code != "" {
				item["error_code"] = code
			}
			headerObserved := validBlockHash(attempt.BlockHash)
			if headerObserved && attempt.Height >= 0 && attempt.Height <= 100_000_000 {
				item["height"] = attempt.Height
				item["block_hash"] = strings.ToLower(attempt.BlockHash)
				item["header_valid"] = attempt.HeaderValid
				if attempt.HeaderValid {
					item["pow_valid"] = attempt.POWValid
					if attempt.MerkleValid || attempt.ErrorCode == "merkle_mismatch" {
						item["merkle_valid"] = attempt.MerkleValid
					}
				}
			}
			tipObserved := attempt.TipHeight > 0 || attempt.Confirmations > 0 || attempt.ErrorCode == "tip_before_anchor"
			if headerObserved && tipObserved && attempt.TipHeight >= 0 && attempt.TipHeight <= 100_000_000 {
				item["tip_height"] = attempt.TipHeight
			}
			if headerObserved && attempt.Confirmations > 0 && attempt.Confirmations <= 100_000_000 {
				item["confirmations"] = attempt.Confirmations
			}
			if attempt.Duration >= 0 && attempt.Duration <= time.Hour {
				item["duration_ms"] = attempt.Duration.Milliseconds()
			}
			attempts = append(attempts, item)
		}
		evidence["attempts"] = attempts
	}
	return evidence
}

func otsPOWValidity(result *ots.PoolResult) (bool, bool) {
	if result.Height < 0 || result.Height > 100_000_000 || !validBlockHash(result.BlockHash) {
		return false, false
	}
	for _, attempt := range result.Attempts {
		if attempt.Height != result.Height || !strings.EqualFold(attempt.BlockHash, result.BlockHash) {
			continue
		}
		if result.POWValid && attempt.HeaderValid {
			return true, true
		}
		if attempt.HeaderValid && attempt.POWValid {
			return true, true
		}
		if attempt.ErrorCode == "header_pow_invalid" {
			return false, true
		}
	}
	return false, false
}

func validBlockHash(raw string) bool {
	if len(raw) != 64 {
		return false
	}
	_, err := hex.DecodeString(raw)
	return err == nil
}

func validProviderID(raw string) bool {
	if !strings.HasPrefix(raw, "esplora-") {
		return false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(raw, "esplora-"))
	return err == nil && n >= 1 && n <= maxVerificationSource
}

func safeOTSResult(raw string) string {
	switch raw {
	case "verified", "degraded", "pending", "inconclusive", "invalid", "error":
		return raw
	default:
		return "error"
	}
}

func safeOTSError(raw string) string {
	switch raw {
	case "header_hash_mismatch", "header_pow_invalid", "tip_before_anchor", "merkle_mismatch",
		"confirmations_pending", "provider_timeout", "provider_unavailable":
		return raw
	default:
		return ""
	}
}

func locationOverall(results ...string) string {
	out := "verified"
	for _, result := range results {
		out = mergeOverall(out, result)
	}
	return out
}

func mergeOverall(current, next string) string {
	return verification.MergeOverall(current, next)
}
