package importzip

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/backup"
)

func TestArchiveIndexLifecycle(t *testing.T) {
	ctx := t.Context()
	index, err := newArchiveIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := index.path
	if err := index.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := index.Begin(ctx); err == nil {
		t.Fatal("nested transaction accepted")
	}
	if err := index.addLocation(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := index.addLocation(ctx, 1); err == nil {
		t.Fatal("duplicate location accepted")
	}
	found, err := index.hasLocation(ctx, 1)
	missing, missingErr := index.hasLocation(ctx, 2)
	if err != nil || missingErr != nil || !found || missing {
		t.Fatalf("location lookup found=%t missing=%t errors=%v/%v", found, missing, err, missingErr)
	}
	if _, mapped, err := index.locationID(ctx, 1); err != nil || mapped {
		t.Fatalf("unmapped location mapped=%t err=%v", mapped, err)
	}
	digest := bytes.Repeat([]byte{7}, 32)
	if err := index.mapLocation(ctx, 1, 10, true, digest); err != nil {
		t.Fatal(err)
	}
	if id, ok, err := index.locationID(ctx, 1); err != nil || !ok || id != 10 {
		t.Fatalf("location id=%d ok=%t err=%v", id, ok, err)
	}
	if _, ok, err := index.locationID(ctx, 2); err != nil || ok {
		t.Fatalf("missing location ok=%t err=%v", ok, err)
	}
	if err := index.addRun(ctx, 1, 1, "ots"); err != nil {
		t.Fatal(err)
	}
	if err := index.addRun(ctx, 2, 1, "ots"); err != nil {
		t.Fatal(err)
	}
	if err := index.addRun(ctx, 2, 1, "ots"); err == nil {
		t.Fatal("duplicate run accepted")
	}
	found, err = index.hasRun(ctx, 1)
	missing, missingErr = index.hasRun(ctx, 3)
	if err != nil || missingErr != nil || !found || missing {
		t.Fatalf("run lookup found=%t missing=%t errors=%v/%v", found, missing, err, missingErr)
	}
	if _, mapped, err := index.runID(ctx, 1); err != nil || mapped {
		t.Fatalf("unmapped run mapped=%t err=%v", mapped, err)
	}
	if latest, err := index.isLatestRun(ctx, 2, 1, "ots"); err != nil || !latest {
		t.Fatalf("latest=%t err=%v", latest, err)
	}
	if err := index.mapRun(ctx, 1, 20); err != nil {
		t.Fatal(err)
	}
	if id, ok, err := index.runID(ctx, 1); err != nil || !ok || id != 20 {
		t.Fatalf("run id=%d ok=%t err=%v", id, ok, err)
	}
	if err := index.addProvider(ctx, 1, "esplora-1"); err != nil {
		t.Fatal(err)
	}
	if err := index.addProvider(ctx, 1, "esplora-1"); err == nil {
		t.Fatal("duplicate provider accepted")
	}
	if count, err := index.providerCount(ctx, 1); err != nil || count != 1 {
		t.Fatalf("providers=%d err=%v", count, err)
	}
	if err := index.addProofReference(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := index.addProofReference(ctx, "proofs/1.ots"); err != nil {
		t.Fatal(err)
	}
	if count, err := index.proofReferenceCount(ctx); err != nil || count != 1 {
		t.Fatalf("proof references=%d err=%v", count, err)
	}
	var verificationIDs []int64
	if err := index.eachVerificationLocation(ctx, func(id int64) error {
		verificationIDs = append(verificationIDs, id)
		return nil
	}); err != nil || len(verificationIDs) != 1 || verificationIDs[0] != 10 {
		t.Fatalf("verification ids=%v err=%v", verificationIDs, err)
	}
	if err := index.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := index.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := index.markVerify(ctx, 1); err != nil {
		t.Fatal(err)
	}
	stop := os.ErrClosed
	if err := index.eachVerificationLocation(ctx, func(int64) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("iteration error=%v", err)
	}
	if err := index.eachQueuedStamp(ctx, func(id int64, got []byte) {
		if id != 10 || !bytes.Equal(got, digest) {
			t.Errorf("queued id=%d digest=%x", id, got)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := index.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	index.Rollback()
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temporary index remains: %v", err)
	}
}

func TestArchiveIndexRejectsInvalidTempDirectory(t *testing.T) {
	path := t.TempDir() + "/file"
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", path)
	if index, err := newArchiveIndex(t.Context()); err == nil {
		_ = index.Close()
		t.Fatal("invalid temporary directory accepted")
	}
}

func TestArchiveIndexScavengeRemovesStaleCrashArtifact(t *testing.T) {
	f, err := os.CreateTemp("", "mytracker-import-index-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := backup.ScavengeTemp(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale import index remains: %v", err)
	}
}

func TestArchiveIndexScavengePreservesActiveArtifact(t *testing.T) {
	index, err := newArchiveIndex(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(index.path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := backup.ScavengeTemp(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(index.path); err != nil {
		t.Fatalf("active import index removed: %v", err)
	}
}
