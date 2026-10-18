package verification

func ValidPersistedErrorCode(code string) bool {
	switch code {
	case "", "payload_digest_mismatch", "proof_missing", "proof_read", "proof_parse",
		"digest_mismatch", "no_providers", "tsa_invalid", "tsa_nonce_mismatch",
		"tsa_policy_mismatch", "tsa_trust_unavailable", "header_hash_mismatch",
		"header_pow_invalid", "tip_before_anchor", "merkle_mismatch",
		"confirmations_pending", "provider_timeout", "provider_unavailable":
		return true
	default:
		return false
	}
}
