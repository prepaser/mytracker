package ots

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync/atomic"
	"testing"

	libots "github.com/nbd-wtf/opentimestamps"
)

func TestStrongerAttemptPrefersTrustThenConfirmations(t *testing.T) {
	selected := strongerAttempt(nil, Attempt{Result: "invalid"})
	selected = strongerAttempt(selected, Attempt{Result: "pending", Confirmations: 2})
	selected = strongerAttempt(selected, Attempt{Result: "verified", Confirmations: 1})
	selected = strongerAttempt(selected, Attempt{Result: "verified", Confirmations: 8})
	selected = strongerAttempt(selected, Attempt{Result: "pending", Confirmations: 20})
	if selected.Result != "verified" || selected.Confirmations != 8 {
		t.Fatalf("selected=%+v", selected)
	}
}

func TestVerifyPoolRejectsOverflowHeightBeforeRequests(t *testing.T) {
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer provider.Close()
	for _, height := range []uint64{math.MaxInt64 + 1, math.MaxUint64} {
		seqs := []libots.Sequence{{{Attestation: &libots.Attestation{BitcoinBlockHeight: height}}}}
		proof := (libots.File{Digest: make([]byte, 32), Sequences: seqs}).SerializeToFile()
		result, err := VerifyPool(context.Background(), proof, [32]byte{}, []string{provider.URL})
		if err == nil || result.Result != "invalid" || requests.Load() != 0 {
			t.Fatalf("height=%d result=%+v err=%v requests=%d", height, result, err, requests.Load())
		}
		if !errors.Is(validateSequences(seqs), errProofMalformed) {
			t.Fatalf("accepted height=%d for serialization", height)
		}
	}
}

func TestBlockConfirmationsBounds(t *testing.T) {
	for _, tc := range []struct {
		height, tip, want int64
		ok                bool
	}{
		{0, 0, 1, true}, {1, 6, 6, true}, {6, 5, 0, false}, {-1, 10, 0, false},
		{0, math.MaxInt64, 0, false}, {1, math.MaxInt64, math.MaxInt64, true},
		{math.MaxInt64, math.MaxInt64, 1, true},
	} {
		got, ok := blockConfirmations(tc.height, tc.tip)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("height=%d tip=%d got=%d/%v want=%d/%v", tc.height, tc.tip, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPoolResultSameAnchorMerkleMismatchIsInconclusive(t *testing.T) {
	anchor := poolAnchor{height: 123, hash: "anchor"}
	result := poolResult([]Attempt{
		{Result: "verified", Height: anchor.height, BlockHash: anchor.hash, Confirmations: 10},
		{
			Result:      "invalid",
			ErrorCode:   "merkle_mismatch",
			Height:      anchor.height,
			BlockHash:   anchor.hash,
			HeaderValid: true,
			POWValid:    true,
		},
	})
	if result.Result != "inconclusive" || result.Height != anchor.height || result.BlockHash != anchor.hash {
		t.Fatalf("result=%+v", result)
	}
}

func TestPoolResultCrossAnchorMerkleMismatchDoesNotOverrideSelectedAnchor(t *testing.T) {
	good := poolAnchor{height: 123, hash: "good"}
	bad := poolAnchor{height: 124, hash: "bad"}
	result := poolResult([]Attempt{
		{Result: "verified", Height: good.height, BlockHash: good.hash, Confirmations: 10},
		{
			Result:      "invalid",
			ErrorCode:   "merkle_mismatch",
			Height:      bad.height,
			BlockHash:   bad.hash,
			HeaderValid: true,
			POWValid:    true,
		},
	})
	if result.Result != "degraded" || result.Height != good.height || result.BlockHash != good.hash {
		t.Fatalf("result=%+v", result)
	}
}

func TestPoolResultUsesCrossSequenceConsensus(t *testing.T) {
	old := poolAnchor{height: 100, hash: "old"}
	newAnchor := poolAnchor{height: 200, hash: "new"}
	result := poolResultEvidence([]providerEvidence{
		{
			selected: Attempt{ProviderID: "esplora-1", Result: "verified", Height: old.height, BlockHash: old.hash, Confirmations: 20},
			evidence: []Attempt{
				{ProviderID: "esplora-1", Result: "verified", Height: old.height, BlockHash: old.hash, Confirmations: 20},
				{ProviderID: "esplora-1", Result: "verified", Height: newAnchor.height, BlockHash: newAnchor.hash, Confirmations: 10},
			},
		},
		{
			selected: Attempt{ProviderID: "esplora-2", Result: "verified", Height: newAnchor.height, BlockHash: newAnchor.hash, Confirmations: 10},
			evidence: []Attempt{{ProviderID: "esplora-2", Result: "verified", Height: newAnchor.height, BlockHash: newAnchor.hash, Confirmations: 10}},
		},
		{
			selected: Attempt{ProviderID: "esplora-3", Result: "verified", Height: newAnchor.height, BlockHash: newAnchor.hash, Confirmations: 10},
			evidence: []Attempt{{ProviderID: "esplora-3", Result: "verified", Height: newAnchor.height, BlockHash: newAnchor.hash, Confirmations: 10}},
		},
	})
	if result.Result != "verified" || result.Height != newAnchor.height || result.BlockHash != newAnchor.hash || result.Agreeing != 3 || len(result.Attempts) != 3 {
		t.Fatalf("result=%+v", result)
	}
	for _, attempt := range result.Attempts {
		if attempt.BlockHash != newAnchor.hash {
			t.Fatalf("attempt=%+v does not describe selected anchor", attempt)
		}
	}
}

func TestPoolResultConflictingHashesAtHeightRemainInconclusive(t *testing.T) {
	result := poolResultEvidence([]providerEvidence{
		{
			selected: Attempt{ProviderID: "esplora-1", Result: "verified", Height: 100, BlockHash: "a", Confirmations: 10},
			evidence: []Attempt{{ProviderID: "esplora-1", Result: "verified", Height: 100, BlockHash: "a", Confirmations: 10}},
		},
		{
			selected: Attempt{ProviderID: "esplora-2", Result: "verified", Height: 100, BlockHash: "b", Confirmations: 10},
			evidence: []Attempt{{ProviderID: "esplora-2", Result: "verified", Height: 100, BlockHash: "b", Confirmations: 10}},
		},
	})
	if result.Result != "inconclusive" {
		t.Fatalf("result=%+v", result)
	}
}

func TestPoolResultSelectsDeterministicMerkleMismatchQuorum(t *testing.T) {
	result := poolResultEvidence([]providerEvidence{
		{
			selected: Attempt{ProviderID: "esplora-1", Result: "invalid", ErrorCode: "merkle_mismatch", Height: 200, BlockHash: "quorum", HeaderValid: true, POWValid: true},
			evidence: []Attempt{{ProviderID: "esplora-1", Result: "invalid", ErrorCode: "merkle_mismatch", Height: 200, BlockHash: "quorum", HeaderValid: true, POWValid: true}},
		},
		{
			selected: Attempt{ProviderID: "esplora-2", Result: "invalid", ErrorCode: "merkle_mismatch", Height: 200, BlockHash: "quorum", HeaderValid: true, POWValid: true},
			evidence: []Attempt{{ProviderID: "esplora-2", Result: "invalid", ErrorCode: "merkle_mismatch", Height: 200, BlockHash: "quorum", HeaderValid: true, POWValid: true}},
		},
		{
			selected: Attempt{ProviderID: "esplora-3", Result: "invalid", ErrorCode: "merkle_mismatch", Height: 100, BlockHash: "singleton", HeaderValid: true, POWValid: true},
			evidence: []Attempt{{ProviderID: "esplora-3", Result: "invalid", ErrorCode: "merkle_mismatch", Height: 100, BlockHash: "singleton", HeaderValid: true, POWValid: true}},
		},
	})
	if result.Result != "invalid" || result.Height != 200 || result.BlockHash != "quorum" {
		t.Fatalf("result=%+v", result)
	}
	if len(result.Attempts) != 3 || result.Attempts[0].BlockHash != "quorum" || result.Attempts[1].BlockHash != "quorum" {
		t.Fatalf("attempts=%+v", result.Attempts)
	}
}

func TestPoolResultIgnoresSameProviderAlternativeMerkleMismatch(t *testing.T) {
	anchor := poolAnchor{height: 200, hash: "anchor"}
	result := poolResultEvidence([]providerEvidence{
		{
			selected: Attempt{ProviderID: "esplora-1", Result: "verified", Height: anchor.height, BlockHash: anchor.hash, Confirmations: 10},
			evidence: []Attempt{
				{ProviderID: "esplora-1", Result: "verified", Height: anchor.height, BlockHash: anchor.hash, Confirmations: 10},
				{ProviderID: "esplora-1", Result: "invalid", ErrorCode: "merkle_mismatch", Height: anchor.height, BlockHash: anchor.hash, HeaderValid: true, POWValid: true},
			},
		},
		{
			selected: Attempt{ProviderID: "esplora-2", Result: "verified", Height: anchor.height, BlockHash: anchor.hash, Confirmations: 10},
			evidence: []Attempt{{ProviderID: "esplora-2", Result: "verified", Height: anchor.height, BlockHash: anchor.hash, Confirmations: 10}},
		},
	})
	if result.Result != "verified" || result.Agreeing != 2 || len(result.Attempts) != 2 {
		t.Fatalf("result=%+v", result)
	}
}

func TestReadFileRejectsCheckpointExpansion(t *testing.T) {
	proof := append([]byte{}, proofPrefix...)
	for range 10_000 {
		proof = append(proof, 0x08)
	}
	for range 110 {
		proof = append(proof, 0xff)
	}
	if _, err := readFile(proof); !errors.Is(err, errProofComplexity) {
		t.Fatalf("err=%v", err)
	}
}

func TestReadFileRejectsAppendWork(t *testing.T) {
	proof := append([]byte{}, proofPrefix...)
	for range 12_000 {
		proof = append(proof, 0xf0, 1, 0)
	}
	proof = append(proof, (libots.File{Sequences: []libots.Sequence{{{
		Attestation: &libots.Attestation{CalendarServerURL: "https://calendar.example"},
	}}}}).SerializeInstructionSequences()...)
	if _, err := readFile(proof); !errors.Is(err, errProofComplexity) {
		t.Fatalf("err=%v", err)
	}
}

func TestReadFileRejectsUnterminatedProof(t *testing.T) {
	for name, proof := range map[string][]byte{
		"empty":      proofPrefix,
		"checkpoint": append(append([]byte{}, proofPrefix...), 0xff),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readFile(proof); !errors.Is(err, errProofMalformed) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestReadFileAcceptsAppendCalendarProof(t *testing.T) {
	proof := (libots.File{Digest: make([]byte, 32), Sequences: []libots.Sequence{{
		{Operation: &libots.Operation{Name: "append", Tag: 0xf0, Binary: true}, Argument: []byte{1}},
		{Attestation: &libots.Attestation{CalendarServerURL: "https://calendar.example"}},
	}}}).SerializeToFile()
	if _, err := readFile(proof); err != nil {
		t.Fatal(err)
	}
}

func TestReadFileRejectsBitcoinAttestationTrailingBytes(t *testing.T) {
	proof := (libots.File{Digest: make([]byte, 32), Sequences: []libots.Sequence{{{
		Attestation: &libots.Attestation{BitcoinBlockHeight: 1},
	}}}}).SerializeToFile()
	if _, err := readFile(proof); err != nil {
		t.Fatal(err)
	}

	trailing := append([]byte(nil), proof...)
	bodyLength := len(proofPrefix) + 1 + len(bitcoinAttestationMagic)
	trailing[bodyLength]++
	trailing = append(trailing, 0)
	if _, err := readFile(trailing); !errors.Is(err, errProofMalformed) {
		t.Fatalf("err=%v", err)
	}
}

func TestReadFileRejectsUnsupportedOperations(t *testing.T) {
	digest := [32]byte{1}
	proof := (libots.File{
		Digest: digest[:],
		Sequences: []libots.Sequence{{
			{Operation: &libots.Operation{Name: "reverse", Tag: 0xf2}},
			{Attestation: &libots.Attestation{BitcoinBlockHeight: 1}},
		}},
	}).SerializeToFile()
	if _, err := readFile(proof); !errors.Is(err, errProofUnsupported) {
		t.Fatalf("err=%v", err)
	}
}

func TestReadFileRejectsIncompleteBranchesAndTrailingOperations(t *testing.T) {
	attestation := (libots.File{Sequences: []libots.Sequence{{{
		Attestation: &libots.Attestation{CalendarServerURL: "https://calendar.example"},
	}}}}).SerializeInstructionSequences()
	for _, checkpoint := range []bool{false, true} {
		proof := append([]byte(nil), proofPrefix...)
		if checkpoint {
			proof = append(proof, 0xff)
		}
		proof = append(proof, attestation...)
		proof = append(proof, 0x08)
		if _, err := readFile(proof); !errors.Is(err, errProofMalformed) {
			t.Fatalf("checkpoint=%t err=%v", checkpoint, err)
		}
	}
}

func TestProofStatusDoesNotCompareSequenceArguments(t *testing.T) {
	argument := bytes.Repeat([]byte{1}, 64<<10)
	op := &libots.Operation{Name: "append", Tag: 0xf0, Binary: true}
	file := &libots.File{Digest: make([]byte, 32)}
	for i := range 512 {
		file.Sequences = append(file.Sequences,
			libots.Sequence{
				{Operation: op, Argument: argument},
				{Attestation: &libots.Attestation{CalendarServerURL: "https://calendar.example"}},
			},
			libots.Sequence{
				{Operation: op, Argument: argument},
				{Attestation: &libots.Attestation{BitcoinBlockHeight: uint64(i + 1)}},
			},
		)
	}
	pending, bitcoin := proofStatus(file)
	if !pending || !bitcoin {
		t.Fatalf("pending=%t bitcoin=%t", pending, bitcoin)
	}
}

func TestReadFileRejectsSequenceComparisonWork(t *testing.T) {
	argument := bytes.Repeat([]byte{1}, 1<<20)
	op := &libots.Operation{Name: "append", Tag: 0xf0, Binary: true}
	file := libots.File{Digest: make([]byte, 32)}
	for i := range 10 {
		file.Sequences = append(file.Sequences, libots.Sequence{
			{Operation: op, Argument: argument},
			{Attestation: &libots.Attestation{BitcoinBlockHeight: uint64(i + 1)}},
		})
	}
	if _, err := readFile(file.SerializeToFile()); !errors.Is(err, errProofComplexity) {
		t.Fatalf("err=%v", err)
	}
}

func TestReadFileRejectsTooManySequences(t *testing.T) {
	file := libots.File{Digest: make([]byte, 32)}
	for i := range maxProofSequences + 1 {
		file.Sequences = append(file.Sequences, libots.Sequence{{
			Attestation: &libots.Attestation{BitcoinBlockHeight: uint64(i + 1)},
		}})
	}
	if _, err := readFile(file.SerializeToFile()); !errors.Is(err, errProofComplexity) {
		t.Fatalf("err=%v", err)
	}
}

func TestCalendarParserRejectsUnsupportedOperations(t *testing.T) {
	body := (libots.File{Sequences: []libots.Sequence{{
		{Operation: &libots.Operation{Name: "reverse", Tag: 0xf2}},
		{Attestation: &libots.Attestation{CalendarServerURL: "https://calendar.example"}},
	}}}).SerializeInstructionSequences()
	if _, err := parseCalendarResponse(body); !errors.Is(err, errProofUnsupported) {
		t.Fatalf("err=%v", err)
	}
}

func TestStampRejectsCombinedProofCost(t *testing.T) {
	op := &libots.Operation{Name: "append", Tag: 0xf0, Binary: true}
	sequence := make(libots.Sequence, 0, 10001)
	for range 10000 {
		sequence = append(sequence, libots.Instruction{Operation: op, Argument: []byte{1}})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := append(libots.Sequence(nil), sequence...)
		response = append(response, libots.Instruction{Attestation: &libots.Attestation{
			CalendarServerURL: "https://calendar.example" + r.URL.Path,
		}})
		_, _ = w.Write((libots.File{Sequences: []libots.Sequence{response}}).SerializeInstructionSequences())
	}))
	defer server.Close()
	proof, err := New([]string{server.URL + "/a", server.URL + "/b"}).Stamp(t.Context(), [32]byte{1})
	if !errors.Is(err, errProofComplexity) || proof != nil {
		t.Fatalf("proof_bytes=%d err=%v", len(proof), err)
	}
}

func TestUpgradeRejectsComposedProofCost(t *testing.T) {
	op := &libots.Operation{Name: "append", Tag: 0xf0, Binary: true}
	sequence := make(libots.Sequence, 0, 10001)
	for range 10000 {
		sequence = append(sequence, libots.Instruction{Operation: op, Argument: []byte{1}})
	}
	sequence = append(sequence, libots.Instruction{Attestation: &libots.Attestation{BitcoinBlockHeight: 1}})
	response := (libots.File{Sequences: []libots.Sequence{sequence}}).SerializeInstructionSequences()
	if _, err := parseCalendarResponse(response); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(response)
	}))
	defer server.Close()
	digest := [32]byte{1}
	proof := (libots.File{Digest: digest[:], Sequences: []libots.Sequence{{
		{Operation: op, Argument: bytes.Repeat([]byte{1}, 4096)},
		{Attestation: &libots.Attestation{CalendarServerURL: server.URL}},
	}}}).SerializeToFile()
	if _, err := readFile(proof); err != nil {
		t.Fatal(err)
	}
	upgraded, changed, err := Upgrade(t.Context(), proof, digest, []string{server.URL})
	if !errors.Is(err, errProofComplexity) || changed || upgraded != nil {
		t.Fatalf("proof_bytes=%d changed=%t err=%v", len(upgraded), changed, err)
	}
	if _, err := readFile(proof); err != nil {
		t.Fatalf("original proof changed: %v", err)
	}
}

func TestUpgradeRejectsDigestMismatch(t *testing.T) {
	digest := [32]byte{1}
	other := [32]byte{2}
	proof := (libots.File{
		Digest: digest[:],
		Sequences: []libots.Sequence{{{
			Attestation: &libots.Attestation{BitcoinBlockHeight: 1},
		}}},
	}).SerializeToFile()
	upgraded, changed, err := Upgrade(t.Context(), proof, other, nil)
	if err == nil || err.Error() != "digest_mismatch" || changed || upgraded != nil {
		t.Fatalf("proof=%x changed=%t err=%v", upgraded, changed, err)
	}
}

func TestLoadProofRejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/7.ots"
	if err := os.WriteFile(path, make([]byte, maxProofSize+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProof(dir, 7); err != errProofTooLarge {
		t.Fatalf("err=%v", err)
	}
}

func TestStampRejectsOversizedCalendarResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, maxCalendarResponseSize+1))
	}))
	defer server.Close()

	_, err := New([]string{server.URL}).Stamp(context.Background(), [32]byte{})
	if err == nil {
		t.Fatal("expected oversized response error")
	}
}

func TestUpgradeRejectsOversizedCalendarResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, maxCalendarResponseSize+1))
	}))
	defer server.Close()

	digest := [32]byte{1}
	proof := (libots.File{
		Digest: digest[:],
		Sequences: []libots.Sequence{{{
			Attestation: &libots.Attestation{CalendarServerURL: server.URL},
		}}},
	}).SerializeToFile()
	_, _, err := Upgrade(context.Background(), proof, digest, []string{server.URL})
	if !errors.Is(err, errCalendarResponseTooLarge) {
		t.Fatalf("err=%v", err)
	}
}

func TestUpgradeRejectsUnconfiguredCalendar(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	digest := [32]byte{2}
	proof := (libots.File{
		Digest: digest[:],
		Sequences: []libots.Sequence{{{
			Attestation: &libots.Attestation{CalendarServerURL: server.URL},
		}}},
	}).SerializeToFile()
	upgraded, changed, err := Upgrade(context.Background(), proof, digest, []string{"https://calendar.example"})
	if err != nil || changed || !bytes.Equal(upgraded, proof) {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
	if requests.Load() != 0 {
		t.Fatal("unconfigured calendar received a request")
	}
}

func TestUpgradeRejectsCalendarRedirect(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	digest := [32]byte{3}
	proof := (libots.File{
		Digest: digest[:],
		Sequences: []libots.Sequence{{{
			Attestation: &libots.Attestation{CalendarServerURL: source.URL},
		}}},
	}).SerializeToFile()
	_, changed, err := Upgrade(context.Background(), proof, digest, []string{source.URL})
	if err != nil || changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
	if targetRequests.Load() != 0 {
		t.Fatal("calendar redirect target received a request")
	}
}

func TestUpgradeTreatsCalendarNotFoundAsPending(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	digest := [32]byte{4}
	proof := (libots.File{
		Digest: digest[:],
		Sequences: []libots.Sequence{{{
			Attestation: &libots.Attestation{CalendarServerURL: server.URL},
		}}},
	}).SerializeToFile()
	upgraded, changed, err := Upgrade(t.Context(), proof, digest, []string{server.URL})
	if err != nil || changed || !bytes.Equal(upgraded, proof) {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
}

func TestUpgradeReportsCalendarUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	digest := [32]byte{5}
	proof := (libots.File{
		Digest: digest[:],
		Sequences: []libots.Sequence{{{
			Attestation: &libots.Attestation{CalendarServerURL: server.URL},
		}}},
	}).SerializeToFile()
	upgraded, changed, err := Upgrade(t.Context(), proof, digest, []string{server.URL})
	if err == nil || changed || upgraded != nil || !errors.Is(err, errCalendarUnavailable) {
		t.Fatalf("proof=%x changed=%t err=%v", upgraded, changed, err)
	}
}

func TestVerifyProviderDoesNotFollowRedirects(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	seqs := []libots.Sequence{{{Attestation: &libots.Attestation{BitcoinBlockHeight: 1}}}}
	attempt := verifyProvider(context.Background(), seqs, [32]byte{1}, source.URL, 6)
	if attempt.Result != "error" || attempt.ErrorCode != "provider_unavailable" || targetRequests.Load() != 0 {
		t.Fatalf("attempt=%+v target_requests=%d", attempt, targetRequests.Load())
	}
}

func TestNormalizeCalendarURLAcceptsHTTPAndRejectsCredentials(t *testing.T) {
	tests := []struct {
		raw string
		ok  bool
	}{
		{"https://CALENDAR.example/path/", true},
		{"http://127.0.0.1:8080/path", true},
		{"http://calendar.example/path", true},
		{"https://user:pass@calendar.example/path", false},
	}
	for _, tt := range tests {
		got, ok := normalizeCalendarURL(tt.raw)
		if ok != tt.ok {
			t.Fatalf("normalizeCalendarURL(%q) ok=%t want %t (%q)", tt.raw, ok, tt.ok, got)
		}
	}
	if got, _ := normalizeCalendarURL("https://CALENDAR.example/path/"); got != "https://calendar.example/path" {
		t.Fatalf("normalized URL=%q", got)
	}
}

func TestNormalizeCalendarURLCanonicalizesAuthorityAliases(t *testing.T) {
	for raw, want := range map[string]string{
		"https://calendar.example.:443/path/":            "https://calendar.example/path",
		"http://calendar.example.:00080/path/":           "http://calendar.example/path",
		"https://calendar.example:0008443/path":          "https://calendar.example:8443/path",
		"https://[2001:0db8:0:0:0:0:0:1]:0443/path":      "https://[2001:db8::1]/path",
		"https://calendar.example/calendar/%2F?token=a/": "https://calendar.example/calendar/%2F?token=a/",
	} {
		got, ok := normalizeCalendarURL(raw)
		if !ok || got != want {
			t.Fatalf("normalizeCalendarURL(%q)=%q/%t want %q/true", raw, got, ok, want)
		}
	}
}

func TestUniqueCalendarURLsDeduplicatesAliases(t *testing.T) {
	got := uniqueCalendarURLs([]string{
		"https://CALENDAR.example.:443/path/",
		"https://calendar.example/path",
		"https://other.example/path",
	})
	want := []string{"https://calendar.example/path", "https://other.example/path"}
	if !slices.Equal(got, want) {
		t.Fatalf("unique calendars=%v want %v", got, want)
	}
}

func TestCalendarEndpointPreservesQueryAndEscapedPath(t *testing.T) {
	got := calendarEndpoint("https://calendar.example/calendar/%2F?token=a/", "/digest")
	want := "https://calendar.example/calendar/%2F/digest?token=a/"
	if got != want {
		t.Fatalf("calendar endpoint=%q want %q", got, want)
	}
}
