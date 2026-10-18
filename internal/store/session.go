package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
)

var ErrSessionNotFound = errors.New("session not found")

type Session struct {
	ID         int64 `json:"id"`
	CreatedAt  int64 `json:"created_at"`
	LastSeenAt int64 `json:"last_seen_at"`
	ExpiresAt  int64 `json:"expires_at"`
}

func sessionHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func (s *Store) CreateSession(token string, now, expires int64) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO admin_sessions(token_hash,created_at,last_seen_at,expires_at)
		VALUES(?,?,?,?)`, sessionHash(token), now, now, expires)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) CreateSessionForPassword(token, expectedHash string, now, expires int64) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO admin_sessions(token_hash,created_at,last_seen_at,expires_at)
		SELECT ?,?,?,? FROM app_settings
		WHERE id=1 AND json_extract(json,'$.admin_hash')=?`,
		sessionHash(token), now, now, expires, expectedHash)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, ErrSessionNotFound
	}
	return res.LastInsertId()
}

func (s *Store) GetSession(token string, now, idleSeconds int64) (*Session, error) {
	var out Session
	err := s.db.QueryRow(`
		SELECT id,created_at,last_seen_at,expires_at FROM admin_sessions
		WHERE token_hash=? AND expires_at>? AND last_seen_at>?`,
		sessionHash(token), now, now-idleSeconds,
	).Scan(&out.ID, &out.CreatedAt, &out.LastSeenAt, &out.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	return &out, err
}

func (s *Store) TouchSession(id, now int64) error {
	_, err := s.db.Exec(`UPDATE admin_sessions SET last_seen_at=MAX(last_seen_at,?) WHERE id=?`, now, id)
	return err
}

func (s *Store) ListSessions(now, idleSeconds int64) ([]Session, error) {
	rows, err := s.db.Query(`
		SELECT id,created_at,last_seen_at,expires_at FROM admin_sessions
		WHERE expires_at>? AND last_seen_at>? ORDER BY last_seen_at DESC`, now, now-idleSeconds)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Session
	for rows.Next() {
		var session Session
		if err := rows.Scan(&session.ID, &session.CreatedAt, &session.LastSeenAt, &session.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSession(id int64) error {
	res, err := s.db.Exec(`DELETE FROM admin_sessions WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrSessionNotFound
	}
	return nil
}

func (s *Store) DeleteSessionToken(token string) error {
	_, err := s.db.Exec(`DELETE FROM admin_sessions WHERE token_hash=?`, sessionHash(token))
	return err
}

func (s *Store) DeleteOtherSessions(id int64) error {
	_, err := s.db.Exec(`DELETE FROM admin_sessions WHERE id<>?`, id)
	return err
}

func (s *Store) DeleteAllSessions() error {
	_, err := s.db.Exec(`DELETE FROM admin_sessions`)
	return err
}

func (s *Store) CleanupSessions(now, idleSeconds int64) (int64, error) {
	return s.CleanupSessionsContext(context.Background(), now, idleSeconds)
}

func (s *Store) CleanupSessionsContext(ctx context.Context, now, idleSeconds int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at<=? OR last_seen_at<=?`,
		now, now-idleSeconds)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) SessionCount() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM admin_sessions`).Scan(&count)
	return count, err
}
