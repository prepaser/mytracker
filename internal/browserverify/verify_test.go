package browserverify

import (
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/ots"
)

func TestOTSEvidenceOmitsUnavailableDefaults(t *testing.T) {
	got := otsEvidence(&ots.PoolResult{
		Result: "error",
		Attempts: []ots.Attempt{{
			ProviderID: "esplora-1", Result: "error", ErrorCode: "provider_unavailable",
		}},
	})
	for _, key := range []string{"height", "block_hash", "pow_valid", "confirmations"} {
		if _, ok := got[key]; ok {
			t.Fatalf("unavailable result has %s: %#v", key, got)
		}
	}
	attempts, ok := got["attempts"].([]map[string]any)
	if !ok || len(attempts) != 1 {
		t.Fatalf("attempts=%#v", got["attempts"])
	}
	for _, key := range []string{"height", "block_hash", "header_valid", "pow_valid", "confirmations"} {
		if _, ok := attempts[0][key]; ok {
			t.Fatalf("unavailable attempt has %s: %#v", key, attempts[0])
		}
	}
}

func TestOTSProofPreservesSafeResultErrorCode(t *testing.T) {
	got := otsProof(&ots.PoolResult{Result: "error", ErrorCode: "provider_unavailable"}, nil, true)
	if got.ErrorCode != "provider_unavailable" {
		t.Fatalf("error code=%q", got.ErrorCode)
	}
	got = otsProof(&ots.PoolResult{Result: "error", ErrorCode: "private-detail"}, nil, true)
	if got.ErrorCode != "" {
		t.Fatalf("unsafe error code=%q", got.ErrorCode)
	}
}

func TestOverallResultPrecedence(t *testing.T) {
	for _, test := range []struct {
		current, next, want string
	}{
		{"verified", "inconclusive", "inconclusive"},
		{"inconclusive", "missing", "missing"},
		{"missing", "error", "error"},
		{"error", "invalid", "invalid"},
		{"error", "missing", "error"},
	} {
		if got := mergeOverall(test.current, test.next); got != test.want {
			t.Fatalf("mergeOverall(%q, %q)=%q, want %q", test.current, test.next, got, test.want)
		}
	}
}

func TestValidateOptionsProviderPolicy(t *testing.T) {
	for _, raw := range []string{" http://127.0.0.1:3000/api", "http://example.com/api"} {
		if err := ValidateOptions(Options{MinConfirmations: 1, Providers: []string{raw}, Online: true}); err != ErrInvalidOptions {
			t.Fatalf("provider %q err=%v", raw, err)
		}
	}
	if err := ValidateOptions(Options{
		MinConfirmations: 1,
		Providers:        []string{"http://127.0.0.1:3000/api"},
		Online:           true,
	}); err != nil {
		t.Fatalf("loopback provider rejected: %v", err)
	}
}

func TestOTSEvidenceKeepsRealZeroHeightAndInvalidPOW(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	got := otsEvidence(&ots.PoolResult{
		Result: "invalid",
		Attempts: []ots.Attempt{{
			ProviderID: "esplora-1", Result: "invalid", ErrorCode: "header_pow_invalid",
			Height: 0, BlockHash: hash, HeaderValid: true,
		}},
	})
	if _, ok := got["pow_valid"]; ok {
		t.Fatalf("unanchored result has aggregate pow evidence: %#v", got["pow_valid"])
	}
	attempts := got["attempts"].([]map[string]any)
	if attempts[0]["height"] != int64(0) || attempts[0]["block_hash"] != hash || attempts[0]["header_valid"] != true || attempts[0]["pow_valid"] != false {
		t.Fatalf("invalid pow attempt=%#v", attempts[0])
	}
	for _, key := range []string{"confirmations", "merkle_valid"} {
		if _, ok := attempts[0][key]; ok {
			t.Fatalf("invalid pow attempt has %s: %#v", key, attempts[0])
		}
	}

	anchor := otsEvidence(&ots.PoolResult{
		Height: 0, BlockHash: hash, POWValid: true, Confirmations: 1,
		Attempts: []ots.Attempt{{Height: 0, BlockHash: hash, HeaderValid: true, POWValid: true}},
	})
	if anchor["height"] != int64(0) || anchor["block_hash"] != hash || anchor["pow_valid"] != true {
		t.Fatalf("anchor evidence=%#v", anchor)
	}
}
