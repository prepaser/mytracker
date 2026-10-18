package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/prepaser/mytracker/internal/password"
)

type SettingsStore interface {
	LoadSettings() ([]byte, error)
	SaveSettings([]byte, bool) error
	InitializeSettings(func() ([]byte, error)) ([]byte, bool, error)
}

type Manager struct {
	mu    sync.RWMutex
	cfg   *Config
	store SettingsStore
}

var (
	ErrRevisionConflict = errors.New("settings changed since page loaded")
	ErrInvalidSettings  = errors.New("invalid settings")
)

func NewManager(cfg *Config, store SettingsStore) *Manager {
	return &Manager{cfg: cfg.Clone(), store: store}
}

func (c *Config) ApplySettings(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	next := c.Clone()
	next.Settings = Default().Settings
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next.Settings); err != nil {
		return fmt.Errorf("invalid stored settings")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("invalid stored settings")
	}
	if err := next.Normalize(); err != nil {
		return fmt.Errorf("invalid stored settings: %w", err)
	}
	if next.AdminHash == "" || next.PubUser == "" || next.PubPass == "" {
		return fmt.Errorf("stored credentials are missing")
	}
	*c = *next
	return nil
}

func (m *Manager) Initialize() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := bootstrapOutsideData(m.cfg.DataDir); err != nil {
		return false, err
	}
	data, created, err := m.store.InitializeSettings(func() ([]byte, error) {
		next := m.cfg.Clone()
		initial, err := prepareInitialPassword(m.cfg.DataDir)
		if err != nil {
			return nil, err
		}
		pass := string(initial[:len(initial)-1])
		next.AdminHash, err = password.Hash([]byte(pass))
		if err != nil {
			return nil, err
		}
		next.PubPass, err = RandToken(16)
		if err != nil {
			return nil, err
		}
		if err := next.Normalize(); err != nil {
			return nil, err
		}
		data, err := json.Marshal(next.Settings)
		if err != nil {
			return nil, err
		}
		return data, nil
	})
	if err != nil {
		return false, err
	}
	if err := m.cfg.ApplySettings(data); err != nil {
		return created, err
	}
	if err := removePendingInitialPassword(m.cfg.DataDir); err != nil {
		return created, err
	}
	if !created {
		return false, relocateInitialPassword(m.cfg.DataDir)
	}
	return true, nil
}

func (m *Manager) Snapshot() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.Clone()
}

func (m *Manager) SnapshotWithRevision() (*Config, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.Clone(), m.cfg.SettingsRevision()
}

func (m *Manager) InitialPasswordPath() string {
	return InitialPasswordPath()
}

func (m *Manager) RemoveInitialPassword() error {
	return RemoveInitialPassword(m.Snapshot().DataDir)
}

func (m *Manager) Update(fn func(*Settings) error) (*Config, error) {
	return m.update(fn, false)
}

func (m *Manager) UpdateIfRevision(revision string, fn func(*Settings) error) (*Config, error) {
	return m.updateIfRevision(revision, fn, false)
}

func (m *Manager) UpdateWithSessionRevocation(fn func(*Settings) error) (*Config, error) {
	return m.update(fn, true)
}

func (m *Manager) UpdateWithSessionRevocationIfRevision(revision string, fn func(*Settings) error) (*Config, error) {
	return m.updateIfRevision(revision, fn, true)
}

func (m *Manager) update(fn func(*Settings) error, revoke bool) (*Config, error) {
	return m.updateIfRevision("", fn, revoke)
}

func (m *Manager) updateIfRevision(revision string, fn func(*Settings) error, revoke bool) (*Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if revision != "" && revision != m.cfg.SettingsRevision() {
		return nil, ErrRevisionConflict
	}
	next := m.cfg.Clone()
	if err := fn(&next.Settings); err != nil {
		return nil, err
	}
	if err := next.Normalize(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidSettings, err)
	}
	data, err := json.Marshal(next.Settings)
	if err != nil {
		return nil, err
	}
	if err := m.store.SaveSettings(data, revoke); err != nil {
		return nil, err
	}
	m.cfg = next
	return next.Clone(), nil
}

func (c *Config) SettingsRevision() string {
	return settingsRevision(c.Settings)
}

func settingsRevision(settings Settings) string {
	b, _ := json.Marshal(settings)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])
}
