package store

import (
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"time"
)

func (s *Store) LoadSettings() ([]byte, error) {
	var data []byte
	err := s.db.QueryRow(`SELECT json FROM app_settings WHERE id=1`).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return data, err
}

func (s *Store) SaveSettings(data []byte, revokeSessions bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := persistSettings(tx, data); err != nil {
		return err
	}
	if revokeSessions {
		if _, err := tx.Exec(`DELETE FROM admin_sessions`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) InitializeSettings(makeSettings func() ([]byte, error)) ([]byte, bool, error) {
	if makeSettings == nil {
		return nil, false, errors.New("settings initializer is nil")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE app_settings SET updated_at=updated_at WHERE id=1`); err != nil {
		return nil, false, err
	}
	var data []byte
	err = tx.QueryRow(`SELECT json FROM app_settings WHERE id=1`).Scan(&data)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return data, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	data, err = makeSettings()
	if err != nil {
		return nil, false, err
	}
	if err := persistSettings(tx, data); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`DELETE FROM admin_sessions`); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func LoadSettingsReadOnly(path string) ([]byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	var exists int
	err = db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='table' AND name='app_settings'`).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var data []byte
	err = db.QueryRow(`SELECT json FROM app_settings WHERE id=1`).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return data, err
}

func persistSettings(tx *sql.Tx, data []byte) error {
	_, err := tx.Exec(`
		INSERT INTO app_settings(id,json,updated_at) VALUES(1,?,?)
		ON CONFLICT(id) DO UPDATE SET json=excluded.json, updated_at=excluded.updated_at`,
		string(data), time.Now().Unix())
	return err
}
