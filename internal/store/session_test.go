package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/store"
)

func TestSessionLifecycle(t *testing.T) {
	st := openTestDB(t)
	now := time.Now().Unix()
	id, err := st.CreateSession("secret-session-token", now, now+3600)
	if err != nil {
		t.Fatal(err)
	}
	session, err := st.GetSession("secret-session-token", now, 600)
	if err != nil || session.ID != id {
		t.Fatalf("session=%+v err=%v", session, err)
	}
	if _, err := st.GetSession("wrong-token", now, 600); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("wrong token err=%v", err)
	}
	if err := st.TouchSession(id, now+60); err != nil {
		t.Fatal(err)
	}
	sessions, err := st.ListSessions(now+60, 600)
	if err != nil || len(sessions) != 1 || sessions[0].LastSeenAt != now+60 {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	if err := st.DeleteSessionToken("secret-session-token"); err != nil {
		t.Fatal(err)
	}
	if count, _ := st.SessionCount(); count != 0 {
		t.Fatalf("count=%d", count)
	}
}

func TestSessionCreationRejectsChangedPassword(t *testing.T) {
	st := openTestDB(t)
	if err := st.SaveSettings([]byte(`{"admin_hash":"old"}`), false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSessionForPassword("before", "old", 100, 200); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSettings([]byte(`{"admin_hash":"new"}`), true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSessionForPassword("stale", "old", 101, 201); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("stale login: %v", err)
	}
	if _, err := st.CreateSessionForPassword("after", "new", 101, 201); err != nil {
		t.Fatal(err)
	}
	sessions, err := st.ListSessions(102, 100)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions=%v err=%v", sessions, err)
	}
}

func TestSessionTouchPreservesLatestActivity(t *testing.T) {
	st := openTestDB(t)
	id, err := st.CreateSession("session", 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, seen := range []int64{110, 105, 110} {
		if err := st.TouchSession(id, seen); err != nil {
			t.Fatal(err)
		}
	}
	session, err := st.GetSession("session", 114, 5)
	if err != nil || session.LastSeenAt != 110 {
		t.Fatalf("session=%+v err=%v", session, err)
	}
	if _, err := st.GetSession("session", 115, 5); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("idle session err=%v", err)
	}
}

func TestExpiredAndIdleSessionsAreCleaned(t *testing.T) {
	st := openTestDB(t)
	now := time.Now().Unix()
	_, _ = st.CreateSession("expired", now-1000, now-1)
	_, _ = st.CreateSession("idle", now-1000, now+1000)
	_, _ = st.CreateSession("active", now, now+1000)
	n, err := st.CleanupSessions(now, 600)
	if err != nil || n != 2 {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
}

func TestSessionRevocationScopes(t *testing.T) {
	st := openTestDB(t)
	now := time.Now().Unix()
	first, _ := st.CreateSession("first-session", now, now+1000)
	second, _ := st.CreateSession("second-session", now, now+1000)
	if err := st.DeleteSession(second); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSession(second); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("missing session err=%v", err)
	}
	_, _ = st.CreateSession("third-session", now, now+1000)
	if err := st.DeleteOtherSessions(first); err != nil {
		t.Fatal(err)
	}
	if count, _ := st.SessionCount(); count != 1 {
		t.Fatalf("count=%d", count)
	}
	if err := st.DeleteAllSessions(); err != nil {
		t.Fatal(err)
	}
}
