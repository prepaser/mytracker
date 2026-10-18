package store

import (
	"bytes"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSaveSettingsAndRevokeSessionsAreAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	old := []byte(`{"name":"old"}`)
	if err := st.SaveSettings(old, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSession("first", 1, 100); err != nil {
		t.Fatal(err)
	}
	updated := []byte(`{"name":"updated"}`)
	if err := st.SaveSettings(updated, true); err != nil {
		t.Fatal(err)
	}
	data, err := st.LoadSettings()
	if err != nil || !bytes.Equal(data, updated) {
		t.Fatalf("settings=%s err=%v", data, err)
	}
	data, err = LoadSettingsReadOnly(path)
	if err != nil || !bytes.Equal(data, updated) {
		t.Fatalf("read-only settings=%s err=%v", data, err)
	}
	if count, err := st.SessionCount(); err != nil || count != 0 {
		t.Fatalf("session count=%d err=%v", count, err)
	}

	if _, err := st.CreateSession("second", 1, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TRIGGER reject_session_delete BEFORE DELETE ON admin_sessions BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSettings([]byte(`{"name":"rejected"}`), true); err == nil {
		t.Fatal("save succeeded despite session deletion failure")
	}
	data, err = st.LoadSettings()
	if err != nil || !bytes.Equal(data, updated) {
		t.Fatalf("settings after rollback=%s err=%v", data, err)
	}
	if count, err := st.SessionCount(); err != nil || count != 1 {
		t.Fatalf("session count after rollback=%d err=%v", count, err)
	}
}

func TestInitializeSettingsSerializesConcurrentInitialization(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.CreateSession("legacy", 1, 100); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	duplicate := make(chan struct{}, 1)
	var calls atomic.Int32
	makeSettings := func() ([]byte, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return []byte(`{"initialized":true}`), nil
		}
		duplicate <- struct{}{}
		return []byte(`{"initialized":false}`), nil
	}
	type result struct {
		data []byte
		init bool
		err  error
	}
	results := make(chan result, 2)
	initialize := func() {
		data, init, err := st.InitializeSettings(makeSettings)
		results <- result{data: data, init: init, err: err}
	}
	go initialize()
	<-entered
	go initialize()

	duplicateCalled := false
	select {
	case <-duplicate:
		duplicateCalled = true
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	first, second := <-results, <-results
	if duplicateCalled || calls.Load() != 1 {
		t.Fatalf("initializer called %d times", calls.Load())
	}
	if first.err != nil || second.err != nil {
		t.Fatalf("initialization errors: %v, %v", first.err, second.err)
	}
	if first.init == second.init {
		t.Fatalf("initialized flags are %v and %v, want one true", first.init, second.init)
	}
	want := []byte(`{"initialized":true}`)
	if !bytes.Equal(first.data, want) || !bytes.Equal(second.data, want) {
		t.Fatalf("initialized settings differ: %s / %s", first.data, second.data)
	}
	if count, err := st.SessionCount(); err != nil || count != 0 {
		t.Fatalf("session count=%d err=%v", count, err)
	}
	if _, err := st.CreateSession("later", 1, 100); err != nil {
		t.Fatal(err)
	}
	data, initialized, err := st.InitializeSettings(func() ([]byte, error) {
		return nil, errors.New("initializer called for existing settings")
	})
	if err != nil || initialized || !bytes.Equal(data, want) {
		t.Fatalf("repeat initialization: settings=%s initialized=%t err=%v", data, initialized, err)
	}
	if count, err := st.SessionCount(); err != nil || count != 1 {
		t.Fatalf("session count after repeat initialization=%d err=%v", count, err)
	}
}

func TestLoadSettingsReadOnlyDoesNotMigrateLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createVersionFixture(t, path, 13)

	data, err := LoadSettingsReadOnly(path)
	if err != nil || data != nil {
		t.Fatalf("settings=%s err=%v", data, err)
	}
	db := openRawDB(t, path)
	defer func() { _ = db.Close() }()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 13 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='app_settings'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("app_settings tables=%d err=%v", count, err)
	}
}
