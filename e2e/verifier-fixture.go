//go:build ignore

package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/export"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/digitorus/timestamp"
	libots "github.com/nbd-wtf/opentimestamps"
)

func main() {
	if len(os.Args) != 2 {
		fail(fmt.Errorf("usage: go run e2e/verifier-fixture.go <fixture-dir>"))
	}
	if err := generate(os.Args[1]); err != nil {
		fail(err)
	}
}

func generate(root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	dataDir := filepath.Join(root, "data")
	proofsDir := filepath.Join(dataDir, "proofs")
	if err := os.MkdirAll(proofsDir, 0700); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dataDir, "mytracker.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	record := canonical.Record{
		User: "fixture", Device: "verifier", Tst: time.Now().Unix() - 60,
		Lat: 37.5665, Lon: 126.978,
	}
	digest, err := record.Digest()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"_type": "location", "tst": record.Tst, "lat": record.Lat, "lon": record.Lon,
	})
	if err != nil {
		return err
	}
	id, err := st.Insert(&store.Location{
		User: record.User, Device: record.Device, Tst: record.Tst,
		Lat: record.Lat, Lon: record.Lon, Payload: payload, Digest: digest[:],
	})
	if err != nil {
		return err
	}

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	genTime := time.Now().UTC().Truncate(time.Second)
	completed := map[string]bool{}
	for range 2 {
		now := time.Now().Unix()
		job, err := st.ClaimStampJob(now, "verifier-fixture", now+3600)
		if err != nil {
			return err
		}
		if job == nil {
			return fmt.Errorf("missing stamp job")
		}
		switch job.Kind {
		case "tsa":
			stampTime := genTime.Unix()
			if err := st.CompleteStampJob(job, "ok", &stampTime, nonce); err != nil {
				return err
			}
		case "ots":
			if err := st.CompleteStampJob(job, "pending", nil); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected stamp job kind %q", job.Kind)
		}
		completed[job.Kind] = true
	}
	if !completed["tsa"] || !completed["ots"] {
		return fmt.Errorf("did not complete both stamp jobs")
	}

	loc, err := st.Get(id)
	if err != nil {
		return err
	}
	tsr, roots, err := makeTimestamp(loc.Digest, genTime, loc.TSANonce)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "roots.pem"), roots, 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(proofsDir, fmt.Sprintf("%d.tsr", id)), tsr, 0600); err != nil {
		return err
	}

	pending := libots.File{
		Digest: loc.Digest,
		Sequences: []libots.Sequence{{{Attestation: &libots.Attestation{
			CalendarServerURL: "https://test.invalid",
		}}}},
	}
	otsPath := filepath.Join(proofsDir, fmt.Sprintf("%d.ots", id))
	if err := os.WriteFile(otsPath, pending.SerializeToFile(), 0600); err != nil {
		return err
	}
	if err := writeExport(filepath.Join(root, "fixture.zip"), st, proofsDir); err != nil {
		return err
	}

	anchored := libots.File{
		Digest: loc.Digest,
		Sequences: []libots.Sequence{{{Attestation: &libots.Attestation{
			BitcoinBlockHeight: 1,
		}}}},
	}
	if err := os.WriteFile(otsPath, anchored.SerializeToFile(), 0600); err != nil {
		return err
	}
	return writeExport(filepath.Join(root, "anchored.zip"), st, proofsDir)
}

func makeTimestamp(digest []byte, genTime time.Time, nonce []byte) ([]byte, []byte, error) {
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	rootTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "verifier fixture root"},
		NotBefore: genTime.Add(-24 * time.Hour), NotAfter: genTime.Add(48 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		return nil, nil, err
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return nil, nil, err
	}
	signerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	eku, err := asn1.Marshal([]asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 8}})
	if err != nil {
		return nil, nil, err
	}
	signerTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "verifier fixture TSA"},
		NotBefore: genTime.Add(-time.Hour), NotAfter: genTime.Add(48 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
		ExtraExtensions: []pkix.Extension{{
			Id: asn1.ObjectIdentifier{2, 5, 29, 37}, Critical: true, Value: eku,
		}},
	}
	signerDER, err := x509.CreateCertificate(rand.Reader, signerTemplate, root, &signerKey.PublicKey, rootKey)
	if err != nil {
		return nil, nil, err
	}
	signer, err := x509.ParseCertificate(signerDER)
	if err != nil {
		return nil, nil, err
	}
	response, err := (&timestamp.Timestamp{
		HashAlgorithm: crypto.SHA256, HashedMessage: digest, Time: genTime,
		Policy: asn1.ObjectIdentifier{1, 2, 3, 4}, SerialNumber: big.NewInt(42),
		AddTSACertificate: true, Nonce: new(big.Int).SetBytes(nonce),
	}).CreateResponseWithOpts(signer, signerKey, crypto.SHA256)
	if err != nil {
		return nil, nil, err
	}
	roots := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})
	return response, roots, nil
}

func writeExport(path string, st *store.Store, proofsDir string) error {
	var archive bytes.Buffer
	if err := export.WriteZip(&archive, export.Options{Store: st, ProofsDir: proofsDir}); err != nil {
		return err
	}
	return os.WriteFile(path, archive.Bytes(), 0600)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
