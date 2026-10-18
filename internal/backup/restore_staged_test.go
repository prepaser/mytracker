package backup

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/prepaser/mytracker/internal/config"
	_ "modernc.org/sqlite"
)

func TestPrepareStagedDatabaseDoesNotMigrateLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mytracker.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		PRAGMA user_version=5;
		CREATE TABLE app_settings (id INTEGER PRIMARY KEY, json TEXT NOT NULL, updated_at INTEGER NOT NULL);
		CREATE TABLE admin_sessions (id INTEGER PRIMARY KEY);
		INSERT INTO admin_sessions(id) VALUES(1);`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := prepareStagedDatabase(path, config.Default()); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 5 {
		t.Fatalf("schema version changed during restore: %d", version)
	}
	var sessions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM admin_sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("sessions=%d", sessions)
	}
	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='locations'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("restore migrated the legacy database")
	}
}
