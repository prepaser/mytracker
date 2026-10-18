package proofverify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	libots "github.com/nbd-wtf/opentimestamps"
	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
	"github.com/prepaser/mytracker/internal/worker"
)

func (s *Service) verifyKind(ctx context.Context, loc *store.Location, digest [32]byte, payloadOK bool, kind, trigger string) *store.VerificationRun {
	cfg := s.cfg.Snapshot()
	ext := ".tsr"
	if kind == "ots" {
		ext = ".ots"
	}
	proof, err := readBoundedFile(filepath.Join(cfg.ProofsDir(), fmt.Sprintf("%d%s", loc.ID, ext)), maxArtifactBytes)
	return s.verifyProof(ctx, verificationInput{cfg: cfg, location: loc, proof: proof, readErr: err}, digest, payloadOK, kind, trigger)
}

func TestImportedVerificationHistoryStaysStaleUntilNativeRun(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	raw := map[string]any{"_type": "location", "tst": float64(1), "lat": 37.5, "lon": 127.0}
	payload, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := canonical.FromLocation("u", "d", raw)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := rec.Digest()
	if err != nil {
		t.Fatal(err)
	}
	proofs := map[string][]byte{"tsa": []byte("tsa-proof"), "ots": []byte("ots-proof")}
	for kind, proof := range proofs {
		ext := ".tsr"
		if kind == "ots" {
			ext = ".ots"
		}
		if err := os.WriteFile(filepath.Join(cfg.ProofsDir(), "1"+ext), proof, 0600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Unix()
	var id int64
	err = st.ImportAtomic(func(tx *store.ImportTx) error {
		var err error
		id, err = tx.InsertLocation(&store.Location{
			User: "u", Device: "d", Tst: 1, Lat: 37.5, Lon: 127,
			Payload: payload, Digest: digest[:],
		})
		if err != nil {
			return err
		}
		for kind, proof := range proofs {
			proofHash := sha256.Sum256(proof)
			if _, err := tx.InsertVerification(&store.VerificationRun{
				LocationID: id, Kind: kind, Result: "verified", CheckedAt: now,
				EngineVersion: engineVersion, PolicySHA256: policyHash(cfg, kind), ProofSHA256: proofHash[:],
			}); err != nil {
				return err
			}
			if err := tx.EnqueueVerification(id, kind, "import", now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	svc := &Service{store: st, cfg: config.NewManager(cfg, nil)}
	view, err := svc.Latest(id)
	if err != nil {
		t.Fatal(err)
	}
	if !view.TSAStale || !view.OTSStale || view.Overall != "stale" {
		t.Fatalf("imported history was trusted: %+v", view)
	}
	stats, staleCount, err := svc.Summary()
	if err != nil || stats.Results["verified"] != 0 || staleCount != 1 {
		t.Fatalf("imported history summary=%+v stale=%d err=%v", stats, staleCount, err)
	}
	if policyStale, err := svc.PolicyStaleCount(); err != nil || policyStale != 1 {
		t.Fatalf("imported policy stale=%d err=%v", policyStale, err)
	}
	for range 2 {
		job, err := st.ClaimVerificationJob(now, "test", now+90)
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			t.Fatal("queued imported verification job was not claimed")
		}
		proofHash := sha256.Sum256(proofs[job.Kind])
		if err := st.FinishVerificationJob(job, &store.VerificationRun{
			LocationID: id, Kind: job.Kind, Trigger: job.Trigger, Result: "verified",
			CheckedAt: now + 1, EngineVersion: engineVersion,
			PolicySHA256: policyHash(cfg, job.Kind), ProofSHA256: proofHash[:],
		}, false, nil, now+2); err != nil {
			t.Fatal(err)
		}
	}
	view, err = svc.Latest(id)
	if err != nil {
		t.Fatal(err)
	}
	if view.TSAStale || view.OTSStale || view.Overall != "verified" {
		t.Fatalf("native verification did not restore trust: %+v", view)
	}
	stats, staleCount, err = svc.Summary()
	if err != nil || stats.Results["verified"] != 2 || staleCount != 0 {
		t.Fatalf("native verification summary=%+v stale=%d err=%v", stats, staleCount, err)
	}
	if policyStale, err := svc.PolicyStaleCount(); err != nil || policyStale != 0 {
		t.Fatalf("native policy stale=%d err=%v", policyStale, err)
	}
	proofHash := sha256.Sum256(proofs["ots"])
	if err := st.ImportVerification(&store.VerificationRun{
		LocationID: id, Kind: "ots", Trigger: "manual", Result: "verified", CheckedAt: now + 3,
		EngineVersion: engineVersion, PolicySHA256: policyHash(cfg, "ots"), ProofSHA256: proofHash[:],
	}, []store.VerificationAttempt{{ProviderID: "esplora-5", Result: "verified"}}); err != nil {
		t.Fatal(err)
	}
	view, err = svc.Latest(id)
	if err != nil || !view.OTSStale || view.Overall != "stale" {
		t.Fatalf("import after completed job was trusted: %+v err=%v", view, err)
	}
	providers, err := st.VerificationProviderStatuses()
	if err != nil || len(providers) != 0 {
		t.Fatalf("imported provider attempt was trusted: %+v err=%v", providers, err)
	}
}

func TestVerifyKindOTSConsensusAndPending(t *testing.T) {
	header := testBlockHeader()
	digest := [32]byte(header.MerkleRoot)
	a := testEsplora(t, header, 6)
	b := testEsplora(t, header, 6)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.EsploraURLs = []string{a.URL, b.URL}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(cfg, nil)
	svc := &Service{cfg: mgr}
	loc := &store.Location{ID: 1}
	proof := (libots.File{
		Digest: digest[:],
		Sequences: []libots.Sequence{{{
			Attestation: &libots.Attestation{BitcoinBlockHeight: 1},
		}}},
	}).SerializeToFile()
	if err := os.WriteFile(filepath.Join(cfg.ProofsDir(), "1.ots"), proof, 0600); err != nil {
		t.Fatal(err)
	}
	run := svc.verifyKind(t.Context(), loc, digest, true, "ots", "manual")
	if run.Result != "verified" || run.AgreeingSources != 2 || len(run.Attempts) != 2 ||
		!run.BitcoinPOWValid || run.BitcoinConfirmations != 6 {
		t.Fatalf("verified=%+v", run)
	}
	svc.observeRun(run, time.Now())
	a.Close()
	b.Close()
	run = svc.verifyKind(t.Context(), loc, digest, true, "ots", "manual")
	if run.Result != "error" || run.ErrorCode != "provider_unavailable" {
		t.Fatalf("provider failure=%+v", run)
	}

	pending := (libots.File{
		Digest: digest[:],
		Sequences: []libots.Sequence{{{
			Attestation: &libots.Attestation{CalendarServerURL: "https://calendar.example"},
		}}},
	}).SerializeToFile()
	if err := os.WriteFile(filepath.Join(cfg.ProofsDir(), "1.ots"), pending, 0600); err != nil {
		t.Fatal(err)
	}
	run = svc.verifyKind(t.Context(), loc, digest, true, "ots", "scheduled")
	if run.Result != "pending" {
		t.Fatalf("pending=%+v", run)
	}
}

func TestVerificationInputWaitsForProofMutation(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.Insert(&store.Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	job, err := st.ClaimStampJob(now, "stamp", now+90)
	if err != nil || job == nil || job.Kind != "tsa" {
		t.Fatalf("stamp claim=%+v err=%v", job, err)
	}
	svc := &Service{store: st, cfg: config.NewManager(cfg, nil)}
	unlock, err := st.LockProofMutationContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := svc.readVerificationInput(ctx, id, "tsa"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("verification bypassed mutation: %v", err)
	}
	proof, nonce := []byte("replacement proof"), []byte("replacement nonce")
	if err := os.WriteFile(filepath.Join(cfg.ProofsDir(), fmt.Sprintf("%d.tsr", id)), proof, 0600); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteStampJob(job, "ok", &now, nonce); err != nil {
		t.Fatal(err)
	}
	unlock()
	input, err := svc.readVerificationInput(t.Context(), id, "tsa")
	if err != nil || input.readErr != nil || !bytes.Equal(input.location.TSANonce, nonce) || !bytes.Equal(input.proof, proof) {
		t.Fatalf("snapshot=%+v err=%v", input, err)
	}
	ctx, cancelMutation := context.WithTimeout(t.Context(), time.Second)
	defer cancelMutation()
	release, err := st.LockProofMutationContext(ctx)
	if err != nil {
		t.Fatalf("snapshot retained proof lock: %v", err)
	}
	release()
}

func TestReconcileBoundsFreshScan(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for i := range 251 {
		id, err := st.Insert(&store.Location{User: "u", Device: "d", Tst: int64(i + 1), Payload: []byte(`{}`), Digest: make([]byte, 32)})
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"tsa", "ots"} {
			if err := st.SaveVerification(&store.VerificationRun{LocationID: id, Kind: kind, Trigger: "manual", Result: "missing", CheckedAt: time.Now().Unix(), EngineVersion: engineVersion, PolicySHA256: policyHash(cfg, kind)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := &Service{store: st, cfg: config.NewManager(cfg, nil), ctx: t.Context(), reconcileWake: worker.NewSignal()}
	s.reconcile()
	if s.reconcileID != 250 || s.reconcileKind != "ots" {
		t.Fatalf("cursor=%d/%s", s.reconcileID, s.reconcileKind)
	}
	s.reconcile()
	if s.reconcileID != 0 {
		t.Fatalf("unfinished scan: %d", s.reconcileID)
	}
}

func TestProofChangedSinceRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proof.ots")
	old := []byte("old-proof")
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(old)
	run := &store.VerificationRun{ProofSHA256: hash[:]}
	if proofChangedSinceRun(run, path) {
		t.Fatal("unchanged proof reported as changed")
	}
	if err := os.WriteFile(path, []byte("new-proof"), 0600); err != nil {
		t.Fatal(err)
	}
	if !proofChangedSinceRun(run, path) {
		t.Fatal("changed proof was not detected")
	}
	if proofChangedSinceRun(&store.VerificationRun{ErrorCode: "worker panic"}, path) {
		t.Fatal("generic error was treated as proof recovery")
	}
	if !proofChangedSinceRun(&store.VerificationRun{ErrorCode: "proof_read"}, path) {
		t.Fatal("read failure recovery was not detected")
	}
	if proofChangedSinceRun(nil, path) == false {
		t.Fatal("missing run was not treated as stale")
	}
	if proofChangedSinceRun(nil, filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("missing proof without a run was treated as changed")
	}
	if !proofChangedSinceRun(run, filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("removed proof was not treated as changed")
	}
	missingRun := &store.VerificationRun{Result: "missing", ErrorCode: "proof_missing"}
	if proofChangedSinceRun(missingRun, filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("unchanged missing proof was treated as changed")
	}
	directory := t.TempDir()
	if !proofChangedSinceRun(missingRun, directory) {
		t.Fatal("unreadable proof was not treated as changed")
	}
	if proofChangedSinceRun(&store.VerificationRun{Result: "error", ErrorCode: "proof_read"}, directory) {
		t.Fatal("unchanged unreadable proof was treated as changed")
	}
}

func TestReconcileRecentFailureYieldsToPolicyChanges(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.Insert(&store.Location{
		User: "u", Device: "policy-change", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{"tst":1,"lat":1,"lon":2}`), Digest: make([]byte, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if err := st.SaveVerification(&store.VerificationRun{
		LocationID: id, Kind: "tsa", Trigger: "manual", Result: "missing", CheckedAt: now,
		EngineVersion: engineVersion, PolicySHA256: policyHash(cfg, "tsa"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnqueueVerification(id, "ots", "manual", now); err != nil {
		t.Fatal(err)
	}
	for attempt := range 5 {
		stamp := now + int64(attempt*100)
		job, err := st.ClaimVerificationJob(stamp, fmt.Sprintf("owner-%d", attempt), stamp+int64(leaseDuration.Seconds()))
		if err != nil || job == nil {
			t.Fatalf("claim attempt %d: job=%+v err=%v", attempt, job, err)
		}
		run := &store.VerificationRun{
			LocationID: id, Kind: "ots", Trigger: "manual", Result: "error", CheckedAt: stamp,
			EngineVersion: engineVersion, PolicySHA256: policyHash(cfg, "ots"),
		}
		if err := st.FinishVerificationJob(job, run, true, errors.New("network"), stamp+1); err != nil {
			t.Fatal(err)
		}
	}
	j := &Service{
		store: st, cfg: config.NewManager(cfg, nil), ctx: t.Context(),
		wake: worker.NewSignal(), reconcileWake: worker.NewSignal(),
	}
	j.accepting.Store(true)
	j.reconcile()
	jobs, err := st.ListLocationVerificationJobs(id)
	if err != nil || len(jobs) != 1 || jobs[0].State != "failed" {
		t.Fatalf("unchanged policy jobs=%+v err=%v", jobs, err)
	}
	changed := cfg.Clone()
	changed.OTSConfirmations++
	j.cfg = config.NewManager(changed, nil)
	j.reconcile()
	jobs, err = st.ListLocationVerificationJobs(id)
	if err != nil || len(jobs) != 1 || jobs[0].State != "queued" {
		t.Fatalf("changed policy jobs=%+v err=%v", jobs, err)
	}
	j.verifyFn = func(context.Context, *store.Location, [32]byte, bool, string, string) *store.VerificationRun {
		panic("boom")
	}
	for attempt := range 5 {
		stamp := time.Now().Add(time.Duration(attempt+1) * time.Hour)
		job, err := st.ClaimVerificationJob(stamp.Unix(), fmt.Sprintf("panic-owner-%d", attempt), stamp.Add(leaseDuration).Unix())
		if err != nil || job == nil {
			t.Fatalf("panic claim attempt %d: job=%+v err=%v", attempt, job, err)
		}
		j.handleJob(job)
	}
	jobs, err = st.ListLocationVerificationJobs(id)
	if err != nil || len(jobs) != 1 || jobs[0].State != "failed" {
		t.Fatalf("nil-run failures jobs=%+v err=%v", jobs, err)
	}
	j.reconcile()
	jobs, err = st.ListLocationVerificationJobs(id)
	if err != nil || len(jobs) != 1 || jobs[0].State != "failed" {
		t.Fatalf("requeued nil-run failures jobs=%+v err=%v", jobs, err)
	}
}

func TestVerificationHelpersAndErrorBranches(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.TSATrustRoots = filepath.Join(cfg.DataDir, "missing.pem")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	svc := &Service{cfg: config.NewManager(cfg, nil)}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	svc.store = st
	if run, err := svc.executeJob(context.Background(),
		&store.VerificationJob{LocationID: 999, Kind: "tsa"}); err == nil || run != nil {
		t.Fatalf("missing execute run=%+v err=%v", run, err)
	}
	loc := &store.Location{ID: 1, Digest: make([]byte, 32), Payload: []byte(`{}`)}
	run := svc.verifyKind(context.Background(), loc, [32]byte{}, false, "tsa", "manual")
	if run.Result != "invalid" || run.ErrorCode != "payload_digest_mismatch" {
		t.Fatalf("payload=%+v", run)
	}
	if err := os.WriteFile(filepath.Join(cfg.ProofsDir(), "1.tsr"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	run = svc.verifyKind(context.Background(), loc, [32]byte{}, true, "tsa", "manual")
	if run.Result != "invalid" || run.ErrorCode != "tsa_invalid" {
		t.Fatalf("tsa=%+v", run)
	}
	if checkedAt(nil) != 0 || checkedAt(run) == 0 {
		t.Fatal("checkedAt")
	}
	oldMissing := &store.VerificationRun{
		Result: "missing", EngineVersion: engineVersion,
		PolicySHA256: policyHash(cfg, "tsa"), CheckedAt: time.Now().Add(-25 * time.Hour).Unix(),
	}
	if !stale(oldMissing, filepath.Join(cfg.ProofsDir(), "missing.tsr"), oldMissing.PolicySHA256) {
		t.Fatal("old missing proof is not stale")
	}
	freshMissing := *oldMissing
	freshMissing.CheckedAt = time.Now().Unix()
	if stale(&freshMissing, filepath.Join(cfg.ProofsDir(), "missing.tsr"), freshMissing.PolicySHA256) {
		t.Fatal("fresh missing proof is stale")
	}
	if !stale(&freshMissing, cfg.ProofsDir(), freshMissing.PolicySHA256) {
		t.Fatal("unreadable proof was treated as a fresh missing proof")
	}
	if overall(nil, nil, true, true, true) != "stale" ||
		overall(run, run, false, false, false) != "invalid" ||
		overall(&store.VerificationRun{Result: "verified"},
			&store.VerificationRun{Result: "inconclusive"}, false, false, true) != "inconclusive" ||
		overall(&store.VerificationRun{Result: "inconclusive"},
			&store.VerificationRun{Result: "error"}, false, false, true) != "error" ||
		overall(&store.VerificationRun{Result: "missing"},
			&store.VerificationRun{Result: "inconclusive"}, false, false, true) != "missing" {
		t.Fatal("overall")
	}
	if (&Service{}).Healthy() {
		t.Fatal("zero heartbeat is healthy")
	}
	bad := &store.Location{Digest: make([]byte, 32), Payload: []byte(`{`)}
	if _, _, _, err := digestMatches(bad); err == nil {
		t.Fatal("malformed payload accepted")
	}
	hash := policyHash(cfg, "tsa")
	if len(hash) != 32 {
		t.Fatalf("policy hash=%x", hash)
	}
}

func TestDigestMatches(t *testing.T) {
	payload := []byte(`{"_type":"location","lat":1,"lon":2,"tst":100}`)
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	rec, err := canonical.FromLocation("u", "d", raw)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := rec.Digest()
	if err != nil {
		t.Fatal(err)
	}
	loc := &store.Location{User: "u", Device: "d", Payload: payload, Digest: digest[:]}
	_, _, ok, err := digestMatches(loc)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	loc.Digest = []byte{1, 2, 3}
	_, _, ok, err = digestMatches(loc)
	if err != nil || ok {
		t.Fatalf("tamper ok=%v err=%v", ok, err)
	}
}

func TestVerificationArtifactReadsAreBounded(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	svc := &Service{cfg: config.NewManager(cfg, nil)}
	loc := &store.Location{ID: 1}
	if err := os.WriteFile(filepath.Join(cfg.ProofsDir(), "1.ots"), make([]byte, maxArtifactBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	run := svc.verifyKind(t.Context(), loc, [32]byte{}, true, "ots", "manual")
	if run.Result != "error" || run.ErrorCode != "proof_read" {
		t.Fatalf("oversized proof=%+v", run)
	}
	if err := os.WriteFile(filepath.Join(cfg.ProofsDir(), "1.tsr"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(cfg.DataDir, "roots.pem")
	if err := os.WriteFile(rootPath, make([]byte, maxArtifactBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedFile(rootPath, maxArtifactBytes); !errors.Is(err, errArtifactTooLarge) {
		t.Fatalf("oversized trust roots error=%v", err)
	}
}

func TestTrustRootStaging(t *testing.T) {
	if path, cleanup, err := stageTrustRoots(""); err != nil || path != "" {
		t.Fatalf("empty path=%q err=%v", path, err)
	} else {
		cleanup()
	}

	source := filepath.Join(t.TempDir(), "roots.pem")
	want := []byte("test trust roots\n")
	if err := os.WriteFile(source, want, 0600); err != nil {
		t.Fatal(err)
	}
	staged, cleanup, err := stageTrustRoots(source)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(staged)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("staged roots=%q err=%v", got, err)
	}
	cleanup()
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged roots remain: %v", err)
	}

	if _, cleanup, err := stageTrustRoots(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		cleanup()
		t.Fatal("missing trust roots were staged")
	}

	empty, cleanup, err := stageEmptyTrustRoots()
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(empty); err != nil || info.Size() != 0 {
		t.Fatalf("empty roots info=%+v err=%v", info, err)
	}
	cleanup()

	f, err := os.CreateTemp(t.TempDir(), "closed-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeAndSync(f, []byte("x")); err == nil {
		t.Fatal("write to closed trust-root staging file succeeded")
	}
}

func TestConfiguredTrustRootAndStoreErrors(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	roots := filepath.Join(cfg.DataDir, "roots.pem")
	if err := os.WriteFile(roots, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.TSATrustRoots = roots
	mgr := config.NewManager(cfg, nil)
	svc := &Service{cfg: mgr}
	if err := os.WriteFile(filepath.Join(cfg.ProofsDir(), "1.tsr"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	run := svc.verifyKind(t.Context(), &store.Location{ID: 1}, [32]byte{}, true, "tsa", "manual")
	if run.Result != "invalid" || run.ErrorCode != "tsa_invalid" {
		t.Fatalf("configured root result=%+v", run)
	}
	if bytes.Equal(policyHash(cfg, "tsa"), policyHash(config.Default(), "tsa")) {
		t.Fatal("configured trust root did not affect policy hash")
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.Insert(&store.Location{
		User: "u", Device: "bad-payload", Tst: 1, Lat: 1, Lon: 1,
		Payload: []byte(`{`), Digest: make([]byte, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.store = st
	view, err := svc.Latest(id)
	if err != nil || view.PayloadOK || view.Overall != "invalid" {
		t.Fatalf("malformed payload view=%+v err=%v", view, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	svc.ctx = context.Background()
	if svc.runReady("closed-store") {
		t.Fatal("closed store reported completed work")
	}
}

func TestWorkerPanicReturnsDurableJobToRetry(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	loc := &store.Location{
		User: "u", Device: "d", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: make([]byte, 32),
	}
	id, err := st.Insert(loc)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, _, err := st.EnqueueVerification(id, "tsa", "manual", now.Unix()); err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimVerificationJob(now.Unix(), "panic-owner", now.Add(leaseDuration).Unix())
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := &Service{
		cfg:   config.NewManager(cfg, nil),
		store: st, ctx: ctx, cancel: cancel, wake: worker.NewSignal(),
		reconcileWake: worker.NewSignal(),
		verifyFn: func(context.Context, *store.Location, [32]byte, bool, string, string) *store.VerificationRun {
			panic("boom")
		},
	}
	svc.handleJob(job)
	jobs, err := st.ListLocationVerificationJobs(id)
	if err != nil || len(jobs) != 1 || jobs[0].State != "retry" ||
		!strings.Contains(jobs[0].LastError, "panic") {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
}

func TestCanceledVerificationRetriesInsteadOfCompleting(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.Insert(&store.Location{
		User: "u", Device: "cancel", Tst: 1, Lat: 1, Lon: 2,
		Payload: []byte(`{}`), Digest: make([]byte, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_, _, err = st.EnqueueVerification(id, "tsa", "manual", now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	job, err := st.ClaimVerificationJob(now.Unix(), "cancel-owner", now.Add(leaseDuration).Unix())
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	svc := &Service{
		cfg:   config.NewManager(cfg, nil),
		store: st, ctx: ctx, cancel: cancel, wake: worker.NewSignal(),
		reconcileWake: worker.NewSignal(), heartbeat: worker.NewHeartbeat(time.Now),
		verifyFn: func(ctx context.Context, _ *store.Location, _ [32]byte, _ bool, _, _ string) *store.VerificationRun {
			close(started)
			<-ctx.Done()
			return &store.VerificationRun{LocationID: id, Kind: "tsa", Result: "verified"}
		},
	}
	done := make(chan struct{})
	go func() {
		svc.handleJob(job)
		close(done)
	}()
	<-started
	cancel()
	<-done

	jobs, err := st.ListLocationVerificationJobs(id)
	if err != nil || len(jobs) != 1 || jobs[0].State != "retry" {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
}

func testEsplora(t *testing.T, header wire.BlockHeader, tip int64) *httptest.Server {
	t.Helper()
	hash := header.BlockHash()
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

func testBlockHeader() wire.BlockHeader {
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
