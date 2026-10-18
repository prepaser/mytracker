package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProofImportParentSyncFailure(t *testing.T) {
	for failAt := range 3 {
		dir := t.TempDir()
		proofs := filepath.Join(dir, "proofs")
		want := []string{filepath.Join(proofs, ".imports"), proofs, dir}
		calls := 0
		injected := errors.New("directory sync failed")
		p, err := beginProofImport(proofs, func(path string) error {
			if path != want[calls] {
				t.Fatalf("sync path=%s want=%s", path, want[calls])
			}
			calls++
			if calls == failAt+1 {
				return injected
			}
			return nil
		})
		if p != nil || !errors.Is(err, injected) {
			t.Fatalf("import=%v err=%v", p, err)
		}
		entries, err := os.ReadDir(filepath.Join(proofs, ".imports"))
		if err != nil || len(entries) != 0 {
			t.Fatalf("staging=%v err=%v", entries, err)
		}
	}
}

func TestProofImportOrphanMarkers(t *testing.T) {
	for _, state := range []string{"missing", "empty", "corrupt"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			st, err := Open(filepath.Join(dir, "db.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			proofs := filepath.Join(dir, "proofs")
			if state != "missing" {
				if err := os.MkdirAll(filepath.Join(proofs, ".imports"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if state == "corrupt" {
				p, err := BeginProofImport(proofs)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p.stageDir, "journal.json"), []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.db.Exec(`INSERT INTO proof_import_commits VALUES ('orphan')`); err != nil {
				t.Fatal(err)
			}
			err = st.RecoverProofImports(proofs)
			if (err != nil) != (state == "corrupt") {
				t.Fatalf("recovery: %v", err)
			}
			var count int
			if err := st.db.QueryRow(`SELECT COUNT(*) FROM proof_import_commits`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if state == "corrupt" {
				want = 1
			}
			if count != want {
				t.Fatalf("markers=%d want=%d", count, want)
			}
		})
	}
}

func TestLoadProofImportJournalVersions(t *testing.T) {
	entry := proofImportJournalItem{
		LocationID: 1, LocationDigest: strings.Repeat("0", 64),
		Name: "1.tsr", Digest: strings.Repeat("1", 64),
	}
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"v1", mustJSON(t, proofImportJournal{Version: 1, Entries: []proofImportJournalItem{entry}})},
		{"v2", append(append(mustJSON(t, proofImportJournal{Version: 2}), '\n'), append(mustJSON(t, entry), '\n')...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.json")
			if err := os.WriteFile(path, test.data, 0600); err != nil {
				t.Fatal(err)
			}
			journal, err := loadProofImportJournal(path)
			if err != nil || journal.Version == 0 || len(journal.Entries) != 1 || journal.Entries[0].Name != entry.Name {
				t.Fatalf("journal=%+v err=%v", journal, err)
			}
		})
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestProofImportRollbackWithExistingLocation(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	digest := make([]byte, 32)
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	p, err := BeginProofImport(filepath.Join(dir, "proofs"))
	if err != nil {
		t.Fatal(err)
	}
	err = st.ImportAtomic(func(tx *ImportTx) error {
		newID, err := tx.InsertLocation(&Location{User: "u", Device: "d", Tst: 2, Payload: []byte(`{}`), Digest: digest})
		if err != nil {
			return err
		}
		for _, locationID := range []int64{id, newID} {
			if err := p.Add(locationID, ".tsr", []byte("proof"), digest); err != nil {
				return err
			}
		}
		if err := p.Install(); err != nil {
			return err
		}
		if err := p.Install(); err != nil {
			t.Fatalf("repeat install: %v", err)
		}
		if err := tx.MarkProofImport(p); err != nil {
			return err
		}
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	if err := st.RecoverProofImports(p.proofsDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.proofsDir, "1.tsr")); !os.IsNotExist(err) {
		t.Fatalf("rolled-back proof remains: %v", err)
	}
}

func TestProofImportTruncatedTail(t *testing.T) {
	p, err := BeginProofImport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Add(1, ".tsr", []byte("proof"), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.stageDir, "journal.json")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"location_id":2`)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	j, err := loadProofImportJournal(path)
	if err != nil || len(j.Entries) != 1 {
		t.Fatalf("journal=%+v err=%v", j, err)
	}
}

func TestProofImportContextCancellationLeavesRecoverableJournal(t *testing.T) {
	dir := t.TempDir()
	proofs := filepath.Join(dir, "proofs")
	st, err := Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	p, err := BeginProofImport(proofs)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Add(1, ".tsr", []byte("proof"), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if err := p.Add(2, ".tsr", []byte("proof"), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if len(p.journal.Entries) != 0 {
		t.Fatalf("journal entries retained=%d", len(p.journal.Entries))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.InstallContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("install error=%v", err)
	}
	for _, name := range []string{"1.tsr", "2.tsr"} {
		if _, err := os.Stat(filepath.Join(proofs, name)); !os.IsNotExist(err) {
			t.Fatalf("cancelled install exposed %s: %v", name, err)
		}
	}
	if err := p.ReconcileContext(ctx, st); !errors.Is(err, context.Canceled) {
		t.Fatalf("reconcile error=%v", err)
	}
	if _, err := os.Stat(p.stageDir); err != nil {
		t.Fatalf("cancelled journal lost: %v", err)
	}
	if err := p.Reconcile(st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.stageDir); !os.IsNotExist(err) {
		t.Fatalf("journal recovery failed: %v", err)
	}
}

func TestLegacyProofImportStillRecovers(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	digest := make([]byte, 32)
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	p, err := BeginProofImport(filepath.Join(dir, "proofs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Add(id, ".ots", []byte("legacy"), digest); err != nil {
		t.Fatal(err)
	}
	journal, err := loadProofImportJournal(filepath.Join(p.stageDir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	journal.Version = 1
	if err := os.WriteFile(filepath.Join(p.stageDir, "journal.json"), mustJSON(t, journal), 0600); err != nil {
		t.Fatal(err)
	}
	if err := st.RecoverProofImports(p.proofsDir); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(p.proofsDir, "1.ots")); err != nil || string(data) != "legacy" {
		t.Fatalf("proof=%q err=%v", data, err)
	}
}

func TestProofImportRecoveryInstallsCommittedProof(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mytracker.db")
	proofs := filepath.Join(dir, "proofs")
	if err := os.Mkdir(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := make([]byte, 32)
	digest[0] = 1
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	imp, err := BeginProofImport(proofs)
	if err != nil {
		t.Fatal(err)
	}
	if err := imp.Add(id, ".tsr", []byte("proof"), digest); err != nil {
		t.Fatal(err)
	}
	if err := st.ImportAtomic(func(tx *ImportTx) error { return tx.MarkProofImport(imp) }); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if got, err := os.ReadFile(filepath.Join(proofs, "1.tsr")); err != nil || string(got) != "proof" {
		t.Fatalf("proof=%q err=%v", got, err)
	}
}

func TestProofImportRecoverySkipsCommittedProofDeletedBeforeRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mytracker.db")
	proofs := filepath.Join(dir, "proofs")
	if err := os.Mkdir(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := bytes.Repeat([]byte{1}, 32)
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	imp, err := BeginProofImport(proofs)
	if err != nil {
		t.Fatal(err)
	}
	if err := imp.Add(id, ".tsr", []byte("proof"), digest); err != nil {
		t.Fatal(err)
	}
	if err := st.ImportAtomic(func(tx *ImportTx) error { return tx.MarkProofImport(imp) }); err != nil {
		t.Fatal(err)
	}
	if err := imp.Install(); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteIDs([]int64{id}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.ProcessProofGC(proofs, 10); err != nil || n != 2 {
		t.Fatalf("gc=%d err=%v", n, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := os.Stat(filepath.Join(proofs, "1.tsr")); !os.IsNotExist(err) {
		t.Fatalf("deleted proof was resurrected: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(proofs, ".imports"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging entries=%v err=%v", entries, err)
	}
	var markers int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM proof_import_commits`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 0 {
		t.Fatalf("markers=%d", markers)
	}
}

func TestProofImportRecoveryRemovesRolledBackStage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mytracker.db")
	proofs := filepath.Join(dir, "proofs")
	if err := os.Mkdir(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	imp, err := BeginProofImport(proofs)
	if err != nil {
		t.Fatal(err)
	}
	if err := imp.Add(1, ".ots", []byte("proof"), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := os.Stat(filepath.Join(proofs, "1.ots")); !os.IsNotExist(err) {
		t.Fatalf("rolled-back proof remains: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(proofs, ".imports"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging entries=%v err=%v", entries, err)
	}
}

func TestProofImportRecoveryRemovesProofInstalledBeforeRollback(t *testing.T) {
	dir := t.TempDir()
	proofs := filepath.Join(dir, "proofs")
	if err := os.Mkdir(proofs, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(dir, "mytracker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	imp, err := BeginProofImport(proofs)
	if err != nil {
		t.Fatal(err)
	}
	digest := make([]byte, 32)
	err = st.ImportAtomic(func(tx *ImportTx) error {
		id, err := tx.InsertLocation(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: digest})
		if err != nil {
			return err
		}
		if err := imp.Add(id, ".tsr", []byte("proof"), digest); err != nil {
			return err
		}
		if err := imp.Install(); err != nil {
			return err
		}
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("import transaction committed")
	}
	if err := imp.Reconcile(st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(proofs, "1.tsr")); !os.IsNotExist(err) {
		t.Fatalf("rolled-back proof remains: %v", err)
	}
}

func TestProofImportRejectsInvalidInput(t *testing.T) {
	if _, err := BeginProofImport(" "); err == nil {
		t.Fatal("empty proof directory accepted")
	}
	dir := t.TempDir()
	imp, err := BeginProofImport(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(imp.stageDir) }()
	if err := imp.Add(0, ".tsr", nil, nil); err == nil {
		t.Fatal("zero location accepted")
	}
	if err := imp.Add(1, ".bad", nil, nil); err == nil {
		t.Fatal("invalid extension accepted")
	}
	if err := imp.Add(1, ".ots", make([]byte, maxStagedProofBytes+1), nil); err == nil {
		t.Fatal("oversized proof accepted")
	}
	digest := make([]byte, 32)
	if err := imp.Add(1, ".tsr", []byte("proof"), digest); err != nil {
		t.Fatal(err)
	}
	if err := imp.Add(1, ".tsr", []byte("proof"), digest); err == nil {
		t.Fatal("duplicate proof accepted")
	}
	imp.journal.Version = 0
	if err := imp.Install(); err == nil {
		t.Fatal("invalid journal accepted")
	}
	if err := imp.Reconcile(nil); err == nil {
		t.Fatal("nil store accepted")
	}
}

func TestBeginProofImportRejectsFileAsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proofs")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginProofImport(path); err == nil {
		t.Fatal("file accepted as proof directory")
	}
}

func TestProofImportInstallRejectsFileConflicts(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*ProofImport, string) error
	}{
		{"missing", func(p *ProofImport, stage string) error { return os.Remove(stage) }},
		{"staged digest", func(p *ProofImport, stage string) error { return os.WriteFile(stage, []byte("changed"), 0600) }},
		{"destination digest", func(p *ProofImport, _ string) error {
			return os.WriteFile(filepath.Join(p.proofsDir, "1.tsr"), []byte("changed"), 0600)
		}},
		{"both paths", func(p *ProofImport, _ string) error {
			return os.WriteFile(filepath.Join(p.proofsDir, "1.tsr"), []byte("proof"), 0600)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			proofs := t.TempDir()
			p, err := BeginProofImport(proofs)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Add(1, ".tsr", []byte("proof"), make([]byte, 32)); err != nil {
				t.Fatal(err)
			}
			if err := test.setup(p, filepath.Join(p.stageDir, "1.tsr")); err != nil {
				t.Fatal(err)
			}
			if err := p.Install(); err == nil {
				t.Fatal("conflicting proof files accepted")
			}
		})
	}
}

func TestRecoverProofImportsRejectsCorruptJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mytracker.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	stage := filepath.Join(dir, "proofs", ".imports", "corrupt")
	if err := os.MkdirAll(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "journal.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := st.RecoverProofImports(filepath.Join(dir, "proofs")); err == nil {
		t.Fatal("corrupt journal accepted")
	}
}

func TestRecoverProofImportsHandlesAbandonedAndUnsupportedStages(t *testing.T) {
	for _, test := range []struct {
		name    string
		journal string
		wantErr bool
	}{
		{"abandoned", "", false},
		{"unsupported", `{"version":4,"entries":[]}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			st, err := Open(filepath.Join(dir, "mytracker.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			proofs := filepath.Join(dir, "proofs")
			stage := filepath.Join(proofs, ".imports", test.name)
			if err := os.MkdirAll(stage, 0700); err != nil {
				t.Fatal(err)
			}
			if test.journal != "" {
				if err := os.WriteFile(filepath.Join(stage, "journal.json"), []byte(test.journal), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = st.RecoverProofImports(proofs)
			if (err != nil) != test.wantErr {
				t.Fatalf("err=%v wantErr=%t", err, test.wantErr)
			}
			if !test.wantErr {
				if _, err := os.Stat(stage); !os.IsNotExist(err) {
					t.Fatalf("abandoned stage remains: %v", err)
				}
			}
		})
	}
}

func TestProofFileDigestRejectsInvalidFiles(t *testing.T) {
	dir := t.TempDir()
	if _, exists, err := proofFileDigest(filepath.Join(dir, "missing")); err != nil || exists {
		t.Fatalf("missing exists=%t err=%v", exists, err)
	}
	if _, _, err := proofFileDigest(dir); err == nil {
		t.Fatal("directory accepted")
	}
	path := filepath.Join(dir, "large.tsr")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxStagedProofBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := proofFileDigest(path); err == nil {
		t.Fatal("oversized proof accepted")
	}
}

func TestProofImportRejectsIncompleteOrMismatchedTransaction(t *testing.T) {
	for _, test := range []struct {
		name   string
		second bool
		digest []byte
	}{
		{"digest mismatch", false, bytes.Repeat([]byte{2}, 32)},
		{"partial transaction", true, make([]byte, 32)},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			proofs := filepath.Join(dir, "proofs")
			if err := os.Mkdir(proofs, 0700); err != nil {
				t.Fatal(err)
			}
			st, err := Open(filepath.Join(dir, "mytracker.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			if _, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: test.digest}); err != nil {
				t.Fatal(err)
			}
			p, err := BeginProofImport(proofs)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Add(1, ".tsr", []byte("proof"), make([]byte, 32)); err != nil {
				t.Fatal(err)
			}
			if test.second {
				if err := p.Add(2, ".tsr", []byte("proof"), make([]byte, 32)); err != nil {
					t.Fatal(err)
				}
			}
			p.journal.Version = 2
			if err := p.Reconcile(st); err == nil {
				t.Fatal("inconsistent import accepted")
			}
		})
	}
}

func TestProofImportJournalValidation(t *testing.T) {
	valid := proofImportJournalItem{LocationID: 1, Name: "1.tsr", LocationDigest: strings.Repeat("0", 64), Digest: strings.Repeat("0", 64)}
	for _, entry := range []proofImportJournalItem{
		{LocationID: 0, Name: "bad", LocationDigest: valid.LocationDigest, Digest: valid.Digest},
		{LocationID: 1, Name: "1.tsr", LocationDigest: strings.Repeat("z", 64), Digest: valid.Digest},
		{LocationID: 1, Name: "1.tsr", LocationDigest: valid.LocationDigest, Digest: strings.Repeat("z", 64)},
	} {
		p := &ProofImport{journal: proofImportJournal{Version: 1, Entries: []proofImportJournalItem{entry}}}
		if err := validateProofImportJournal(p); err == nil {
			t.Fatalf("invalid entry accepted: %+v", entry)
		}
	}
}

func TestProofImportReportsStagingWriteFailures(t *testing.T) {
	digest := make([]byte, 32)
	t.Run("proof", func(t *testing.T) {
		p, err := BeginProofImport(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(p.stageDir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.stageDir, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := p.Add(1, ".tsr", []byte("proof"), digest); err == nil {
			t.Fatal("unwritable stage accepted")
		}
	})
	t.Run("journal", func(t *testing.T) {
		p, err := BeginProofImport(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		journal := filepath.Join(p.stageDir, "journal.json")
		if err := os.Remove(journal); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(journal, 0700); err != nil {
			t.Fatal(err)
		}
		if err := p.Add(1, ".tsr", []byte("proof"), digest); err == nil {
			t.Fatal("unwritable journal accepted")
		}
	})
}
