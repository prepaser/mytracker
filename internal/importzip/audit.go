package importzip

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/export"
	"github.com/prepaser/mytracker/internal/ots"
	"github.com/prepaser/mytracker/internal/tsa"
	"github.com/prepaser/mytracker/internal/zipguard"
)

type AuditReport struct {
	Version        int             `json:"version"`
	ArchiveVersion int             `json:"archive_version"`
	GeneratedAt    string          `json:"generated_at"`
	Overall        string          `json:"overall"`
	Policy         AuditPolicy     `json:"policy"`
	Locations      []AuditLocation `json:"locations"`
	locationCount  int
}

type AuditPolicy struct {
	TSARootsConfigured  bool     `json:"tsa_roots_configured"`
	TSAPolicyOIDs       []string `json:"tsa_policy_oids"`
	OTSProviders        int      `json:"ots_providers"`
	OTSMinConfirmations int      `json:"ots_min_confirmations"`
	OnlineRevocation    bool     `json:"online_revocation"`
}

type AuditLocation struct {
	ID      int64      `json:"id"`
	Digest  string     `json:"digest_sha256"`
	Overall string     `json:"overall"`
	TSA     AuditProof `json:"tsa"`
	OTS     AuditProof `json:"ots"`
}

type AuditProof struct {
	Result                 string         `json:"result"`
	ErrorCode              string         `json:"error_code,omitempty"`
	GenTime                int64          `json:"gen_time,omitempty"`
	PolicyOID              string         `json:"policy_oid,omitempty"`
	Serial                 string         `json:"serial,omitempty"`
	SignerSubject          string         `json:"signer_subject,omitempty"`
	SignerFingerprint      string         `json:"signer_fingerprint,omitempty"`
	TrustAnchorFingerprint string         `json:"trust_anchor_fingerprint,omitempty"`
	NonceStatus            string         `json:"nonce_status,omitempty"`
	PolicyStatus           string         `json:"policy_status,omitempty"`
	TrustMode              string         `json:"trust_mode,omitempty"`
	AccuracyMS             int64          `json:"accuracy_ms,omitempty"`
	BitcoinHeight          int64          `json:"bitcoin_height,omitempty"`
	BitcoinBlockHash       string         `json:"bitcoin_block_hash,omitempty"`
	BitcoinPOWValid        bool           `json:"bitcoin_pow_valid,omitempty"`
	BitcoinConfirmations   int64          `json:"bitcoin_confirmations,omitempty"`
	AgreeingSources        int            `json:"agreeing_sources,omitempty"`
	Attempts               []AuditAttempt `json:"attempts,omitempty"`
}

type AuditAttempt struct {
	ProviderID    string `json:"provider_id"`
	Result        string `json:"result"`
	ErrorCode     string `json:"error_code,omitempty"`
	BlockHeight   int64  `json:"block_height,omitempty"`
	BlockHash     string `json:"block_hash,omitempty"`
	TipHeight     int64  `json:"tip_height,omitempty"`
	Confirmations int64  `json:"confirmations,omitempty"`
	HeaderValid   bool   `json:"header_valid,omitempty"`
	POWValid      bool   `json:"pow_valid,omitempty"`
	MerkleValid   bool   `json:"merkle_valid,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
}

func AuditFile(ctx context.Context, path string, cfg *config.Config) (AuditReport, error) {
	return auditFile(ctx, path, cfg, nil)
}

// AuditFileTo verifies an archive and writes the complete report without
// retaining all location results in memory. The output is written only after
// archive validation and proof verification finish.
func AuditFileTo(ctx context.Context, path string, cfg *config.Config, w io.Writer, format string) (AuditReport, error) {
	if format != "json" && format != "text" {
		return AuditReport{}, fmt.Errorf("format must be json or text")
	}
	spool, err := os.CreateTemp("", ".mytracker-audit-output-*")
	if err != nil {
		return AuditReport{}, err
	}
	spoolPath := spool.Name()
	defer func() {
		_ = spool.Close()
		_ = os.Remove(spoolPath)
	}()
	consume := func(item AuditLocation) error {
		if format == "text" {
			_, err := fmt.Fprintf(spool, "%d %s TSA=%s OTS=%s\n", item.ID, item.Digest, item.TSA.Result, item.OTS.Result)
			return err
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err := spool.Write(encoded); err != nil {
			return err
		}
		_, err = spool.Write([]byte{'\n'})
		return err
	}
	report, err := auditFile(ctx, path, cfg, consume)
	if err != nil {
		return report, err
	}
	if err := spool.Sync(); err != nil {
		return report, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return report, err
	}
	if format == "text" {
		if err := writeAuditTextSpool(w, report, spool); err != nil {
			return report, err
		}
		return report, nil
	}
	if err := writeAuditJSONSpool(w, report, spool); err != nil {
		return report, err
	}
	return report, nil
}

func auditFile(ctx context.Context, path string, cfg *config.Config, consume func(AuditLocation) error) (AuditReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return AuditReport{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return AuditReport{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return AuditReport{}, err
	}
	files, err := zipguard.OpenWithLimits(ctx, f, info.Size(), maxProofEntries+6)
	if err != nil {
		return AuditReport{}, err
	}
	data, err := validateFilesContext(ctx, files)
	if err != nil {
		return AuditReport{}, err
	}
	defer func() { _ = data.index.Close() }()
	tmp, err := os.MkdirTemp("", ".mytracker-audit-*")
	if err != nil {
		return AuditReport{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	auditCfg := cfg.Clone()
	if cfg.TSATrustRoots != "" {
		roots, readErr := readFilePathContext(ctx, cfg.TSATrustRoots)
		stagedRoots := filepath.Join(tmp, "tsa-roots.pem")
		if readErr != nil {
			auditCfg.TSATrustRoots = filepath.Join(tmp, "unavailable-roots.pem")
		} else if err := os.WriteFile(stagedRoots, roots, 0600); err != nil {
			return AuditReport{}, err
		} else {
			auditCfg.TSATrustRoots = stagedRoots
		}
	}
	overall := "verified"
	if data.valid == 0 {
		overall = "inconclusive"
	}
	report := AuditReport{
		Version: 1, ArchiveVersion: data.manifest.Version,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339), Overall: overall,
		Policy: AuditPolicy{
			TSARootsConfigured: cfg.TSATrustRoots != "",
			TSAPolicyOIDs:      append([]string(nil), cfg.TSAPolicyOIDs...),
			OTSProviders:       len(cfg.EsploraURLs), OTSMinConfirmations: cfg.OTSConfirmations,
			OnlineRevocation: false,
		},
	}
	err = eachRecordContext(ctx, data, func(rec export.Record) error {
		digestBytes, _ := hex.DecodeString(rec.DigestSHA256)
		var digest [sha256.Size]byte
		copy(digest[:], digestBytes)
		nonce, _ := hex.DecodeString(rec.TSANonceHex)
		item := AuditLocation{ID: rec.ID, Digest: rec.DigestSHA256}
		var err error
		var tsaProof *zip.File
		tsaProof, err = data.file(ctx, rec.Proofs.TSR)
		if err != nil {
			return err
		}
		item.TSA, err = auditTSA(ctx, tsaProof, tmp, digest, nonce, auditCfg)
		if err != nil {
			return err
		}
		var otsProof *zip.File
		otsProof, err = data.file(ctx, rec.Proofs.OTS)
		if err != nil {
			return err
		}
		item.OTS, err = auditOTS(ctx, otsProof, tmp, digest, auditCfg)
		if err != nil {
			return err
		}
		item.Overall = auditOverall(item.TSA.Result, item.OTS.Result)
		report.Overall = mergeAuditResult(report.Overall, item.Overall)
		report.locationCount++
		if consume == nil {
			report.Locations = append(report.Locations, item)
		} else if err := consume(item); err != nil {
			return err
		}
		return nil
	})
	return report, err
}

func auditTSA(ctx context.Context, file *zip.File, tmp string, digest [sha256.Size]byte, nonce []byte, cfg *config.Config) (AuditProof, error) {
	if file == nil {
		return AuditProof{Result: "missing", ErrorCode: "proof_missing"}, nil
	}
	proof, err := stageProofContext(ctx, tmp, file)
	if err != nil {
		if ctx.Err() != nil {
			return AuditProof{}, ctx.Err()
		}
		return AuditProof{Result: "error", ErrorCode: "proof_read"}, nil
	}
	result, err := tsa.VerifyTokenPolicy(proof, digest, tsa.TrustPolicy{
		RootsFile: cfg.TSATrustRoots, PolicyOIDs: cfg.TSAPolicyOIDs, ExpectedNonce: nonce,
	})
	if err != nil {
		out := AuditProof{Result: "invalid", ErrorCode: "tsa_invalid"}
		switch {
		case errors.Is(err, tsa.ErrNonceMismatch):
			out.ErrorCode = "tsa_nonce_mismatch"
		case errors.Is(err, tsa.ErrPolicyMismatch):
			out.ErrorCode = "tsa_policy_mismatch"
		case errors.Is(err, tsa.ErrTrustUnavailable):
			out.Result, out.ErrorCode = "error", "tsa_trust_unavailable"
		}
		return out, nil
	}
	out := AuditProof{
		Result: "degraded", GenTime: result.GenTime.Unix(), PolicyOID: result.PolicyOID,
		Serial: result.Serial, SignerSubject: result.SignerSubject,
		SignerFingerprint:      result.SignerFingerprint,
		TrustAnchorFingerprint: result.TrustAnchorFingerprint,
		NonceStatus:            result.NonceStatus, PolicyStatus: result.PolicyStatus,
		TrustMode: result.TrustMode, AccuracyMS: result.Accuracy.Milliseconds(),
	}
	if result.Pinned && result.NonceStatus == "matched" && result.PolicyStatus == "matched" {
		out.Result = "verified"
	}
	return out, nil
}

func auditOTS(ctx context.Context, file *zip.File, tmp string, digest [sha256.Size]byte, cfg *config.Config) (AuditProof, error) {
	if file == nil {
		return AuditProof{Result: "missing", ErrorCode: "proof_missing"}, nil
	}
	proof, err := stageProofContext(ctx, tmp, file)
	if err != nil {
		if ctx.Err() != nil {
			return AuditProof{}, ctx.Err()
		}
		return AuditProof{Result: "error", ErrorCode: "proof_read"}, nil
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, verifyErr := ots.VerifyPool(verifyCtx, proof, digest, cfg.EsploraURLs, cfg.OTSConfirmations)
	if ctx.Err() != nil {
		return AuditProof{}, ctx.Err()
	}
	out := AuditProof{
		Result: result.Result, BitcoinHeight: result.Height,
		ErrorCode:        result.ErrorCode,
		BitcoinBlockHash: result.BlockHash, BitcoinPOWValid: result.POWValid,
		BitcoinConfirmations: result.Confirmations, AgreeingSources: result.Agreeing,
	}
	if verifyErr != nil {
		out.ErrorCode = verifyErr.Error()
	}
	for _, attempt := range result.Attempts {
		out.Attempts = append(out.Attempts, AuditAttempt{
			ProviderID: attempt.ProviderID, Result: attempt.Result,
			ErrorCode: attempt.ErrorCode, BlockHeight: attempt.Height,
			BlockHash: attempt.BlockHash, TipHeight: attempt.TipHeight,
			Confirmations: attempt.Confirmations, HeaderValid: attempt.HeaderValid,
			POWValid: attempt.POWValid, MerkleValid: attempt.MerkleValid,
			DurationMS: attempt.Duration.Milliseconds(),
		})
	}
	return out, nil
}

func stageProofContext(ctx context.Context, dir string, file *zip.File) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, filepath.Base(file.Name))
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	in, err := file.Open()
	if err != nil {
		_ = out.Close()
		return nil, err
	}
	counted := &countWriter{w: out}
	copyErr := copyContext(ctx, counted, io.LimitReader(in, maxProofBytes+1))
	_ = in.Close()
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || counted.n > maxProofBytes {
		_ = os.Remove(path)
		return nil, fmt.Errorf("proof staging failed")
	}
	return readFilePathContext(ctx, path)
}

type countWriter struct {
	w io.Writer
	n int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

func readFilePathContext(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var b bytes.Buffer
	counted := &countWriter{w: &b}
	if err := copyContext(ctx, counted, io.LimitReader(f, maxProofBytes+1)); err != nil {
		return nil, err
	}
	if counted.n > maxProofBytes {
		return nil, fmt.Errorf("proof too large")
	}
	return b.Bytes(), nil
}

func auditOverall(results ...string) string {
	if len(results) == 0 {
		return "inconclusive"
	}
	out := "verified"
	for _, result := range results {
		switch result {
		case "invalid", "error", "missing":
			return "invalid"
		case "degraded", "pending", "inconclusive":
			out = "degraded"
		}
	}
	return out
}

func mergeAuditResult(current, next string) string {
	if current == "invalid" || next == "invalid" {
		return "invalid"
	}
	if current == "degraded" || next == "degraded" {
		return "degraded"
	}
	if current == "inconclusive" || next == "inconclusive" {
		return "inconclusive"
	}
	return "verified"
}

func AuditExitCode(report AuditReport) int {
	switch report.Overall {
	case "verified":
		return 0
	case "degraded":
		return 2
	case "inconclusive":
		return 2
	default:
		return 1
	}
}

func WriteAuditText(w io.Writer, report AuditReport) error {
	if _, err := fmt.Fprintf(w, "archive v%d: %s\n", report.ArchiveVersion, report.Overall); err != nil {
		return err
	}
	for _, loc := range report.Locations {
		if _, err := fmt.Fprintf(w, "%d %s TSA=%s OTS=%s\n",
			loc.ID, loc.Digest, loc.TSA.Result, loc.OTS.Result); err != nil {
			return err
		}
	}
	return nil
}

type auditJSONHeader struct {
	Version        int         `json:"version"`
	ArchiveVersion int         `json:"archive_version"`
	GeneratedAt    string      `json:"generated_at"`
	Overall        string      `json:"overall"`
	Policy         AuditPolicy `json:"policy"`
}

func writeAuditTextSpool(w io.Writer, report AuditReport, spool io.Reader) error {
	if _, err := fmt.Fprintf(w, "archive v%d: %s\n", report.ArchiveVersion, report.Overall); err != nil {
		return err
	}
	_, err := io.Copy(w, spool)
	return err
}

func writeAuditJSONSpool(w io.Writer, report AuditReport, spool io.Reader) error {
	header, err := json.Marshal(auditJSONHeader{
		Version: report.Version, ArchiveVersion: report.ArchiveVersion,
		GeneratedAt: report.GeneratedAt, Overall: report.Overall, Policy: report.Policy,
	})
	if err != nil {
		return err
	}
	if len(header) == 0 || header[len(header)-1] != '}' {
		return fmt.Errorf("invalid audit JSON header")
	}
	header = header[:len(header)-1]
	if err := writeAuditBytes(w, header); err != nil {
		return err
	}
	if report.locationCount == 0 {
		return writeAuditBytes(w, []byte(",\"locations\":null}\n"))
	}
	if err := writeAuditBytes(w, []byte(",\"locations\":[")); err != nil {
		return err
	}
	reader := bufio.NewReader(spool)
	for i := 0; i < report.locationCount; i++ {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return err
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		if i > 0 {
			if err := writeAuditBytes(w, []byte{','}); err != nil {
				return err
			}
		}
		if err := writeAuditBytes(w, line); err != nil {
			return err
		}
	}
	return writeAuditBytes(w, []byte("]}\n"))
}

func writeAuditBytes(w io.Writer, data []byte) error {
	_, err := io.Copy(w, bytes.NewReader(data))
	return err
}
