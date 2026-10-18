package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMigrationRegistry(t *testing.T) {
	if err := validateMigrations(migrations); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func([]migration) []migration
	}{
		{"missing", func(ms []migration) []migration { return ms[:len(ms)-1] }},
		{"duplicate", func(ms []migration) []migration {
			cp := append([]migration(nil), ms...)
			cp[1].version = 1
			return cp
		}},
		{"unnamed", func(ms []migration) []migration {
			cp := append([]migration(nil), ms...)
			cp[0].name = ""
			return cp
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateMigrations(tt.edit(migrations)); err == nil {
				t.Fatal("invalid registry accepted")
			}
		})
	}
}

func TestMigrationFixturesReachIdenticalLatestSchema(t *testing.T) {
	wantPath := filepath.Join(t.TempDir(), "fresh.db")
	wantStore, err := Open(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = wantStore.Close()
	want := schemaSnapshot(t, wantPath)

	for _, version := range []int{0, 4, 5, 8, 9, 10, 11, 12, 13, 14} {
		t.Run("v"+strconv.Itoa(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.db")
			if version > 0 {
				createVersionFixture(t, path, version)
			}
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			_ = st.Close()
			if got := schemaSnapshot(t, path); got != want {
				t.Fatalf("schema mismatch\nwant:\n%s\ngot:\n%s", want, got)
			}
		})
	}
}

func TestMigrationFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	createVersionFixture(t, path, 4)
	db := openRawDB(t, path)
	if _, err := db.Exec(`CREATE TABLE admin_sessions(id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	if st, err := Open(path); err == nil {
		_ = st.Close()
		t.Fatal("migration unexpectedly succeeded")
	}
	db = openRawDB(t, path)
	defer func() { _ = db.Close() }()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 4 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if !tableHasColumn(t, db, "share_links", "token") {
		t.Fatal("v4 share table was not restored")
	}
}

func TestFutureSchemaIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db := openRawDB(t, path)
	if _, err := db.Exec(`PRAGMA user_version=16`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if st, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than supported") {
		if st != nil {
			_ = st.Close()
		}
		t.Fatalf("err=%v", err)
	}
}

func TestLocationAllocatorPreventsIDReuse(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "allocator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	first, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteRange(t.TempDir(), ExportFilter{From: 1, To: 1}); err != nil {
		t.Fatal(err)
	}
	second, err := st.Insert(&Location{User: "u", Device: "d", Tst: 2, Payload: []byte(`{}`), Digest: []byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Fatalf("location id reused: first=%d second=%d", first, second)
	}
}

func TestLocationAllocatorStartsAtOne(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "allocator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 {
		t.Fatalf("first location id=%d, want 1", id)
	}
}

func TestLocationAllocatorIncludesPendingProofGC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allocator.db")
	createVersionFixture(t, path, 11)
	db := openRawDB(t, path)
	if _, err := db.Exec(`INSERT INTO proof_gc(path,created_at,updated_at) VALUES ('7.tsr',1,1),('invalid.ots',1,1)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if id != 8 {
		t.Fatalf("location id=%d, want 8", id)
	}
}

func TestLocationAllocatorRepairsExistingV12Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allocator.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO proof_gc(path,created_at,updated_at) VALUES ('7.ots',1,1)`); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := st.Insert(&Location{User: "u", Device: "d", Tst: 1, Payload: []byte(`{}`), Digest: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if id != 8 {
		t.Fatalf("location id=%d, want 8", id)
	}
}

func TestLocationRangeIndexesAreUsed(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "indexes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	tests := []struct {
		name  string
		query string
		args  []any
		index string
	}{
		{"time", `SELECT id FROM locations WHERE tst>=? ORDER BY tst,id`, []any{1}, "idx_locations_tst_id"},
		{"user", `SELECT id FROM locations WHERE user=? AND tst>=? ORDER BY tst,id`, []any{"u", 1}, "idx_locations_user_tst_id"},
		{"device", `SELECT id FROM locations WHERE device=? AND tst>=? ORDER BY tst,id`, []any{"d", 1}, "idx_locations_device_tst_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := st.db.Query(`EXPLAIN QUERY PLAN `+tt.query, tt.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var plan strings.Builder
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan.WriteString(detail)
			}
			if !strings.Contains(plan.String(), tt.index) {
				t.Fatalf("plan %q does not use %s", plan.String(), tt.index)
			}
		})
	}
}

func createVersionFixture(t *testing.T, path string, version int) {
	t.Helper()
	db := openRawDB(t, path)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version > version {
			break
		}
		if err := m.up(tx); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
}

func openRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func schemaSnapshot(t *testing.T, path string) string {
	t.Helper()
	db := openRawDB(t, path)
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`
		SELECT type || ':' || name || ':' || COALESCE(sql, '')
		FROM sqlite_schema
		WHERE name NOT LIKE 'sqlite_%'
		ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, strings.Join(strings.Fields(line), " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func tableHasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	return false
}
