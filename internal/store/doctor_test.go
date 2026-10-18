package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

var (
	errAuditRows                 = errors.New("audit rows failed")
	registerAuditRowsErrorDriver sync.Once
)

func TestAuditProofsRejectsRowsIterationError(t *testing.T) {
	const driverName = "mytracker-audit-rows-error"
	registerAuditRowsErrorDriver.Do(func() { sql.Register(driverName, auditRowsErrorDriver{}) })
	db, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	audit, err := (&Store{db: db}).AuditProofs(t.TempDir())
	if !errors.Is(err, errAuditRows) {
		t.Fatalf("err=%v", err)
	}
	if len(audit.Missing) != 0 || len(audit.Orphan) != 0 {
		t.Fatalf("audit=%+v", audit)
	}
}

func TestAuditProofsReportsMissingAndOrphanFiles(t *testing.T) {
	st := openAuditTestDB(t)
	defer func() { _ = st.Close() }()
	proofs := t.TempDir()

	tsa := auditTestLocation("tsa")
	tsa.TsaStatus = "ok"
	tsaID, err := st.ImportLocation(tsa)
	if err != nil {
		t.Fatal(err)
	}
	ots := auditTestLocation("ots")
	ots.OtsStatus = "confirmed"
	otsID, err := st.ImportLocation(ots)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proofs, fmt.Sprintf("%d.tsr", tsaID)), []byte("proof"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proofs, "orphan.ots"), []byte("proof"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(proofs, fmt.Sprintf("%d.ots", otsID)), 0700); err != nil {
		t.Fatal(err)
	}

	audit, err := st.AuditProofs(proofs)
	if err != nil {
		t.Fatal(err)
	}
	wantMissing := []string{fmt.Sprintf("%d.ots", otsID)}
	if len(audit.Missing) != len(wantMissing) || audit.Missing[0] != wantMissing[0] {
		t.Fatalf("missing=%v want=%v", audit.Missing, wantMissing)
	}
	if len(audit.Orphan) != 1 || audit.Orphan[0] != "orphan.ots" {
		t.Fatalf("orphan=%v", audit.Orphan)
	}
}

func TestAuditProofsReadsDirectoryInBatches(t *testing.T) {
	st := openAuditTestDB(t)
	defer func() { _ = st.Close() }()
	proofs := t.TempDir()
	count := auditProofReadDirBatchSize*2 + 1
	for i := range count {
		name := fmt.Sprintf("orphan-%04d.tsr", i)
		if err := os.WriteFile(filepath.Join(proofs, name), []byte("proof"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	audit, err := st.AuditProofs(proofs)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit.Orphan) != count {
		t.Fatalf("orphan count=%d want=%d", len(audit.Orphan), count)
	}
	for i, name := range audit.Orphan {
		want := fmt.Sprintf("orphan-%04d.tsr", i)
		if name != want {
			t.Fatalf("orphan[%d]=%q want=%q", i, name, want)
		}
	}
}

func openAuditTestDB(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func auditTestLocation(device string) *Location {
	return &Location{
		User: "alice", Device: device, Tst: 1751800000,
		Lat: 37.5665, Lon: 126.978,
		Payload: []byte(`{"_type":"location"}`), Digest: []byte{1, 2, 3, 4},
	}
}

type auditRowsErrorDriver struct{}

func (auditRowsErrorDriver) Open(string) (driver.Conn, error) {
	return auditRowsErrorConn{}, nil
}

type auditRowsErrorConn struct{}

func (auditRowsErrorConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (auditRowsErrorConn) Close() error                        { return nil }
func (auditRowsErrorConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (auditRowsErrorConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &auditRowsErrorRows{}, nil
}

type auditRowsErrorRows struct {
	read bool
}

func (r *auditRowsErrorRows) Columns() []string { return []string{"id", "tsa_status", "ots_status"} }
func (r *auditRowsErrorRows) Close() error      { return nil }
func (r *auditRowsErrorRows) Next(dest []driver.Value) error {
	if r.read {
		return errAuditRows
	}
	r.read = true
	dest[0] = int64(1)
	dest[1] = "ok"
	dest[2] = "pending"
	return nil
}

var _ driver.Rows = (*auditRowsErrorRows)(nil)
