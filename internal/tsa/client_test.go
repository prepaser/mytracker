package tsa_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitorus/pkcs7"
	"github.com/digitorus/timestamp"
	"github.com/prepaser/mytracker/internal/tsa"
)

func TestVerifyTokenRejectsInvalidToken(t *testing.T) {
	digest := sha256.Sum256([]byte("x"))
	if _, err := tsa.VerifyToken([]byte("not-a-tsr"), digest, ""); err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestStampRejectsHTTPAndMalformedResponses(t *testing.T) {
	digest := sha256.Sum256([]byte("x"))
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusBadGateway, "upstream failed"},
		{http.StatusOK, "not a timestamp"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := tsa.New(srv.URL).Stamp(context.Background(), digest)
		srv.Close()
		if err == nil {
			t.Fatalf("status %d accepted", tc.status)
		}
	}
}

func TestStampDoesNotFollowRedirects(t *testing.T) {
	digest := sha256.Sum256([]byte("x"))
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		var targetCalls atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			targetCalls.Add(1)
		}))
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, code)
		}))
		_, err := tsa.New(source.URL).Stamp(t.Context(), digest)
		source.Close()
		target.Close()
		if err == nil || !strings.Contains(err.Error(), "tsa http status") || targetCalls.Load() != 0 {
			t.Fatalf("status=%d target_calls=%d err=%v", code, targetCalls.Load(), err)
		}
	}
}

func TestStampBoundsAndRedactsResponseBody(t *testing.T) {
	digest := sha256.Sum256([]byte("x"))
	t.Run("oversized", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.CopyN(w, zeroReader{}, (1<<20)+1)
		}))
		defer srv.Close()
		_, err := tsa.New(srv.URL).Stamp(t.Context(), digest)
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("oversized content length", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1048577")
			_, _ = w.Write([]byte("x"))
		}))
		defer srv.Close()
		_, err := tsa.New(srv.URL).Stamp(t.Context(), digest)
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("error body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("untrusted response body"))
		}))
		defer srv.Close()
		_, err := tsa.New(srv.URL).Stamp(t.Context(), digest)
		if err == nil || strings.Contains(err.Error(), "untrusted") {
			t.Fatalf("err=%v", err)
		}
	})
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestSaveAndLoadToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.tsr")
	if err := tsa.SaveToken(path, []byte("token")); err != nil {
		t.Fatal(err)
	}
	got, err := tsa.LoadToken(path)
	if err != nil || string(got) != "token" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestVerifyTokenRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("network: FreeTSA")
	}
	digest := sha256.Sum256([]byte("mytracker-test"))
	c := tsa.New("https://freetsa.org/tsr")
	res, err := c.Stamp(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "1.tsr")
	if err := tsa.SaveToken(path, res.Token); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := timestamp.ParseResponse(data)
	if err != nil {
		t.Fatal(err)
	}
	p7, err := pkcs7.Parse(parsed.RawToken)
	if err != nil {
		t.Fatal(err)
	}
	var certs []byte
	for _, cert := range p7.Certificates {
		certs = append(certs, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	}
	pin := filepath.Join(dir, "tsa.pem")
	if err := os.WriteFile(pin, certs, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := tsa.VerifyToken(data, digest, pin)
	if err != nil {
		t.Fatal(err)
	}
	if got.IsZero() {
		t.Fatal("zero timestamp")
	}
	detailed, err := tsa.VerifyTokenDetailed(data, digest, pin)
	if err != nil {
		t.Fatal(err)
	}
	if !detailed.Pinned || detailed.PolicyOID == "" || detailed.Serial == "" ||
		detailed.SignerSubject == "" || len(detailed.SignerFingerprint) != 64 {
		t.Fatalf("details=%+v", detailed)
	}
}

func TestVerifyTokenDigestMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("network: FreeTSA")
	}
	digest := sha256.Sum256([]byte("a"))
	c := tsa.New("https://freetsa.org/tsr")
	res, err := c.Stamp(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	other := sha256.Sum256([]byte("b"))
	if _, err := tsa.VerifyToken(res.Token, other, ""); err == nil {
		t.Fatal("expected digest mismatch")
	}
}

func TestVerifyTokenAtGenerationTimeAndEKU(t *testing.T) {
	digest := sha256.Sum256([]byte("offline timestamp"))
	genTime := time.Now().UTC().Truncate(time.Second)
	token, root := localTimestamp(t, digest, genTime, x509.ExtKeyUsageTimeStamping)
	wrongEKU, wrongRoot := localTimestamp(t, digest, genTime, x509.ExtKeyUsageServerAuth)
	pin := filepath.Join(t.TempDir(), "root.pem")
	if err := os.WriteFile(pin, root, 0600); err != nil {
		t.Fatal(err)
	}
	wrongPin := filepath.Join(t.TempDir(), "wrong-root.pem")
	if err := os.WriteFile(wrongPin, wrongRoot, 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(genTime.Add(2100 * time.Millisecond)))
	result, err := tsa.VerifyTokenDetailed(token, digest, pin)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Pinned || !result.GenTime.Equal(genTime) || result.PolicyOID != "1.2.3.4" ||
		result.Serial == "" || result.SignerSubject == "" || len(result.SignerFingerprint) != 64 {
		t.Fatalf("result=%+v", result)
	}
	if _, err := tsa.VerifyTokenDetailed(token, digest, ""); err == nil {
		t.Fatal("private root trusted by system pool")
	}

	if _, err := tsa.VerifyTokenDetailed(wrongEKU, digest, wrongPin); err == nil {
		t.Fatal("non-timestamping certificate accepted")
	}
}

func TestVerifyTokenStrictPolicyAndNonce(t *testing.T) {
	digest := sha256.Sum256([]byte("strict timestamp"))
	genTime := time.Now().UTC().Truncate(time.Second)
	nonce := []byte{1, 2, 3, 4}
	token, root := localTimestampWithNonce(t, digest, genTime, x509.ExtKeyUsageTimeStamping, nonce)
	roots := filepath.Join(t.TempDir(), "root.pem")
	if err := os.WriteFile(roots, root, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := tsa.VerifyTokenPolicy(token, digest, tsa.TrustPolicy{
		RootsFile: roots, PolicyOIDs: []string{"1.2.3.4"}, ExpectedNonce: nonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.NonceStatus != "matched" || result.PolicyStatus != "matched" ||
		result.TrustMode != "configured" || len(result.TrustAnchorFingerprint) != 64 {
		t.Fatalf("result=%+v", result)
	}
	if _, err := tsa.VerifyTokenPolicy(token, digest, tsa.TrustPolicy{
		RootsFile: roots, PolicyOIDs: []string{"1.2.3.4"}, ExpectedNonce: []byte{9},
	}); !errors.Is(err, tsa.ErrNonceMismatch) {
		t.Fatalf("nonce error=%v", err)
	}
	if _, err := tsa.VerifyTokenPolicy(token, digest, tsa.TrustPolicy{
		RootsFile: roots, PolicyOIDs: []string{"1.2.3.5"}, ExpectedNonce: nonce,
	}); !errors.Is(err, tsa.ErrPolicyMismatch) {
		t.Fatalf("policy error=%v", err)
	}
	legacy, err := tsa.VerifyTokenPolicy(token, digest, tsa.TrustPolicy{RootsFile: roots})
	if err != nil || legacy.NonceStatus != "legacy" || legacy.PolicyStatus != "unconfigured" {
		t.Fatalf("legacy=%+v err=%v", legacy, err)
	}
	if _, err := tsa.VerifyTokenPolicy(token, digest, tsa.TrustPolicy{
		RootsFile: filepath.Join(t.TempDir(), "missing.pem"),
	}); !errors.Is(err, tsa.ErrTrustUnavailable) {
		t.Fatalf("trust error=%v", err)
	}
}

func TestVerifyTokenRejectsMissingEKUAndWrongImprint(t *testing.T) {
	digest := sha256.Sum256([]byte("strict signer"))
	genTime := time.Now().UTC().Truncate(time.Second)
	nonce := []byte{4, 3, 2, 1}
	for name, makeToken := range map[string]func() ([]byte, []byte){
		"missing EKU": func() ([]byte, []byte) {
			return localTimestampNoEKU(t, digest, genTime, nonce)
		},
		"wrong imprint algorithm": func() ([]byte, []byte) {
			return localTimestampWithAlgorithm(t, digest, genTime, crypto.SHA384, nonce)
		},
	} {
		t.Run(name, func(t *testing.T) {
			token, roots := makeToken()
			if _, err := tsa.VerifyTokenPolicy(token, digest, tsa.TrustPolicy{
				RootsPEM: roots, PolicyOIDs: []string{"1.2.3.4"}, ExpectedNonce: nonce,
			}); err == nil {
				t.Fatal("malformed TSA proof accepted")
			}
		})
	}
}

func TestVerifyTokenPolicyRootsPEM(t *testing.T) {
	digest := sha256.Sum256([]byte("in-memory roots"))
	genTime := time.Now().UTC().Truncate(time.Second)
	token, rootsPEM := localTimestamp(t, digest, genTime, x509.ExtKeyUsageTimeStamping)
	rootsFile := filepath.Join(t.TempDir(), "root.pem")
	if err := os.WriteFile(rootsFile, rootsPEM, 0600); err != nil {
		t.Fatal(err)
	}

	fromFile, err := tsa.VerifyTokenPolicy(token, digest, tsa.TrustPolicy{RootsFile: rootsFile})
	if err != nil {
		t.Fatal(err)
	}
	fromPEM, err := tsa.VerifyTokenPolicy(token, digest, tsa.TrustPolicy{RootsPEM: rootsPEM})
	if err != nil {
		t.Fatal(err)
	}
	if *fromFile != *fromPEM {
		t.Fatalf("file result=%+v PEM result=%+v", fromFile, fromPEM)
	}
	if !fromPEM.Pinned || fromPEM.TrustMode != "configured" {
		t.Fatalf("PEM trust mode=%q pinned=%v", fromPEM.TrustMode, fromPEM.Pinned)
	}

	for _, roots := range [][]byte{{}, []byte("not a certificate")} {
		if _, err := tsa.VerifyTokenPolicy(token, digest, tsa.TrustPolicy{RootsPEM: roots}); !errors.Is(err, tsa.ErrTrustUnavailable) {
			t.Fatalf("roots PEM %q error=%v", roots, err)
		}
	}
	if _, err := tsa.VerifyTokenPolicy(token, digest, tsa.TrustPolicy{
		RootsFile: rootsFile, RootsPEM: rootsPEM,
	}); !errors.Is(err, tsa.ErrTrustUnavailable) {
		t.Fatalf("multiple roots error=%v", err)
	}
}

func TestStampUsesAndChecksNonce(t *testing.T) {
	digest := sha256.Sum256([]byte("nonce request"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, err := timestamp.ParseRequest(body)
		if err != nil || req.Nonce == nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		token, _ := localTimestampWithNonce(t, digest, time.Now().UTC().Truncate(time.Second),
			x509.ExtKeyUsageTimeStamping, req.Nonce.Bytes())
		_, _ = w.Write(token)
	}))
	defer srv.Close()
	result, err := tsa.New(srv.URL).Stamp(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := timestamp.ParseResponse(result.Token)
	if err != nil || parsed.Nonce == nil || !bytes.Equal(parsed.Nonce.Bytes(), result.Nonce) {
		t.Fatalf("nonce=%x parsed=%v err=%v", result.Nonce, parsed.Nonce, err)
	}
}

func TestStampAcceptsGrantedWithMods(t *testing.T) {
	digest := sha256.Sum256([]byte("granted with mods"))
	genTime := time.Now().UTC().Truncate(time.Second)
	served := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		req, err := timestamp.ParseRequest(body)
		if err != nil || req.Nonce == nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		token, _ := localTimestampWithNonce(t, digest, genTime, x509.ExtKeyUsageTimeStamping, req.Nonce.Bytes())
		modified, err := responseWithStatus(token, int(timestamp.GrantedWithMods), "granted with modifications")
		if err != nil {
			t.Error(err)
			return
		}
		served <- modified
		_, _ = w.Write(modified)
	}))
	defer srv.Close()

	result, err := tsa.New(srv.URL).Stamp(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if !result.GenTime.Equal(genTime) || !bytes.Equal(result.Token, <-served) {
		t.Fatalf("result=%+v", result)
	}
}

func TestVerifyTokenPolicyAcceptsGrantedWithMods(t *testing.T) {
	digest := sha256.Sum256([]byte("granted with mods verification"))
	genTime := time.Now().UTC().Truncate(time.Second)
	nonce := []byte{1, 2, 3, 4}
	token, roots := localTimestampWithNonce(t, digest, genTime, x509.ExtKeyUsageTimeStamping, nonce)
	modified, err := responseWithStatus(token, int(timestamp.GrantedWithMods))
	if err != nil {
		t.Fatal(err)
	}
	result, err := tsa.VerifyTokenPolicy(modified, digest, tsa.TrustPolicy{
		RootsPEM: roots, ExpectedNonce: nonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.GenTime.Equal(genTime) || result.NonceStatus != "matched" {
		t.Fatalf("result=%+v", result)
	}
}

func TestTSARejectsFailureAndTrailingResponseData(t *testing.T) {
	digest := sha256.Sum256([]byte("invalid response envelope"))
	genTime := time.Now().UTC().Truncate(time.Second)
	token, roots := localTimestamp(t, digest, genTime, x509.ExtKeyUsageTimeStamping)
	failure, err := timestamp.CreateErrorResponse(timestamp.Rejection, timestamp.BadRequest)
	if err != nil {
		t.Fatal(err)
	}
	trailing := append(append([]byte(nil), token...), 0)
	for name, body := range map[string][]byte{"failure status": failure, "trailing data": trailing} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(body)
			}))
			_, stampErr := tsa.New(srv.URL).Stamp(t.Context(), digest)
			srv.Close()
			if stampErr == nil {
				t.Fatal("Stamp accepted invalid response")
			}
			if _, verifyErr := tsa.VerifyTokenPolicy(body, digest, tsa.TrustPolicy{RootsPEM: roots}); verifyErr == nil {
				t.Fatal("VerifyTokenPolicy accepted invalid response")
			}
		})
	}
}

func responseWithStatus(data []byte, status int, statusString ...string) ([]byte, error) {
	var response struct {
		Status struct {
			Status       int
			StatusString asn1.RawValue  `asn1:"optional"`
			FailureInfo  asn1.BitString `asn1:"optional"`
		}
		TimeStampToken asn1.RawValue `asn1:"optional"`
	}
	if rest, err := asn1.Unmarshal(data, &response); err != nil {
		return nil, err
	} else if len(rest) > 0 {
		return nil, errors.New("response has trailing data")
	}
	response.Status.Status = status
	if len(statusString) > 0 {
		values := make([]asn1.RawValue, len(statusString))
		for i, value := range statusString {
			values[i] = asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagUTF8String, Bytes: []byte(value)}
		}
		encoded, err := asn1.Marshal(values)
		if err != nil {
			return nil, err
		}
		response.Status.StatusString = asn1.RawValue{FullBytes: encoded}
	}
	return asn1.Marshal(response)
}

func localTimestamp(t *testing.T, digest [32]byte, genTime time.Time, eku x509.ExtKeyUsage) ([]byte, []byte) {
	return localTimestampWithNonce(t, digest, genTime, eku, nil)
}

func localTimestampWithNonce(t *testing.T, digest [32]byte, genTime time.Time, eku x509.ExtKeyUsage, nonce []byte) ([]byte, []byte) {
	return localTimestampWithEKU(t, digest, genTime, []x509.ExtKeyUsage{eku}, nonce, crypto.SHA256, eku == x509.ExtKeyUsageTimeStamping)
}

func localTimestampNoEKU(t *testing.T, digest [32]byte, genTime time.Time, nonce []byte) ([]byte, []byte) {
	return localTimestampWithEKU(t, digest, genTime, nil, nonce, crypto.SHA256, false)
}

func localTimestampWithAlgorithm(t *testing.T, digest [32]byte, genTime time.Time, algorithm crypto.Hash, nonce []byte) ([]byte, []byte) {
	return localTimestampWithEKU(t, digest, genTime, []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping}, nonce, algorithm, true)
}

func localTimestampWithEKU(t *testing.T, digest [32]byte, genTime time.Time, ekus []x509.ExtKeyUsage, nonce []byte, algorithm crypto.Hash, critical bool) ([]byte, []byte) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"},
		NotBefore: genTime.Add(-24 * time.Hour), NotAfter: genTime.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	signerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signerTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "test tsa"},
		NotBefore: genTime.Add(-time.Hour), NotAfter: genTime.Add(2 * time.Second),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: ekus,
	}
	if critical {
		ekuDER, err := asn1.Marshal([]asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 8}})
		if err != nil {
			t.Fatal(err)
		}
		signerTemplate.ExtraExtensions = []pkix.Extension{{
			Id: []int{2, 5, 29, 37}, Critical: true, Value: ekuDER,
		}}
	}
	signerDER, err := x509.CreateCertificate(rand.Reader, signerTemplate, root, &signerKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := x509.ParseCertificate(signerDER)
	if err != nil {
		t.Fatal(err)
	}
	token, err := (&timestamp.Timestamp{
		HashAlgorithm: algorithm, HashedMessage: digest[:], Time: genTime,
		Policy: asn1.ObjectIdentifier{1, 2, 3, 4}, SerialNumber: big.NewInt(42),
		AddTSACertificate: true, Nonce: new(big.Int).SetBytes(nonce),
	}).CreateResponseWithOpts(signer, signerKey, crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})
	return token, rootPEM
}
