package ots_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	libots "github.com/nbd-wtf/opentimestamps"
	"github.com/prepaser/mytracker/internal/ots"
)

func TestStampAllCalendarsFail(t *testing.T) {
	c := ots.New([]string{"http://127.0.0.1:1/nope"})
	digest := sha256.Sum256([]byte("mock"))
	_, err := c.Stamp(context.Background(), digest)
	if err == nil {
		t.Fatal("expected calendar failure")
	}
}

func TestSaveLoadProof(t *testing.T) {
	dir := t.TempDir()
	data := []byte{0, 'O', 'T', 'S'}
	if err := ots.SaveProof(dir, 7, data); err != nil {
		t.Fatal(err)
	}
	got, err := ots.LoadProof(dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("got %v", got)
	}
	want := filepath.Join(dir, "7.ots")
	if gotPath := filepath.Join(dir, "7.ots"); gotPath != want {
		t.Fatalf("path %s", gotPath)
	}
}

func TestPendingInvalidProof(t *testing.T) {
	if !ots.Pending([]byte("not ots")) {
		t.Fatal("invalid proof should be treated as pending")
	}
}

func TestStampAndUpgradeWithCalendar(t *testing.T) {
	var srv *httptest.Server
	var posts atomic.Int32
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var seq libots.Sequence
		if r.Method == http.MethodPost {
			posts.Add(1)
			seq = libots.Sequence{{Attestation: &libots.Attestation{CalendarServerURL: srv.URL}}}
		} else {
			seq = libots.Sequence{{Attestation: &libots.Attestation{BitcoinBlockHeight: 123}}}
		}
		_, _ = w.Write((libots.File{Sequences: []libots.Sequence{seq}}).SerializeInstructionSequences())
	}))
	defer srv.Close()
	digest := sha256.Sum256([]byte("calendar"))
	alias := strings.Replace(srv.URL, "http://", "HTTP://", 1) + "/"
	calendars := []string{"ftp://calendar.example", "http://127.0.0.1:0", alias, srv.URL}
	proof, err := ots.New(calendars).Stamp(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if !ots.Pending(proof) {
		t.Fatal("new proof should be pending")
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("duplicate calendar requests=%d", got)
	}
	upgraded, changed, err := ots.Upgrade(context.Background(), proof, digest, calendars)
	if err != nil || !changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
	if ots.Pending(upgraded) {
		t.Fatal("upgraded proof is still pending")
	}
}

func TestVerifyPoolDeduplicatesProviderAliases(t *testing.T) {
	header := blockOneHeader()
	digest := [32]byte(header.MerkleRoot)
	proof := bitcoinProof(digest, 1)
	server := fakeEsplora(t, header, nil, 6)
	alias := strings.Replace(server.URL, "http://", "HTTP://", 1) + "/"
	result, err := ots.VerifyPool(t.Context(), proof, digest, []string{server.URL, alias})
	if err != nil || result.Result != "degraded" || result.Agreeing != 1 || len(result.Attempts) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestVerifyPoolConsensus(t *testing.T) {
	header := blockOneHeader()
	digest := [32]byte(header.MerkleRoot)
	proof := bitcoinProof(digest, 1)
	a := fakeEsplora(t, header, nil, 6)
	b := fakeEsplora(t, header, nil, 6)
	result, err := ots.VerifyPool(context.Background(), proof, digest, []string{a.URL, b.URL})
	if err != nil || result.Result != "verified" || result.Agreeing != 2 ||
		result.Height != 1 || result.Confirmations != 6 || !result.POWValid {
		t.Fatalf("result=%+v err=%v", result, err)
	}

	result, err = ots.VerifyPool(context.Background(), proof, digest, []string{a.URL, "http://127.0.0.1:1"})
	if err != nil || result.Result != "degraded" || result.Agreeing != 1 {
		t.Fatalf("degraded=%+v err=%v", result, err)
	}

	pending := fakeEsplora(t, header, nil, 5)
	result, err = ots.VerifyPool(context.Background(), proof, digest, []string{pending.URL, b.URL}, 7)
	if err != nil || result.Result != "pending" || result.Confirmations != 6 {
		t.Fatalf("pending=%+v err=%v", result, err)
	}
}

func TestVerifyPoolPendingInvalidAndTimeout(t *testing.T) {
	digest := sha256.Sum256([]byte("proof"))
	pending := (libots.File{
		Digest: digest[:],
		Sequences: []libots.Sequence{{{
			Attestation: &libots.Attestation{CalendarServerURL: "https://calendar.example"},
		}}},
	}).SerializeToFile()
	result, err := ots.VerifyPool(context.Background(), pending, digest, []string{"https://example.com"})
	if err != nil || result.Result != "pending" {
		t.Fatalf("pending=%+v err=%v", result, err)
	}
	other := sha256.Sum256([]byte("other"))
	result, err = ots.VerifyPool(context.Background(), pending, other, []string{"https://example.com"})
	if err == nil || result.Result != "invalid" {
		t.Fatalf("digest=%+v err=%v", result, err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result, err = ots.VerifyPool(ctx, bitcoinProof(digest, 1), digest, []string{slow.URL})
	if err != nil || result.Result != "error" || result.Attempts[0].ErrorCode != "provider_timeout" {
		t.Fatalf("timeout=%+v err=%v", result, err)
	}
}

func TestVerifyPoolRejectsHeaderHashPOWAndMerkle(t *testing.T) {
	header := blockOneHeader()
	digest := [32]byte(header.MerkleRoot)
	proof := bitcoinProof(digest, 1)
	wrongHash := chainhash.Hash{}
	hashMismatch := fakeEsplora(t, header, &wrongHash, 10)
	result, _ := ots.VerifyPool(t.Context(), proof, digest, []string{hashMismatch.URL})
	if result.Result != "error" || result.Attempts[0].ErrorCode != "header_hash_mismatch" {
		t.Fatalf("hash mismatch=%+v", result)
	}

	badPOW := header
	badPOW.Nonce++
	powServer := fakeEsplora(t, badPOW, nil, 10)
	result, _ = ots.VerifyPool(t.Context(), proof, digest, []string{powServer.URL})
	if result.Result != "error" || result.Attempts[0].ErrorCode != "header_pow_invalid" {
		t.Fatalf("pow=%+v", result)
	}

	other := sha256.Sum256([]byte("other merkle root"))
	mismatchProof := bitcoinProof(other, 1)
	a := fakeEsplora(t, header, nil, 10)
	b := fakeEsplora(t, header, nil, 10)
	result, _ = ots.VerifyPool(t.Context(), mismatchProof, other, []string{a.URL, b.URL})
	if result.Result != "invalid" || result.Attempts[0].ErrorCode != "merkle_mismatch" {
		t.Fatalf("merkle=%+v", result)
	}
}

func TestVerifyPoolSelectsValidLaterSequence(t *testing.T) {
	header := blockOneHeader()
	digest := [32]byte(header.MerkleRoot)
	provider := func(invalidFirst bool) *httptest.Server {
		hash := header.BlockHash()
		var buf bytes.Buffer
		if err := header.Serialize(&buf); err != nil {
			t.Fatal(err)
		}
		headerHex := hex.EncodeToString(buf.Bytes())
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/block-height/2":
				claimed := hash.String()
				if invalidFirst {
					claimed = strings.Repeat("0", 64)
				}
				_, _ = w.Write([]byte(claimed))
			case r.URL.Path == "/block-height/1":
				_, _ = w.Write([]byte(hash.String()))
			case strings.HasSuffix(r.URL.Path, "/header"):
				_, _ = w.Write([]byte(headerHex))
			case r.URL.Path == "/blocks/tip/height":
				_, _ = fmt.Fprint(w, 10)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	a, b := provider(true), provider(true)
	proof := (libots.File{Digest: digest[:], Sequences: []libots.Sequence{
		{{Attestation: &libots.Attestation{BitcoinBlockHeight: 2}}},
		{{Attestation: &libots.Attestation{BitcoinBlockHeight: 1}}},
	}}).SerializeToFile()
	result, err := ots.VerifyPool(t.Context(), proof, digest, []string{a.URL, b.URL})
	if err != nil || result.Result != "verified" || result.Height != 1 || result.Agreeing != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	c, d := provider(false), provider(false)
	result, err = ots.VerifyPool(t.Context(), proof, digest, []string{c.URL, d.URL})
	if err != nil || result.Result != "verified" || result.Height != 1 || result.Confirmations != 10 {
		t.Fatalf("strongest=%+v err=%v", result, err)
	}
}

func bitcoinProof(digest [32]byte, height uint64) []byte {
	return (libots.File{
		Digest: digest[:],
		Sequences: []libots.Sequence{{{
			Attestation: &libots.Attestation{BitcoinBlockHeight: height},
		}}},
	}).SerializeToFile()
}

func fakeEsplora(t *testing.T, header wire.BlockHeader, claimed *chainhash.Hash, tip int64) *httptest.Server {
	t.Helper()
	hash := header.BlockHash()
	if claimed != nil {
		hash = *claimed
	}
	var buf bytes.Buffer
	if err := header.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	headerHex := hex.EncodeToString(buf.Bytes())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/block-height/"):
			_, _ = w.Write([]byte(hash.String()))
		case strings.HasSuffix(r.URL.Path, "/header"):
			_, _ = w.Write([]byte(headerHex))
		case r.URL.Path == "/blocks/tip/height":
			_, _ = fmt.Fprint(w, tip)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func blockOneHeader() wire.BlockHeader {
	return wire.BlockHeader{
		Version: 1,
		PrevBlock: chainhash.Hash{
			0x6f, 0xe2, 0x8c, 0x0a, 0xb6, 0xf1, 0xb3, 0x72,
			0xc1, 0xa6, 0xa2, 0x46, 0xae, 0x63, 0xf7, 0x4f,
			0x93, 0x1e, 0x83, 0x65, 0xe1, 0x5a, 0x08, 0x9c,
			0x68, 0xd6, 0x19,
		},
		MerkleRoot: chainhash.Hash{
			0x98, 0x20, 0x51, 0xfd, 0x1e, 0x4b, 0xa7, 0x44,
			0xbb, 0xbe, 0x68, 0x0e, 0x1f, 0xee, 0x14, 0x67,
			0x7b, 0xa1, 0xa3, 0xc3, 0x54, 0x0b, 0xf7, 0xb1,
			0xcd, 0xb6, 0x06, 0xe8, 0x57, 0x23, 0x3e, 0x0e,
		},
		Timestamp: time.Unix(0x4966bc61, 0),
		Bits:      0x1d00ffff,
		Nonce:     0x9962e301,
	}
}
