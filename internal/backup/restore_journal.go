package backup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

func parseRestoreJournal(data []byte, dataDir string) (restoreJournal, error) {
	var j restoreJournal
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return j, fmt.Errorf("invalid restore journal")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return j, fmt.Errorf("invalid restore journal")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return j, fmt.Errorf("invalid restore journal")
	}
	required := []string{"phase", "stage_dir"}
	switch j.Phase {
	case "staging":
		if j.OldDB != "" || j.OldProofs != "" || j.HadDB || j.HadProofs {
			return j, fmt.Errorf("invalid staging restore journal")
		}
	case "prepared", "committed":
		required = append(required, "old_db", "old_proofs", "had_db", "had_proofs")
		if !restorePath(dataDir, j.OldDB, ".restore-old-", ".db") ||
			!restorePath(dataDir, j.OldProofs, ".restore-old-proofs-", "") {
			return j, fmt.Errorf("invalid restore rollback paths")
		}
	default:
		return j, fmt.Errorf("invalid restore journal phase")
	}
	for _, field := range required {
		value, ok := fields[field]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return j, fmt.Errorf("incomplete restore journal")
		}
	}
	if !restorePath(dataDir, j.StageDir, ".restore-stage-", "") {
		return j, fmt.Errorf("invalid restore staging path")
	}
	return j, nil
}

func restorePath(dataDir, path, prefix, suffix string) bool {
	if path == "" {
		return false
	}
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil || filepath.Dir(abs) != root {
		return false
	}
	name := filepath.Base(abs)
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) && len(name) > len(prefix)+len(suffix)
}
