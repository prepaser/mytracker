package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/store"
)

func TestShareLinkCRUD(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	created := &store.ShareLink{Label: "family", Hours: 12}
	id, err := st.CreateShare(created)
	if err != nil || id != 1 {
		t.Fatalf("create id=%d err=%v", id, err)
	}
	list, err := st.ListShares()
	if err != nil || len(list) != 1 || list[0].Token != "" {
		t.Fatalf("list: %+v err=%v", list, err)
	}
	got, err := st.GetShareByToken(created.Token)
	if err != nil || got.Label != "family" {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	if err := st.TouchShare(id); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetShareByToken(created.Token)
	if !got.LastAccessAt.Valid {
		t.Fatal("last access not set")
	}
	if err := st.DeleteShare(id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetShareByToken(created.Token); err != store.ErrShareNotFound {
		t.Fatalf("expected not found, got %v", err)
	}
	if err := st.DeleteShare(id); err != store.ErrShareNotFound {
		t.Fatalf("expected delete not found, got %v", err)
	}
}

func TestShareExpired(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	exp := time.Now().Add(-time.Hour).Unix()
	_, err = st.CreateShare(&store.ShareLink{
		Hours:     24,
		ExpiresAt: store.ShareExpiresAt(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	link := store.ShareLink{ExpiresAt: sql.NullInt64{Int64: exp, Valid: true}}
	if link.Active() {
		t.Fatal("should be expired")
	}
}

func TestRotateShareInvalidatesOldToken(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	expires := time.Now().Add(time.Hour).Unix()
	link := &store.ShareLink{Hours: 24, ExpiresAt: sql.NullInt64{Int64: expires, Valid: true}}
	id, err := st.CreateShare(link)
	if err != nil {
		t.Fatal(err)
	}
	old := link.Token
	next, err := st.RotateShare(id)
	if err != nil || next == old {
		t.Fatalf("next=%q err=%v", next, err)
	}
	if _, err := st.GetShareByToken(old); !errors.Is(err, store.ErrShareNotFound) {
		t.Fatalf("old token err=%v", err)
	}
	got, err := st.GetShareByToken(next)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.Valid || got.ExpiresAt.Int64 != expires {
		t.Fatalf("expiry=%v, want %d", got.ExpiresAt, expires)
	}
}

func TestRotateShareRejectsExpiredLink(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shares.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	old := "expired-share-token"
	id, err := st.CreateShare(&store.ShareLink{
		Token: old, Hours: 24,
		ExpiresAt: sql.NullInt64{Int64: time.Now().Add(-time.Hour).Unix(), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RotateShare(id); !errors.Is(err, store.ErrShareExpired) {
		t.Fatalf("rotate err=%v", err)
	}
	link, err := st.GetShareByToken(old)
	if err != nil {
		t.Fatalf("old token err=%v", err)
	}
	if link.ExpiresAt.Int64 >= time.Now().Unix() {
		t.Fatalf("expired link expiry=%d", link.ExpiresAt.Int64)
	}
}
