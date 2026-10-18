package tsa

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"time"

	"github.com/digitorus/pkcs7"
	"github.com/digitorus/timestamp"
	"github.com/prepaser/mytracker/internal/atomicfile"
)

type Client struct {
	URL    string
	client *http.Client
}

const maxResponseBytes = 1 << 20

func New(url string) *Client {
	return &Client{
		URL: url,
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

type Result struct {
	GenTime time.Time
	Token   []byte
	Nonce   []byte
}

type Verification struct {
	GenTime                time.Time
	PolicyOID              string
	Serial                 string
	SignerSubject          string
	SignerFingerprint      string
	TrustAnchorFingerprint string
	Accuracy               time.Duration
	NonceStatus            string
	PolicyStatus           string
	TrustMode              string
	Pinned                 bool
}

type TrustPolicy struct {
	RootsFile     string
	RootsPEM      []byte
	PolicyOIDs    []string
	ExpectedNonce []byte
}

var (
	ErrNonceMismatch    = errors.New("tsa nonce mismatch")
	ErrPolicyMismatch   = errors.New("tsa policy mismatch")
	ErrTrustUnavailable = errors.New("tsa trust unavailable")
)

func (c *Client) Stamp(ctx context.Context, digest [32]byte) (*Result, error) {
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return nil, err
	}
	nonce := new(big.Int).SetBytes(nonceBytes)
	req := &timestamp.Request{
		HashAlgorithm: crypto.SHA256,
		HashedMessage: digest[:],
		Certificates:  true,
		Nonce:         nonce,
	}
	reqBytes, err := req.Marshal()
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(reqBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/timestamp-query")
	httpReq.Header.Set("Accept", "application/timestamp-reply")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("tsa http status %d", resp.StatusCode)
	}
	if resp.ContentLength > maxResponseBytes {
		return nil, fmt.Errorf("tsa response too large")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("tsa response too large")
	}
	tsResp, err := parseResponse(body)
	if err != nil {
		return nil, err
	}
	if err := checkDigest(tsResp, digest[:]); err != nil {
		return nil, err
	}
	if tsResp.Nonce == nil || tsResp.Nonce.Cmp(nonce) != 0 {
		return nil, ErrNonceMismatch
	}
	return &Result{GenTime: tsResp.Time, Token: body, Nonce: nonce.Bytes()}, nil
}

func SaveToken(path string, token []byte) error {
	return atomicfile.Write(path, token, 0600)
}

func LoadToken(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func VerifyToken(token []byte, digest [32]byte, certPin string) (time.Time, error) {
	result, err := VerifyTokenDetailed(token, digest, certPin)
	if err != nil {
		return time.Time{}, err
	}
	return result.GenTime, nil
}

func VerifyTokenDetailed(token []byte, digest [32]byte, certPin string) (*Verification, error) {
	return VerifyTokenPolicy(token, digest, TrustPolicy{RootsFile: certPin})
}

func VerifyTokenPolicy(token []byte, digest [32]byte, policy TrustPolicy) (*Verification, error) {
	tsResp, err := parseResponse(token)
	if err != nil {
		return nil, err
	}
	if err := checkDigest(tsResp, digest[:]); err != nil {
		return nil, err
	}
	nonceStatus := "legacy"
	if len(policy.ExpectedNonce) > 0 {
		expected := new(big.Int).SetBytes(policy.ExpectedNonce)
		if tsResp.Nonce == nil || tsResp.Nonce.Cmp(expected) != 0 {
			return nil, ErrNonceMismatch
		}
		nonceStatus = "matched"
	}
	policyStatus := "unconfigured"
	if len(policy.PolicyOIDs) > 0 {
		policyStatus = "mismatch"
		for _, allowed := range policy.PolicyOIDs {
			if tsResp.Policy.String() == allowed {
				policyStatus = "matched"
				break
			}
		}
		if policyStatus != "matched" {
			return nil, ErrPolicyMismatch
		}
	}
	signer, anchor, err := verifyChainAt(tsResp.RawToken, policy.RootsFile, policy.RootsPEM, tsResp.Time)
	if err != nil {
		return nil, err
	}
	fingerprint := sha256.Sum256(signer.Raw)
	anchorFingerprint := sha256.Sum256(anchor.Raw)
	serial := ""
	if tsResp.SerialNumber != nil {
		serial = tsResp.SerialNumber.String()
	}
	return &Verification{
		GenTime:                tsResp.Time,
		PolicyOID:              tsResp.Policy.String(),
		Serial:                 serial,
		SignerSubject:          signer.Subject.String(),
		SignerFingerprint:      hex.EncodeToString(fingerprint[:]),
		TrustAnchorFingerprint: hex.EncodeToString(anchorFingerprint[:]),
		Accuracy:               tsResp.Accuracy,
		NonceStatus:            nonceStatus,
		PolicyStatus:           policyStatus,
		TrustMode:              map[bool]string{true: "configured", false: "system"}[policy.RootsFile != "" || len(policy.RootsPEM) > 0],
		Pinned:                 policy.RootsFile != "" || len(policy.RootsPEM) > 0,
	}, nil
}

func verifyChainAt(raw []byte, rootsFile string, rootsPEM []byte, at time.Time) (*x509.Certificate, *x509.Certificate, error) {
	if len(raw) == 0 {
		return nil, nil, fmt.Errorf("tsa: empty token")
	}
	p7, err := pkcs7.Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("tsa pkcs7: %w", err)
	}
	if len(p7.Certificates) == 0 {
		return nil, nil, fmt.Errorf("tsa: no embedded certificates")
	}
	signer := p7.GetOnlySigner()
	if signer == nil {
		return nil, nil, fmt.Errorf("tsa: expected exactly one signer")
	}
	if !validTimestampingSigner(signer) {
		return nil, nil, fmt.Errorf("tsa: signer lacks critical timestamping EKU")
	}
	var roots *x509.CertPool
	if rootsFile != "" && rootsPEM != nil {
		return nil, nil, fmt.Errorf("%w: multiple roots sources", ErrTrustUnavailable)
	}
	if rootsFile != "" || rootsPEM != nil {
		pem := rootsPEM
		if rootsFile != "" {
			var err error
			pem, err = os.ReadFile(rootsFile)
			if err != nil {
				return nil, nil, fmt.Errorf("%w: roots file", ErrTrustUnavailable)
			}
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, nil, fmt.Errorf("%w: invalid roots PEM", ErrTrustUnavailable)
		}
	} else {
		roots, err = x509.SystemCertPool()
		if err != nil || roots == nil {
			return nil, nil, fmt.Errorf("%w: system roots", ErrTrustUnavailable)
		}
	}
	intermediates := x509.NewCertPool()
	for _, cert := range p7.Certificates {
		if !cert.Equal(signer) {
			intermediates.AddCert(cert)
		}
	}
	opts := x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   at,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
	}
	if err := p7.VerifyWithOpts(opts); err != nil {
		if rootsFile != "" {
			return nil, nil, fmt.Errorf("tsa trust roots: %w", err)
		}
		return nil, nil, fmt.Errorf("tsa cert chain: %w", err)
	}
	chains, err := signer.Verify(opts)
	if err != nil || len(chains) == 0 || len(chains[0]) == 0 {
		return nil, nil, fmt.Errorf("tsa certificate chain: %w", err)
	}
	return signer, chains[0][len(chains[0])-1], nil
}

func checkDigest(ts *timestamp.Timestamp, digest []byte) error {
	if ts.HashAlgorithm != crypto.SHA256 || len(ts.HashedMessage) != sha256.Size {
		return fmt.Errorf("digest mismatch: expected SHA-256 imprint")
	}
	if subtle.ConstantTimeCompare(ts.HashedMessage, digest) != 1 {
		return fmt.Errorf("digest mismatch")
	}
	return nil
}

type timestampResponse struct {
	Status         timestampResponseStatus
	TimeStampToken asn1.RawValue `asn1:"optional"`
}

type timestampResponseStatus struct {
	Status       int
	StatusString []string       `asn1:"optional,utf8"`
	FailureInfo  asn1.BitString `asn1:"optional"`
}

func parseResponse(data []byte) (*timestamp.Timestamp, error) {
	var response timestampResponse
	rest, err := asn1.Unmarshal(data, &response)
	if err != nil {
		return nil, err
	}
	if len(rest) > 0 {
		return nil, timestamp.ParseError("trailing data in Time-Stamp response")
	}
	if response.Status.Status != int(timestamp.Granted) && response.Status.Status != int(timestamp.GrantedWithMods) {
		return nil, fmt.Errorf("tsa: invalid response status %d", response.Status.Status)
	}
	if len(response.TimeStampToken.Bytes) == 0 {
		return nil, timestamp.ParseError("no pkcs7 data in Time-Stamp response")
	}
	return timestamp.Parse(response.TimeStampToken.FullBytes)
}

func validTimestampingSigner(cert *x509.Certificate) bool {
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageTimeStamping ||
		len(cert.UnknownExtKeyUsage) != 0 {
		return false
	}
	var found bool
	for _, ext := range cert.Extensions {
		if !ext.Id.Equal([]int{2, 5, 29, 37}) {
			continue
		}
		if found || !ext.Critical {
			return false
		}
		found = true
	}
	return found
}
