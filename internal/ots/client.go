package ots

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/nbd-wtf/opentimestamps"
	"github.com/prepaser/mytracker/internal/atomicfile"
	"github.com/prepaser/mytracker/internal/providerurl"
)

var (
	errProofTooLarge            = errors.New("proof too large")
	errProofMalformed           = errors.New("malformed proof")
	errProofComplexity          = errors.New("proof complexity limit")
	errProofUnsupported         = errors.New("unsupported proof operation")
	errCalendarResponseTooLarge = errors.New("calendar response too large")
	errCalendarNotFound         = errors.New("calendar proof pending")
	errCalendarUnavailable      = errors.New("calendar unavailable")
	errProviderResponseTooLarge = errors.New("provider response too large")
)

const (
	maxProofSize            = 8 << 20
	maxCalendarResponseSize = 8 << 20
	maxProviderResponseSize = 1 << 20
	maxProofInstructions    = 1 << 16
	maxProofExpansion       = 1 << 18
	maxProofWork            = 64 << 20
	maxProofMessage         = 64 << 20
	maxProofComparison      = 64 << 20
	maxProofSequences       = 64
	maxProofCheckpoints     = 4096
)

var proofPrefix = (&opentimestamps.File{Digest: make([]byte, 32)}).SerializeToFile()

var bitcoinAttestationMagic = []byte{0x05, 0x88, 0x96, 0x0d, 0x73, 0xd7, 0x19, 0x01}

type Client struct {
	Calendars []string
}

func New(calendars []string) *Client {
	return &Client{Calendars: calendars}
}

func (c *Client) Stamp(ctx context.Context, digest [32]byte) ([]byte, error) {
	var seqs []opentimestamps.Sequence
	for _, calendar := range uniqueCalendarURLs(c.Calendars) {
		seq, err := stampCalendar(ctx, calendar, digest)
		if err != nil {
			continue
		}
		seqs = append(seqs, seq)
		if err := validateSequences(seqs); err != nil {
			return nil, err
		}
	}
	if len(seqs) == 0 {
		return nil, fmt.Errorf("all ots calendars failed")
	}
	file := opentimestamps.File{Digest: digest[:], Sequences: seqs}
	return serializeProof(file)
}

func SaveProof(dir string, id int64, data []byte) error {
	if len(data) > maxProofSize {
		return errProofTooLarge
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return atomicfile.WriteFunc(filepath.Join(dir, fmt.Sprintf("%d.ots", id)), 0600, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

func LoadProof(dir string, id int64) ([]byte, error) {
	f, err := os.Open(filepath.Join(dir, fmt.Sprintf("%d.ots", id)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readBounded(f, maxProofSize, errProofTooLarge)
}

func Upgrade(ctx context.Context, proof []byte, digest [32]byte, calendars []string) (out []byte, changed bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			out, changed, err = nil, false, errProofMalformed
		}
	}()
	file, err := readFile(proof)
	if err != nil {
		return nil, false, err
	}
	if !bytes.Equal(file.Digest, digest[:]) {
		return nil, false, fmt.Errorf("digest_mismatch")
	}
	allowed := normalizedCalendars(calendars)
	upgraded := false
	var transientErr error
	for i, seq := range file.Sequences {
		att := seq.GetAttestation()
		if att.BitcoinBlockHeight > 0 {
			continue
		}
		if att.CalendarServerURL == "" {
			continue
		}
		calendar, ok := normalizeCalendarURL(att.CalendarServerURL)
		if !ok || !allowed[calendar] {
			continue
		}
		upSeq, err := upgradeCalendar(ctx, seq, digest[:], calendar)
		if err != nil {
			if errors.Is(err, errCalendarResponseTooLarge) {
				return nil, false, err
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, false, err
			}
			if errors.Is(err, errCalendarUnavailable) {
				if transientErr == nil {
					transientErr = err
				}
			}
			continue
		}
		file.Sequences[i] = upSeq
		if err := validateSequences(file.Sequences); err != nil {
			return nil, false, err
		}
		upgraded = true
	}
	pending, bitcoin := proofStatus(file)
	confirmed := !pending && bitcoin
	if !upgraded {
		if transientErr != nil {
			return nil, false, transientErr
		}
		return proof, confirmed, nil
	}
	out, err = serializeProof(*file)
	if err != nil {
		return nil, false, err
	}
	return out, upgraded || confirmed, nil
}

func serializeProof(file opentimestamps.File) (proof []byte, err error) {
	if err := validateSequences(file.Sequences); err != nil {
		return nil, err
	}
	defer func() {
		if recover() != nil {
			proof, err = nil, errProofMalformed
		}
	}()
	proof = file.SerializeToFile()
	if _, err := readFile(proof); err != nil {
		return nil, err
	}
	return proof, nil
}

func validateSequences(sequences []opentimestamps.Sequence) error {
	if len(sequences) == 0 {
		return errProofMalformed
	}
	if len(sequences) > maxProofSequences {
		return errProofComplexity
	}
	totalInstructions, totalWork, totalComparison := 0, 0, 0
	for _, seq := range sequences {
		if len(seq) == 0 {
			return errProofMalformed
		}
		totalInstructions += len(seq)
		if totalInstructions > maxProofInstructions+maxProofExpansion {
			return errProofComplexity
		}
		state := proofState{messageLen: 32, active: true}
		for i, instruction := range seq {
			if i == len(seq)-1 {
				if instruction.Attestation == nil || instruction.Operation != nil {
					return errProofMalformed
				}
				attestation := instruction.Attestation
				if attestation.BitcoinBlockHeight > math.MaxInt64 {
					return errProofMalformed
				}
				var encoded [binary.MaxVarintLen64]byte
				if attestation.BitcoinBlockHeight != 0 {
					state.comparison += 9 + binary.PutUvarint(encoded[:], attestation.BitcoinBlockHeight)
				} else if attestation.CalendarServerURL != "" {
					length := len(attestation.CalendarServerURL)
					state.comparison += 9 + binary.PutUvarint(encoded[:], uint64(length)) + length
				} else {
					return errProofMalformed
				}
				if err := finishProofPath(&state, &totalWork, &totalComparison); err != nil {
					return err
				}
				continue
			}
			if instruction.Operation == nil || instruction.Attestation != nil {
				return errProofMalformed
			}
			switch instruction.Tag {
			case 0x03, 0x08, 0xf0, 0xf1:
				if err := applyProofOperation(&state, instruction.Tag, len(instruction.Argument)); err != nil {
					return err
				}
			default:
				return errProofUnsupported
			}
		}
	}
	if totalComparison > maxProofComparison/len(sequences) {
		return errProofComplexity
	}
	return nil
}

type Attempt struct {
	ProviderID    string
	Result        string
	Height        int64
	BlockHash     string
	Duration      time.Duration
	ErrorCode     string
	TipHeight     int64
	Confirmations int64
	HeaderValid   bool
	POWValid      bool
	MerkleValid   bool
}

type PoolResult struct {
	ErrorCode     string
	Result        string
	Height        int64
	BlockHash     string
	Agreeing      int
	Confirmations int64
	POWValid      bool
	Attempts      []Attempt
}

func VerifyPool(ctx context.Context, proof []byte, digest [32]byte, urls []string, minConfirmations ...int) (*PoolResult, error) {
	file, err := readFile(proof)
	if err != nil {
		return &PoolResult{Result: "invalid"}, fmt.Errorf("proof_parse")
	}
	if !bytes.Equal(file.Digest, digest[:]) {
		return &PoolResult{Result: "invalid"}, fmt.Errorf("digest_mismatch")
	}
	seqs := file.GetBitcoinAttestedSequences()
	if len(seqs) == 0 {
		return &PoolResult{Result: "pending"}, nil
	}
	if len(urls) == 0 {
		return &PoolResult{Result: "error"}, fmt.Errorf("no_providers")
	}
	providers := uniqueProviderURLs(urls)
	if len(providers) == 0 {
		return &PoolResult{Result: "error"}, fmt.Errorf("no_providers")
	}
	required := 6
	if len(minConfirmations) > 0 && minConfirmations[0] > 0 {
		required = minConfirmations[0]
	}
	evidence := make([]providerEvidence, len(providers))
	var wg sync.WaitGroup
	for i, rawURL := range providers {
		wg.Add(1)
		go func(i int, rawURL string) {
			defer wg.Done()
			start := time.Now()
			result := safeVerifyProviderEvidence(ctx, seqs, digest, rawURL, required)
			providerID := fmt.Sprintf("esplora-%d", i+1)
			result.selected.ProviderID = providerID
			result.selected.Duration = time.Since(start)
			for j := range result.evidence {
				result.evidence[j].ProviderID = providerID
			}
			evidence[i] = result
		}(i, rawURL)
	}
	wg.Wait()
	return poolResultEvidence(evidence), nil
}

type poolAnchor struct {
	height int64
	hash   string
}

type providerEvidence struct {
	selected Attempt
	evidence []Attempt
}

func poolResult(attempts []Attempt) *PoolResult {
	providers := make([]providerEvidence, len(attempts))
	for i, attempt := range attempts {
		if attempt.ProviderID == "" {
			attempt.ProviderID = fmt.Sprintf("esplora-%d", i+1)
		}
		providers[i] = providerEvidence{selected: attempt, evidence: []Attempt{attempt}}
	}
	return poolResultEvidence(providers)
}

func poolResultEvidence(providers []providerEvidence) *PoolResult {
	valid := make(map[poolAnchor]map[string]Attempt)
	mismatch := make(map[poolAnchor]map[string]Attempt)
	for i, provider := range providers {
		providerID := evidenceProviderID(provider, i)
		for _, attempt := range provider.evidence {
			if attempt.Result == "verified" || attempt.Result == "pending" {
				a := poolAnchor{attempt.Height, attempt.BlockHash}
				if current, ok := valid[a][providerID]; !ok || attemptStronger(attempt, current) {
					if valid[a] == nil {
						valid[a] = make(map[string]Attempt)
					}
					valid[a][providerID] = attempt
				}
			}
			if attempt.ErrorCode == "merkle_mismatch" && attempt.HeaderValid && attempt.POWValid {
				a := poolAnchor{attempt.Height, attempt.BlockHash}
				if current, ok := mismatch[a][providerID]; !ok || attemptStronger(attempt, current) {
					if mismatch[a] == nil {
						mismatch[a] = make(map[string]Attempt)
					}
					mismatch[a][providerID] = attempt
				}
			}
		}
	}
	for anchor, evidence := range mismatch {
		for providerID := range evidence {
			if _, ok := valid[anchor][providerID]; ok {
				delete(evidence, providerID)
			}
		}
		if len(evidence) == 0 {
			delete(mismatch, anchor)
		}
	}

	out := &PoolResult{Attempts: providerAttempts(providers)}
	if hasConflictingHashes(valid) {
		out.Result = "inconclusive"
		return out
	}
	if anchor, evidence, ok := strongestAnchor(valid); ok {
		out.Attempts = providerAttemptsForAnchor(providers, anchor, false)
		out.Height, out.BlockHash = anchor.height, anchor.hash
		out.Agreeing = len(evidence)
		out.POWValid = true
		for _, attempt := range evidence {
			if attempt.Confirmations > out.Confirmations {
				out.Confirmations = attempt.Confirmations
			}
		}
		if len(mismatch[anchor]) > 0 {
			out.Result = "inconclusive"
			return out
		}
		mature := 0
		for _, attempt := range evidence {
			if attempt.Result == "verified" {
				mature++
			}
		}
		if mature >= 2 {
			out.Result = "verified"
		} else if mature == 1 {
			out.Result = "degraded"
		} else {
			out.Result = "pending"
		}
		return out
	}
	if anchor, evidence, ok := strongestMismatch(mismatch); ok {
		out.Attempts = providerAttemptsForAnchor(providers, anchor, true)
		out.Height, out.BlockHash = anchor.height, anchor.hash
		if len(evidence) >= 2 {
			out.Result = "invalid"
			return out
		}
		out.Result = "inconclusive"
		return out
	}
	out.Result = "error"
	out.ErrorCode = "provider_unavailable"
	if len(out.Attempts) > 0 && out.Attempts[0].ErrorCode != "" {
		code := out.Attempts[0].ErrorCode
		if !slices.ContainsFunc(out.Attempts, func(a Attempt) bool { return a.ErrorCode != code }) {
			out.ErrorCode = code
		}
	}
	return out
}

func evidenceProviderID(provider providerEvidence, index int) string {
	if provider.selected.ProviderID != "" {
		return provider.selected.ProviderID
	}
	for _, attempt := range provider.evidence {
		if attempt.ProviderID != "" {
			return attempt.ProviderID
		}
	}
	return fmt.Sprintf("esplora-%d", index+1)
}

func providerAttempts(providers []providerEvidence) []Attempt {
	attempts := make([]Attempt, len(providers))
	for i, provider := range providers {
		attempts[i] = provider.selected
		if attempts[i].ProviderID == "" {
			attempts[i].ProviderID = evidenceProviderID(provider, i)
		}
	}
	return attempts
}

func providerAttemptsForAnchor(providers []providerEvidence, anchor poolAnchor, mismatch bool) []Attempt {
	attempts := providerAttempts(providers)
	for i, provider := range providers {
		var selected *Attempt
		for _, candidate := range provider.evidence {
			valid := candidate.Result == "verified" || candidate.Result == "pending"
			if mismatch {
				valid = candidate.ErrorCode == "merkle_mismatch" && candidate.HeaderValid && candidate.POWValid
			}
			if !valid || candidate.Height != anchor.height || candidate.BlockHash != anchor.hash {
				continue
			}
			selected = strongerAttempt(selected, candidate)
		}
		if selected == nil {
			continue
		}
		selected.ProviderID = attempts[i].ProviderID
		selected.Duration = attempts[i].Duration
		attempts[i] = *selected
	}
	return attempts
}

func strongestMismatch(mismatch map[poolAnchor]map[string]Attempt) (poolAnchor, map[string]Attempt, bool) {
	var selected poolAnchor
	var selectedEvidence map[string]Attempt
	for anchor, evidence := range mismatch {
		if selectedEvidence == nil || len(evidence) > len(selectedEvidence) ||
			(len(evidence) == len(selectedEvidence) && mismatchAnchorBefore(anchor, selected)) {
			selected, selectedEvidence = anchor, evidence
		}
	}
	return selected, selectedEvidence, selectedEvidence != nil
}

func mismatchAnchorBefore(a, b poolAnchor) bool {
	if a.height != b.height {
		return a.height > b.height
	}
	return a.hash < b.hash
}

func hasConflictingHashes(valid map[poolAnchor]map[string]Attempt) bool {
	hashes := make(map[int64]map[string]struct{})
	for anchor := range valid {
		if hashes[anchor.height] == nil {
			hashes[anchor.height] = make(map[string]struct{})
		}
		hashes[anchor.height][anchor.hash] = struct{}{}
		if len(hashes[anchor.height]) > 1 {
			return true
		}
	}
	return false
}

func strongestAnchor(valid map[poolAnchor]map[string]Attempt) (poolAnchor, map[string]Attempt, bool) {
	var selected poolAnchor
	var selectedEvidence map[string]Attempt
	selectedRank := -1
	for anchor, evidence := range valid {
		rank := anchorRank(evidence)
		if selectedEvidence == nil || rank > selectedRank ||
			(rank == selectedRank && strongerAnchor(anchor, evidence, selected, selectedEvidence)) {
			selected, selectedEvidence, selectedRank = anchor, evidence, rank
		}
	}
	return selected, selectedEvidence, selectedEvidence != nil
}

func anchorRank(evidence map[string]Attempt) int {
	mature := 0
	maxConfirmations := int64(0)
	for _, attempt := range evidence {
		if attempt.Result == "verified" {
			mature++
		}
		if attempt.Confirmations > maxConfirmations {
			maxConfirmations = attempt.Confirmations
		}
	}
	if mature >= 2 {
		return 4
	}
	if mature == 1 {
		return 3
	}
	if maxConfirmations > 0 {
		return 2
	}
	return 1
}

func strongerAnchor(a poolAnchor, ae map[string]Attempt, b poolAnchor, be map[string]Attempt) bool {
	if len(ae) != len(be) {
		return len(ae) > len(be)
	}
	aConfirmations, bConfirmations := int64(0), int64(0)
	for _, attempt := range ae {
		if attempt.Confirmations > aConfirmations {
			aConfirmations = attempt.Confirmations
		}
	}
	for _, attempt := range be {
		if attempt.Confirmations > bConfirmations {
			bConfirmations = attempt.Confirmations
		}
	}
	if aConfirmations != bConfirmations {
		return aConfirmations > bConfirmations
	}
	return a.height > b.height
}

func attemptStronger(candidate, current Attempt) bool {
	return attemptRank(candidate) > attemptRank(current) ||
		(attemptRank(candidate) == attemptRank(current) && candidate.Confirmations > current.Confirmations)
}

func strongestEvidence(evidence []Attempt) *Attempt {
	var selected *Attempt
	for _, candidate := range evidence {
		if candidate.Result != "verified" && candidate.Result != "pending" {
			continue
		}
		selected = strongerAttempt(selected, candidate)
	}
	return selected
}

func verifyProvider(parent context.Context, seqs []opentimestamps.Sequence, digest [32]byte, rawURL string, minConfirmations int) Attempt {
	return safeVerifyProviderEvidence(parent, seqs, digest, rawURL, minConfirmations).selected
}

func verifyProviderEvidence(parent context.Context, seqs []opentimestamps.Sequence, digest [32]byte, rawURL string, minConfirmations int) providerEvidence {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	client := &esploraClient{
		ctx: ctx, baseURL: strings.TrimRight(rawURL, "/"),
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	var evidence []Attempt
	var mismatch, invalid *Attempt
	for _, seq := range seqs {
		rawHeight := seq.GetAttestation().BitcoinBlockHeight
		if rawHeight > math.MaxInt64 {
			return providerEvidence{selected: Attempt{Result: "invalid", ErrorCode: "proof_parse"}}
		}
		height := int64(rawHeight)
		hash, err := client.GetBlockHash(height)
		if err != nil {
			continue
		}
		header, err := client.GetBlockHeader(hash)
		if err != nil {
			continue
		}
		attempt := Attempt{Height: height, BlockHash: hash.String()}
		actualHash := header.BlockHash()
		if !actualHash.IsEqual(hash) {
			attempt.Result, attempt.ErrorCode = "invalid", "header_hash_mismatch"
			evidence = append(evidence, attempt)
			invalid = strongerAttempt(invalid, attempt)
			continue
		}
		attempt.HeaderValid = true
		if !validProofOfWork(header, &actualHash) {
			attempt.Result, attempt.ErrorCode = "invalid", "header_pow_invalid"
			evidence = append(evidence, attempt)
			invalid = strongerAttempt(invalid, attempt)
			continue
		}
		attempt.POWValid = true
		tip, err := client.GetTipHeight()
		if err != nil {
			continue
		}
		attempt.TipHeight = tip
		if tip < height {
			attempt.Result, attempt.ErrorCode = "invalid", "tip_before_anchor"
			evidence = append(evidence, attempt)
			invalid = strongerAttempt(invalid, attempt)
			continue
		}
		confirmations, ok := blockConfirmations(height, tip)
		if !ok {
			continue
		}
		attempt.Confirmations = confirmations
		root, _ := seq.Compute(digest[:])
		if !bytes.Equal(root, header.MerkleRoot[:]) {
			attempt.Result, attempt.ErrorCode = "invalid", "merkle_mismatch"
			evidence = append(evidence, attempt)
			mismatch = strongerAttempt(mismatch, attempt)
			continue
		}
		attempt.MerkleValid = true
		if attempt.Confirmations < int64(minConfirmations) {
			attempt.Result, attempt.ErrorCode = "pending", "confirmations_pending"
		} else {
			attempt.Result = "verified"
		}
		evidence = append(evidence, attempt)
	}
	selected := strongestEvidence(evidence)
	if selected != nil {
		return providerEvidence{selected: *selected, evidence: evidence}
	}
	if mismatch != nil {
		return providerEvidence{selected: *mismatch, evidence: evidence}
	}
	if invalid != nil {
		return providerEvidence{selected: *invalid, evidence: evidence}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return providerEvidence{selected: Attempt{Result: "error", ErrorCode: "provider_timeout"}}
	}
	return providerEvidence{selected: Attempt{Result: "error", ErrorCode: "provider_unavailable"}}
}

func uniqueProviderURLs(urls []string) []string {
	providers := make([]string, 0, len(urls))
	seen := make(map[string]struct{}, len(urls))
	for _, raw := range urls {
		canonical, ok := normalizeProviderURL(raw)
		if !ok {
			continue
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		providers = append(providers, canonical)
	}
	return providers
}

func safeVerifyProviderEvidence(parent context.Context, seqs []opentimestamps.Sequence, digest [32]byte, rawURL string, minConfirmations int) (result providerEvidence) {
	defer func() {
		if recover() != nil {
			result = providerEvidence{selected: Attempt{Result: "error", ErrorCode: "proof_parse"}}
		}
	}()
	return verifyProviderEvidence(parent, seqs, digest, rawURL, minConfirmations)
}

func strongerAttempt(current *Attempt, candidate Attempt) *Attempt {
	if current == nil || attemptRank(candidate) > attemptRank(*current) ||
		attemptRank(candidate) == attemptRank(*current) && candidate.Confirmations > current.Confirmations {
		copy := candidate
		return &copy
	}
	return current
}

func blockConfirmations(height, tip int64) (int64, bool) {
	if height < 0 || tip < height || tip-height == math.MaxInt64 {
		return 0, false
	}
	return tip - height + 1, true
}

func attemptRank(attempt Attempt) int {
	switch attempt.Result {
	case "verified":
		return 3
	case "pending":
		return 2
	default:
		return 1
	}
}

func validProofOfWork(header *wire.BlockHeader, hash *chainhash.Hash) bool {
	target := blockchain.CompactToBig(header.Bits)
	return target.Sign() > 0 &&
		target.Cmp(chaincfg.MainNetParams.PowLimit) <= 0 &&
		blockchain.HashToBig(hash).Cmp(target) <= 0
}

func stampCalendar(ctx context.Context, calendar string, digest [32]byte) (opentimestamps.Sequence, error) {
	body, err := calendarRequest(ctx, http.MethodPost, calendarEndpoint(calendar, "/digest"), bytes.NewReader(digest[:]))
	if err != nil {
		return nil, err
	}
	return parseCalendarResponse(body)
}

func upgradeCalendar(ctx context.Context, seq opentimestamps.Sequence, initial []byte, calendar string) (opentimestamps.Sequence, error) {
	result, _ := seq.Compute(initial)
	endpoint := calendarEndpoint(calendar, fmt.Sprintf("/timestamp/%x", result))
	body, err := calendarRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	tail, err := parseCalendarResponse(body)
	if err != nil {
		return nil, err
	}
	if len(seq) == 0 {
		return nil, fmt.Errorf("empty calendar sequence")
	}
	upgraded := make(opentimestamps.Sequence, len(seq)+len(tail)-1)
	copy(upgraded, seq[:len(seq)-1])
	copy(upgraded[len(seq)-1:], tail)
	return upgraded, nil
}

func calendarRequest(ctx context.Context, method, endpoint string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "github.com/fiatjaf/opentimestamps")
	req.Header.Set("Accept", "application/vnd.opentimestamps.v1")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := calendarHTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", errCalendarUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errCalendarNotFound
	}
	if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode >= http.StatusInternalServerError {
		return nil, fmt.Errorf("%w: calendar status %d", errCalendarUnavailable, resp.StatusCode)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("calendar status %d", resp.StatusCode)
	}
	if resp.ContentLength > maxCalendarResponseSize {
		return nil, errCalendarResponseTooLarge
	}
	data, err := readBounded(resp.Body, maxCalendarResponseSize, errCalendarResponseTooLarge)
	if err != nil && !errors.Is(err, errCalendarResponseTooLarge) {
		return nil, fmt.Errorf("%w: %v", errCalendarUnavailable, err)
	}
	return data, err
}

var calendarHTTPClient = &http.Client{
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func normalizedCalendars(calendars []string) map[string]bool {
	urls := uniqueCalendarURLs(calendars)
	allowed := make(map[string]bool, len(urls))
	for _, normalized := range urls {
		allowed[normalized] = true
	}
	return allowed
}

func uniqueCalendarURLs(calendars []string) []string {
	urls := make([]string, 0, len(calendars))
	seen := make(map[string]struct{}, len(calendars))
	for _, raw := range calendars {
		normalized, ok := normalizeCalendarURL(raw)
		if !ok {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		urls = append(urls, normalized)
	}
	return urls
}

func normalizeCalendarURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.Scheme == "" {
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else if !strings.Contains(host, ":") {
		host = strings.TrimSuffix(host, ".")
		if host == "" {
			return "", false
		}
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", false
		}
		if (u.Scheme == "http" && n == 80) || (u.Scheme == "https" && n == 443) {
			port = ""
		} else {
			port = strconv.Itoa(n)
		}
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	u.Fragment = ""
	escapedPath := strings.TrimRight(u.EscapedPath(), "/")
	if escapedPath == "" {
		u.Path, u.RawPath = "", ""
	} else {
		u.Path, err = url.PathUnescape(escapedPath)
		if err != nil {
			return "", false
		}
		u.RawPath = escapedPath
	}
	return u.String(), true
}

func normalizeProviderURL(raw string) (string, bool) {
	canonical, err := providerurl.NormalizeLoopbackHTTP(raw)
	return canonical, err == nil
}

func calendarEndpoint(base, suffix string) string {
	u, err := url.Parse(base)
	if err != nil {
		return strings.TrimRight(base, "/") + suffix
	}
	escapedPath := strings.TrimRight(u.EscapedPath(), "/") + suffix
	u.Path, err = url.PathUnescape(escapedPath)
	if err != nil {
		return strings.TrimRight(base, "/") + suffix
	}
	if escapedPath == u.Path {
		u.RawPath = ""
	} else {
		u.RawPath = escapedPath
	}
	return u.String()
}

func parseCalendarResponse(body []byte) (opentimestamps.Sequence, error) {
	proof := make([]byte, 0, len(proofPrefix)+len(body))
	proof = append(proof, proofPrefix...)
	proof = append(proof, body...)
	file, err := parseFile(proof)
	if err != nil {
		return nil, err
	}
	if len(file.Sequences) != 1 {
		return nil, fmt.Errorf("invalid calendar sequence count: %d", len(file.Sequences))
	}
	seq := file.Sequences[0]
	if len(seq) == 0 || seq[len(seq)-1].Attestation == nil {
		return nil, fmt.Errorf("calendar sequence has no attestation")
	}
	return seq, nil
}

func readBounded(r io.Reader, limit int64, tooLarge error) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, tooLarge
	}
	return data, nil
}

type esploraClient struct {
	ctx     context.Context
	baseURL string
	client  *http.Client
}

func (e *esploraClient) GetBlockHash(height int64) (*chainhash.Hash, error) {
	body, err := e.get("/block-height/" + strconv.FormatInt(height, 10))
	if err != nil {
		return nil, err
	}
	return chainhash.NewHashFromStr(strings.TrimSpace(string(body)))
}

func (e *esploraClient) GetBlockHeader(hash *chainhash.Hash) (*wire.BlockHeader, error) {
	body, err := e.get("/block/" + hash.String() + "/header")
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(body)))
	if err != nil {
		return nil, err
	}
	if len(raw) != 80 {
		return nil, fmt.Errorf("invalid header length")
	}
	header := &wire.BlockHeader{}
	if err := header.BtcDecode(bytes.NewReader(raw), 0, 0); err != nil {
		return nil, err
	}
	return header, nil
}

func (e *esploraClient) GetTipHeight() (int64, error) {
	body, err := e.get("/blocks/tip/height")
	if err != nil {
		return 0, err
	}
	height, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
	if err != nil || height < 0 {
		return 0, fmt.Errorf("invalid tip height")
	}
	return height, nil
}

func (e *esploraClient) get(path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(e.ctx, http.MethodGet, e.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider status")
	}
	return readBounded(resp.Body, maxProviderResponseSize, errProviderResponseTooLarge)
}

func Pending(proof []byte) bool {
	file, err := readFile(proof)
	if err != nil {
		return true
	}
	pending, _ := proofStatus(file)
	return pending
}

func HasBitcoinAnchor(proof []byte) bool {
	file, err := readFile(proof)
	if err != nil {
		return false
	}
	_, bitcoin := proofStatus(file)
	return bitcoin
}

func proofStatus(file *opentimestamps.File) (pending, bitcoin bool) {
	for _, seq := range file.Sequences {
		attestation := seq.GetAttestation()
		pending = pending || attestation.CalendarServerURL != ""
		bitcoin = bitcoin || attestation.BitcoinBlockHeight > 0
	}
	return pending, bitcoin
}

func readFile(proof []byte) (f *opentimestamps.File, err error) {
	if len(proof) > maxProofSize {
		return nil, errProofTooLarge
	}
	return parseFile(proof)
}

func parseFile(proof []byte) (f *opentimestamps.File, err error) {
	if err := validateProof(proof); err != nil {
		return nil, err
	}
	defer func() {
		if recover() != nil {
			err = errProofMalformed
		}
	}()
	f, err = opentimestamps.ReadFromFile(proof)
	if err != nil {
		return nil, err
	}
	for _, seq := range f.Sequences {
		if seq.GetAttestation().BitcoinBlockHeight > math.MaxInt64 {
			return nil, errProofMalformed
		}
	}
	return f, nil
}

func validateProof(proof []byte) error {
	headerLen := len(proofPrefix) - 32
	if len(proof) < len(proofPrefix) || !bytes.Equal(proof[:headerLen], proofPrefix[:headerLen]) {
		return errProofMalformed
	}
	pos := len(proofPrefix)
	current, regular, expanded, branches, attestations := 0, 0, 0, 1, 0
	state := proofState{messageLen: 32, active: true}
	totalWork, totalComparison := 0, 0
	checkpoints := make([]proofCheckpoint, 0, 8)
	for pos < len(proof) {
		if !state.active {
			return errProofMalformed
		}
		tag := proof[pos]
		pos++
		switch tag {
		case 0xff:
			if len(checkpoints) >= maxProofCheckpoints {
				return errProofComplexity
			}
			expanded += current
			if expanded > maxProofExpansion {
				return errProofComplexity
			}
			checkpoints = append(checkpoints, proofCheckpoint{instructions: current, state: state})
		case 0x00:
			if len(proof)-pos < 8 {
				return errProofMalformed
			}
			magic := proof[pos : pos+8]
			pos += 8
			attestation, err := proofVarBytes(proof, &pos)
			if err != nil {
				return err
			}
			if bytes.Equal(magic, bitcoinAttestationMagic) {
				attestationPos := 0
				if _, err := proofVarUint(attestation, &attestationPos); err != nil {
					return err
				}
				if attestationPos != len(attestation) {
					return errProofMalformed
				}
			}
			attestations++
			if attestations > maxProofSequences {
				return errProofComplexity
			}
			state.comparison += 9 + len(attestation)
			if err := finishProofPath(&state, &totalWork, &totalComparison); err != nil {
				return err
			}
			current++
			regular++
			if regular > maxProofInstructions {
				return errProofComplexity
			}
			if len(checkpoints) > 0 {
				checkpoint := checkpoints[len(checkpoints)-1]
				checkpoints = checkpoints[:len(checkpoints)-1]
				current, state = checkpoint.instructions, checkpoint.state
				branches++
				if branches > maxProofCheckpoints {
					return errProofComplexity
				}
			}
		case 0xf0, 0xf1:
			argument, err := proofVarBytes(proof, &pos)
			if err != nil {
				return err
			}
			if err := applyProofOperation(&state, tag, len(argument)); err != nil {
				return err
			}
			current++
			regular++
			if regular > maxProofInstructions {
				return errProofComplexity
			}
		case 0x03, 0x08:
			if err := applyProofOperation(&state, tag, 0); err != nil {
				return err
			}
			current++
			regular++
			if regular > maxProofInstructions {
				return errProofComplexity
			}
		case 0x02, 0x67, 0xf2, 0xf3:
			return errProofUnsupported
		default:
			return errProofMalformed
		}
	}
	if state.active || attestations == 0 || len(checkpoints) != 0 {
		return errProofMalformed
	}
	if totalComparison > maxProofComparison/attestations {
		return errProofComplexity
	}
	return nil
}

type proofState struct {
	messageLen int
	work       int
	comparison int
	active     bool
}

type proofCheckpoint struct {
	instructions int
	state        proofState
}

func finishProofPath(state *proofState, totalWork, totalComparison *int) error {
	if !state.active {
		return nil
	}
	if state.work > maxProofWork-*totalWork {
		return errProofComplexity
	}
	*totalWork += state.work
	if state.comparison > maxProofComparison-*totalComparison {
		return errProofComplexity
	}
	*totalComparison += state.comparison
	state.active = false
	return nil
}

func applyProofOperation(state *proofState, tag byte, argumentLength int) error {
	if !state.active {
		return nil
	}
	switch tag {
	case 0xf0, 0xf1:
		if argumentLength > maxProofMessage-state.messageLen {
			return errProofComplexity
		}
		state.messageLen += argumentLength
		state.work += state.messageLen
		state.comparison += 1 + argumentLength
	case 0x03:
		state.work += state.messageLen
		state.messageLen = 20
		state.comparison++
	case 0x08:
		state.work += state.messageLen
		state.messageLen = 32
		state.comparison++
	}
	if state.work > maxProofWork {
		return errProofComplexity
	}
	return nil
}

func proofVarBytes(data []byte, pos *int) ([]byte, error) {
	length, err := proofVarUint(data, pos)
	if err != nil {
		return nil, err
	}
	if length > uint64(len(data)-*pos) {
		return nil, errProofMalformed
	}
	start := *pos
	*pos += int(length)
	return data[start:*pos], nil
}

func proofVarUint(data []byte, pos *int) (uint64, error) {
	var value uint64
	for i := range uint(10) {
		if *pos >= len(data) {
			return 0, errProofMalformed
		}
		b := data[*pos]
		*pos++
		if i == 9 && b > 1 {
			return 0, errProofMalformed
		}
		value |= uint64(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return value, nil
		}
	}
	return 0, errProofMalformed
}

const DefaultEsplora = "https://blockstream.info/api"
