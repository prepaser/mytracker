package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/prepaser/mytracker/internal/atomicfile"
)

const maxStagedProofBytes = 8 << 20
const maxProofImportJournalBytes = 128 << 20
const maxProofImportEntries = 500000

var errDuplicateProofImport = errors.New("duplicate proof in import")

type proofImportJournal struct {
	Version int                      `json:"version"`
	Entries []proofImportJournalItem `json:"entries"`
}

type proofImportJournalItem struct {
	LocationID     int64  `json:"location_id"`
	LocationDigest string `json:"location_digest"`
	Name           string `json:"name"`
	Digest         string `json:"digest"`
}

// ProofImport keeps proof files outside the live directory until its DB rows
// are committed. The journal makes the file/DB boundary recoverable.
type ProofImport struct {
	proofsDir    string
	stageDir     string
	journal      proofImportJournal
	entryCount   int
	journalBytes int64
	sealed       bool
	installed    bool
}

func BeginProofImport(proofsDir string) (*ProofImport, error) {
	return beginProofImport(proofsDir, atomicfile.SyncDir)
}

func beginProofImport(proofsDir string, syncParent func(string) error) (*ProofImport, error) {
	if strings.TrimSpace(proofsDir) == "" {
		return nil, errors.New("proof directory is required")
	}
	if err := os.MkdirAll(filepath.Join(proofsDir, ".imports"), 0700); err != nil {
		return nil, err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	stageDir := filepath.Join(proofsDir, ".imports", "session-"+hex.EncodeToString(token[:]))
	if err := os.Mkdir(stageDir, 0700); err != nil {
		return nil, err
	}
	p := &ProofImport{
		proofsDir: proofsDir,
		stageDir:  stageDir,
		journal:   proofImportJournal{Version: 3},
	}
	if err := p.persist(); err != nil {
		_ = os.RemoveAll(stageDir)
		return nil, err
	}
	for _, dir := range []string{filepath.Dir(stageDir), proofsDir, filepath.Dir(proofsDir)} {
		if err := syncParent(dir); err != nil {
			_ = os.RemoveAll(stageDir)
			return nil, err
		}
	}
	return p, nil
}

func (p *ProofImport) Add(locationID int64, ext string, data, locationDigest []byte) error {
	if p == nil || locationID < 1 {
		return errors.New("invalid proof import")
	}
	if p.sealed {
		return errors.New("proof import is sealed")
	}
	if ext != ".tsr" && ext != ".ots" {
		return errors.New("invalid proof extension")
	}
	if len(data) > maxStagedProofBytes {
		return errors.New("proof is too large")
	}
	name := fmt.Sprintf("%d%s", locationID, ext)
	if p.stageDir == "" && p.entryCount == 0 && len(p.journal.Entries) > 0 {
		p.entryCount = len(p.journal.Entries)
	}
	if p.entryCount >= maxProofImportEntries {
		return errors.New("too many proof import entries")
	}
	if _, err := os.Stat(filepath.Join(p.stageDir, name)); err == nil {
		return errDuplicateProofImport
	} else if !os.IsNotExist(err) {
		return err
	}
	proofSum := sha256.Sum256(data)
	if err := atomicfile.Write(filepath.Join(p.stageDir, name), data, 0600); err != nil {
		return err
	}
	entry := proofImportJournalItem{
		LocationID: locationID, LocationDigest: hex.EncodeToString(locationDigest),
		Name: name, Digest: hex.EncodeToString(proofSum[:]),
	}
	if err := p.appendEntry(entry); err != nil {
		return err
	}
	p.entryCount++
	return nil
}

func (p *ProofImport) persist() error {
	b, err := json.Marshal(struct {
		Version int `json:"version"`
	}{Version: p.journal.Version})
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := atomicfile.Write(filepath.Join(p.stageDir, "journal.json"), b, 0600); err != nil {
		return err
	}
	p.journalBytes = int64(len(b))
	return nil
}

func (p *ProofImport) appendEntry(entry proofImportJournalItem) error {
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if p.journalBytes+int64(len(b)) > maxProofImportJournalBytes {
		return errors.New("proof import journal is too large")
	}
	f, err := os.OpenFile(filepath.Join(p.stageDir, "journal.json"), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	var n int
	if n, err = f.Write(b); err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		p.journalBytes += int64(len(b))
	}
	return err
}

func (p *ProofImport) Reconcile(s *Store) error {
	return p.ReconcileContext(context.Background(), s)
}

func (p *ProofImport) ReconcileContext(ctx context.Context, s *Store) error {
	if p == nil || s == nil {
		return errors.New("proof import is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.sealed = true
	return s.reconcileProofImport(ctx, p)
}

func (p *ProofImport) Install() error {
	return p.InstallContext(context.Background())
}

func (p *ProofImport) InstallContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.sealed = true
	if err := validateProofImportJournal(p); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.validateEntries(ctx); err != nil {
		return err
	}
	if err := p.install(ctx); err != nil {
		return err
	}
	p.installed = true
	return nil
}

func (tx *ImportTx) MarkProofImport(p *ProofImport) error {
	if p.stageDir == "" && p.entryCount == 0 {
		p.entryCount = len(p.journal.Entries)
	}
	if p.entryCount == 0 {
		return nil
	}
	_, err := tx.tx.Exec(`INSERT INTO proof_import_commits(token) VALUES (?)`, filepath.Base(p.stageDir))
	return err
}

func (s *Store) RecoverProofImports(proofsDir string) error {
	return s.RecoverProofImportsContext(context.Background(), proofsDir)
}

func (s *Store) RecoverProofImportsContext(ctx context.Context, proofsDir string) error {
	unlock, err := s.LockProofMutationContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return s.recoverProofImportsContext(ctx, proofsDir)
}

func (s *Store) recoverProofImports(proofsDir string) error {
	return s.recoverProofImportsContext(context.Background(), proofsDir)
}

func (s *Store) recoverProofImportsContext(ctx context.Context, proofsDir string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	importsDir := filepath.Join(proofsDir, ".imports")
	entries, err := os.ReadDir(importsDir)
	if os.IsNotExist(err) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := atomicfile.SyncDir(proofsDir); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := atomicfile.SyncDir(filepath.Dir(proofsDir)); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := s.db.ExecContext(ctx, `DELETE FROM proof_import_commits`)
		return err
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			continue
		}
		stageDir := filepath.Join(importsDir, entry.Name())
		journalPath := filepath.Join(stageDir, "journal.json")
		journal, readErr := readProofImportJournalContext(ctx, journalPath)
		if os.IsNotExist(readErr) {
			if err := os.RemoveAll(stageDir); err != nil {
				return err
			}
			continue
		}
		if readErr != nil {
			return readErr
		}
		p := &ProofImport{proofsDir: proofsDir, stageDir: stageDir, journal: proofImportJournal{Version: journal.Version}, entryCount: journal.Count}
		if err := s.reconcileProofImport(ctx, p); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := atomicfile.SyncDir(importsDir); err != nil {
		return err
	}
	// Recovery holds the proof mutation lock and has resolved every staging directory.
	_, err = s.db.ExecContext(ctx, `DELETE FROM proof_import_commits`)
	return err
}

type proofImportJournalInfo struct {
	Version int
	Count   int
}

func readProofImportJournalContext(ctx context.Context, path string) (proofImportJournalInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return proofImportJournalInfo{}, err
	}
	version, count, err := streamProofImportFile(path, func(proofImportJournalItem) error {
		return ctx.Err()
	})
	if err != nil {
		return proofImportJournalInfo{}, err
	}
	return proofImportJournalInfo{Version: version, Count: count}, nil
}

func loadProofImportJournal(path string) (proofImportJournal, error) {
	version, count, err := streamProofImportFile(path, nil)
	if err != nil {
		return proofImportJournal{}, err
	}
	journal := proofImportJournal{Version: version, Entries: make([]proofImportJournalItem, 0, count)}
	_, _, err = streamProofImportFile(path, func(entry proofImportJournalItem) error {
		journal.Entries = append(journal.Entries, entry)
		return nil
	})
	if err != nil {
		return proofImportJournal{}, err
	}
	return journal, nil
}

func streamProofImportFile(path string, fn func(proofImportJournalItem) error) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	if info.Size() > maxProofImportJournalBytes {
		return 0, 0, errors.New("proof import journal is too large")
	}
	return streamProofImportJournal(io.LimitReader(f, maxProofImportJournalBytes+1), fn)
}

func streamProofImportJournal(r io.Reader, fn func(proofImportJournalItem) error) (int, int, error) {
	dec := json.NewDecoder(r)
	token, err := dec.Token()
	if err != nil {
		return 0, 0, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return 0, 0, errors.New("invalid proof import journal")
	}
	version := 0
	count := 0
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return 0, 0, err
		}
		name, ok := key.(string)
		if !ok {
			return 0, 0, errors.New("invalid proof import journal")
		}
		if name == "version" {
			if err := dec.Decode(&version); err != nil {
				return 0, 0, err
			}
			continue
		}
		if name != "entries" {
			if err := skipProofImportJSONValue(dec); err != nil {
				return 0, 0, err
			}
			continue
		}
		token, err := dec.Token()
		if err != nil {
			return 0, 0, err
		}
		if token == nil {
			continue
		}
		if delim, ok := token.(json.Delim); !ok || delim != '[' {
			return 0, 0, errors.New("invalid proof import journal")
		}
		for dec.More() {
			var entry proofImportJournalItem
			if err := dec.Decode(&entry); err != nil {
				return 0, 0, err
			}
			if count >= maxProofImportEntries {
				return 0, 0, errors.New("too many proof import entries")
			}
			count++
			if fn != nil {
				if err := fn(entry); err != nil {
					return 0, 0, err
				}
			}
		}
		if _, err := dec.Token(); err != nil {
			return 0, 0, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return 0, 0, err
	}
	if version == 0 {
		return 0, 0, errors.New("invalid proof import journal")
	}
	if version < 1 || version > 3 {
		return 0, 0, errors.New("unsupported proof import journal")
	}
	if version == 1 {
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return 0, 0, errors.New("invalid proof import journal")
		}
		return version, count, nil
	}
	for {
		var entry proofImportJournalItem
		err := dec.Decode(&entry)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return version, count, nil
		}
		if err != nil {
			return 0, 0, err
		}
		if count >= maxProofImportEntries {
			return 0, 0, errors.New("too many proof import entries")
		}
		count++
		if fn != nil {
			if err := fn(entry); err != nil {
				return 0, 0, err
			}
		}
	}
}

func skipProofImportJSONValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		for dec.More() {
			if _, err := dec.Token(); err != nil {
				return err
			}
			if err := skipProofImportJSONValue(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := skipProofImportJSONValue(dec); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid proof import journal")
	}
	_, err = dec.Token()
	return err
}

func (s *Store) reconcileProofImport(ctx context.Context, p *ProofImport) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.stageDir != "" {
		if _, err := os.Stat(p.stageDir); os.IsNotExist(err) {
			var committed int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM proof_import_commits WHERE token=?`, filepath.Base(p.stageDir)).Scan(&committed); err != nil {
				return err
			}
			if committed == 0 {
				return nil
			}
			return errors.New("proof import journal is missing")
		} else if err != nil {
			return err
		}
	}
	if err := validateProofImportJournal(p); err != nil {
		return err
	}
	if p.importEntryCount() == 0 {
		return os.RemoveAll(p.stageDir)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.journal.Version == 3 {
		var committed int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM proof_import_commits WHERE token=?`, filepath.Base(p.stageDir)).Scan(&committed); err != nil {
			return err
		}
		if committed == 1 {
			// The caller keeps installed files locked through the DB commit.
			if !p.installed {
				if err := p.validateEntries(ctx); err != nil {
					return err
				}
				if err := p.installCommitted(ctx, s); err != nil {
					return err
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.RemoveAll(p.stageDir); err != nil {
				return err
			}
			if err := atomicfile.SyncDir(filepath.Dir(p.stageDir)); err != nil {
				return err
			}
			_, err := s.db.ExecContext(ctx, `DELETE FROM proof_import_commits WHERE token=?`, filepath.Base(p.stageDir))
			return err
		}
	}
	if err := p.validateEntries(ctx); err != nil {
		return err
	}
	present := 0
	if p.journal.Version != 3 {
		err := p.forEachEntry(ctx, func(entry proofImportJournalItem) error {
			var digest []byte
			err := s.db.QueryRowContext(ctx, `SELECT digest FROM locations WHERE id=?`, entry.LocationID).Scan(&digest)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if !strings.EqualFold(hex.EncodeToString(digest), entry.LocationDigest) {
				return fmt.Errorf("proof import location %d digest mismatch", entry.LocationID)
			}
			present++
			return nil
		})
		if err != nil {
			return err
		}
	}
	if present == 0 {
		if err := p.removeInstalled(ctx); err != nil {
			return err
		}
		if err := atomicfile.SyncDir(p.proofsDir); err != nil {
			return err
		}
		return os.RemoveAll(p.stageDir)
	}
	if present != p.importEntryCount() {
		return errors.New("proof import transaction is incomplete")
	}
	if err := p.install(ctx); err != nil {
		return err
	}
	return os.RemoveAll(p.stageDir)
}

func (p *ProofImport) installCommitted(ctx context.Context, s *Store) error {
	if err := p.forEachEntry(ctx, func(entry proofImportJournalItem) error {
		var digest []byte
		err := s.db.QueryRowContext(ctx, `SELECT digest FROM locations WHERE id=?`, entry.LocationID).Scan(&digest)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !strings.EqualFold(hex.EncodeToString(digest), entry.LocationDigest) {
			return fmt.Errorf("proof import location %d digest mismatch", entry.LocationID)
		}
		return p.installEntry(ctx, entry)
	}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(atomicfile.SyncDir(p.proofsDir), atomicfile.SyncDir(p.stageDir))
}

func (p *ProofImport) install(ctx context.Context) error {
	err := p.forEachEntry(ctx, func(entry proofImportJournalItem) error {
		return p.installEntry(ctx, entry)
	})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(atomicfile.SyncDir(p.proofsDir), atomicfile.SyncDir(p.stageDir))
}

func (p *ProofImport) installEntry(ctx context.Context, entry proofImportJournalItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stagePath := filepath.Join(p.stageDir, entry.Name)
	destPath := filepath.Join(p.proofsDir, entry.Name)
	stageDigest, stageExists, err := proofFileDigestContext(ctx, stagePath)
	if err != nil {
		return err
	}
	destDigest, destExists, err := proofFileDigestContext(ctx, destPath)
	if err != nil {
		return err
	}
	if destExists && !strings.EqualFold(destDigest, entry.Digest) {
		return fmt.Errorf("existing proof %s differs from import", entry.Name)
	}
	if stageExists && !strings.EqualFold(stageDigest, entry.Digest) {
		return fmt.Errorf("staged proof %s digest mismatch", entry.Name)
	}
	if !stageExists && !destExists {
		return fmt.Errorf("proof %s is missing", entry.Name)
	}
	if stageExists && destExists {
		return fmt.Errorf("proof %s already exists", entry.Name)
	}
	if stageExists {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Rename(stagePath, destPath); err != nil {
			return err
		}
	}
	return nil
}

func (p *ProofImport) removeInstalled(ctx context.Context) error {
	return p.forEachEntry(ctx, func(entry proofImportJournalItem) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		stagePath := filepath.Join(p.stageDir, entry.Name)
		destPath := filepath.Join(p.proofsDir, entry.Name)
		stageInfo, err := os.Stat(stagePath)
		if err == nil {
			if !stageInfo.Mode().IsRegular() {
				return fmt.Errorf("proof path is not a regular file: %s", entry.Name)
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		destDigest, destExists, err := proofFileDigestContext(ctx, destPath)
		if err != nil {
			return err
		}
		if destExists {
			if !strings.EqualFold(destDigest, entry.Digest) {
				return fmt.Errorf("installed proof %s digest mismatch", entry.Name)
			}
			if err := os.Remove(destPath); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *ProofImport) importEntryCount() int {
	if p.entryCount > 0 {
		return p.entryCount
	}
	return len(p.journal.Entries)
}

func (p *ProofImport) forEachEntry(ctx context.Context, fn func(proofImportJournalItem) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if p.stageDir == "" && len(p.journal.Entries) > 0 {
		for _, entry := range p.journal.Entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := fn(entry); err != nil {
				return err
			}
		}
		return nil
	}
	version, count, err := streamProofImportFile(filepath.Join(p.stageDir, "journal.json"), func(entry proofImportJournalItem) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(entry)
	})
	if err != nil {
		return err
	}
	if version != p.journal.Version || count != p.importEntryCount() {
		return errors.New("proof import journal changed")
	}
	return nil
}

func (p *ProofImport) validateEntries(ctx context.Context) error {
	return p.forEachEntry(ctx, validateProofImportEntry)
}

func validateProofImportEntry(entry proofImportJournalItem) error {
	if entry.LocationID < 1 || entry.Name != fmt.Sprintf("%d%s", entry.LocationID, filepath.Ext(entry.Name)) ||
		(filepath.Ext(entry.Name) != ".tsr" && filepath.Ext(entry.Name) != ".ots") ||
		filepath.Base(entry.Name) != entry.Name || len(entry.LocationDigest) != sha256.Size*2 || len(entry.Digest) != sha256.Size*2 {
		return fmt.Errorf("invalid proof import entry %q", entry.Name)
	}
	if _, err := hex.DecodeString(entry.LocationDigest); err != nil {
		return fmt.Errorf("invalid location digest: %w", err)
	}
	if _, err := hex.DecodeString(entry.Digest); err != nil {
		return fmt.Errorf("invalid proof digest: %w", err)
	}
	return nil
}

func validateProofImportJournal(p *ProofImport) error {
	if p.journal.Version < 1 || p.journal.Version > 3 {
		return errors.New("unsupported proof import journal")
	}
	for _, entry := range p.journal.Entries {
		if err := validateProofImportEntry(entry); err != nil {
			return err
		}
	}
	return nil
}

func proofFileDigest(path string) (string, bool, error) {
	return proofFileDigestContext(context.Background(), path)
}

func proofFileDigestContext(ctx context.Context, path string) (string, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("proof path is not a regular file: %s", filepath.Base(path))
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(contextReader{ctx: ctx, r: f}, maxStagedProofBytes+1))
	if err != nil {
		return "", false, err
	}
	if n > maxStagedProofBytes {
		return "", false, errors.New("staged proof is too large")
	}
	return hex.EncodeToString(h.Sum(nil)), true, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
