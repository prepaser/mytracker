package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

var ErrShareNotFound = errors.New("share link not found")
var ErrShareExpired = errors.New("share link expired")

const shareAccessTouchInterval = 5 * time.Minute

type ShareLink struct {
	ID           int64
	Token        string
	Label        string
	User         string
	Device       string
	Hours        int
	ExpiresAt    sql.NullInt64
	CreatedAt    int64
	LastAccessAt sql.NullInt64
	DelayMinutes int
	PrecisionM   int
}

func NewShareToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Store) CreateShare(link *ShareLink) (int64, error) {
	if link.Token == "" {
		tok, err := NewShareToken()
		if err != nil {
			return 0, err
		}
		link.Token = tok
	}
	if link.Hours <= 0 {
		link.Hours = 24
	}
	if link.CreatedAt == 0 {
		link.CreatedAt = time.Now().Unix()
	}
	res, err := s.db.Exec(`
		INSERT INTO share_links (token_hash, label, user, device, hours, expires_at, created_at, delay_minutes, precision_m)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tokenHash(link.Token), link.Label, link.User, link.Device, link.Hours, link.ExpiresAt, link.CreatedAt,
		link.DelayMinutes, link.PrecisionM,
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err == nil {
		s.publish(Change{Topic: ChangeShares})
	}
	return id, err
}

func (s *Store) ListShares() ([]ShareLink, error) {
	rows, err := s.db.Query(`
		SELECT id,label,user,device,hours,expires_at,created_at,last_access_at,delay_minutes,precision_m
		FROM share_links ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ShareLink
	for rows.Next() {
		link, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *link)
	}
	return out, rows.Err()
}

func (s *Store) GetShareByToken(token string) (*ShareLink, error) {
	row := s.db.QueryRow(`
		SELECT id,label,user,device,hours,expires_at,created_at,last_access_at,delay_minutes,precision_m
		FROM share_links WHERE token_hash=?`, tokenHash(token))
	link, err := scanShare(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrShareNotFound
	}
	return link, err
}

func (s *Store) RotateShare(id int64) (string, error) {
	token, err := NewShareToken()
	if err != nil {
		return "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var expires sql.NullInt64
	if err := tx.QueryRow(`SELECT expires_at FROM share_links WHERE id=?`, id).Scan(&expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrShareNotFound
		}
		return "", err
	}
	now := time.Now().Unix()
	if expires.Valid && expires.Int64 <= now {
		return "", ErrShareExpired
	}
	res, err := tx.Exec(`UPDATE share_links SET token_hash=?,last_access_at=NULL
		WHERE id=? AND (expires_at IS NULL OR expires_at>?)`, tokenHash(token), id, now)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", ErrShareExpired
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	s.clearShareTouch(id)
	s.publish(Change{Topic: ChangeShares})
	return token, nil
}

func (s *Store) DeleteShare(id int64) error {
	res, err := s.db.Exec(`DELETE FROM share_links WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrShareNotFound
	}
	s.clearShareTouch(id)
	s.publish(Change{Topic: ChangeShares})
	return nil
}

func (s *Store) TouchShare(id int64) error {
	now := time.Now().Unix()
	cutoff := now - int64(shareAccessTouchInterval/time.Second)
	s.shareTouchMu.Lock()
	defer s.shareTouchMu.Unlock()
	if s.shareTouches[id] > cutoff {
		return nil
	}
	_, err := s.db.Exec(`
		UPDATE share_links
		SET last_access_at=?
		WHERE id=? AND (last_access_at IS NULL OR last_access_at <= ?)`, now, id, cutoff)
	if err == nil {
		s.shareTouches[id] = now
	}
	return err
}

func (s *Store) clearShareTouch(id int64) {
	s.shareTouchMu.Lock()
	delete(s.shareTouches, id)
	s.shareTouchMu.Unlock()
}

func (link *ShareLink) Active() bool {
	if !link.ExpiresAt.Valid {
		return true
	}
	return link.ExpiresAt.Int64 > time.Now().Unix()
}

func scanShare(row scannable) (*ShareLink, error) {
	var link ShareLink
	err := row.Scan(&link.ID, &link.Label, &link.User, &link.Device,
		&link.Hours, &link.ExpiresAt, &link.CreatedAt, &link.LastAccessAt,
		&link.DelayMinutes, &link.PrecisionM)
	if err != nil {
		return nil, err
	}
	return &link, nil
}

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func ShareExpiresAt(days int) sql.NullInt64 {
	if days <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix(), Valid: true}
}

func FormatShareExpiry(exp sql.NullInt64) string {
	if !exp.Valid {
		return "never"
	}
	return time.Unix(exp.Int64, 0).UTC().Format("2006-01-02 15:04")
}

func (link ShareLink) FilterDesc() string {
	if link.User == "" && link.Device == "" {
		return "all devices"
	}
	if link.Device == "" {
		return fmt.Sprintf("user %s", link.User)
	}
	if link.User == "" {
		return fmt.Sprintf("device %s", link.Device)
	}
	return fmt.Sprintf("%s / %s", link.User, link.Device)
}
