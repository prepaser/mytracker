package backup

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/prepaser/mytracker/internal/config"
)

func TestRestoreJournalValidation(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	for _, phase := range []string{"staging", "prepared", "committed"} {
		j := restoreFixture(cfg, phase)
		if phase == "staging" {
			j.OldDB, j.OldProofs = "", ""
		}
		data, err := json.Marshal(j)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseRestoreJournal(data, cfg.DataDir); err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, "stage_dir")
		data, err = json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseRestoreJournal(data, cfg.DataDir); err == nil {
			t.Fatalf("%s: incomplete journal accepted", phase)
		}
	}
	j := restoreFixture(cfg, "prepared")
	j.StageDir = filepath.Join(cfg.DataDir, "proofs")
	data, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseRestoreJournal(data, cfg.DataDir); err == nil {
		t.Fatal("live directory accepted as staging")
	}
	if _, err := parseRestoreJournal([]byte(`{"phase":"unknown"}`), cfg.DataDir); err == nil {
		t.Fatal("unknown phase accepted")
	}
	valid, err := json.Marshal(restoreFixture(cfg, "prepared"))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"had_db", "had_proofs", "old_db", "old_proofs"} {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(valid, &fields); err != nil {
			t.Fatal(err)
		}
		fields[field] = json.RawMessage(`null`)
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseRestoreJournal(data, cfg.DataDir); err == nil {
			t.Fatalf("null %s accepted", field)
		}
	}
	for _, data := range [][]byte{nil, []byte(`{}`), []byte(`{"phase":123}`), append(valid, []byte(` {}`)...)} {
		if _, err := parseRestoreJournal(data, cfg.DataDir); err == nil {
			t.Fatal("invalid journal accepted")
		}
	}
	j = restoreFixture(cfg, "staging")
	data, err = json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseRestoreJournal(data, cfg.DataDir); err == nil {
		t.Fatal("staging rollback metadata accepted")
	}
}
