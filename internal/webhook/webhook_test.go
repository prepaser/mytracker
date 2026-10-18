package webhook_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prepaser/mytracker/internal/webhook"
)

func TestNotifyOK(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	webhook.Notify(context.Background(), srv.URL, "", "stamp_fail", map[string]any{"id": 1})
	if got == "" {
		t.Fatal("expected body")
	}
}

func TestNotifyBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("oops"))
	}))
	defer srv.Close()
	webhook.Notify(context.Background(), srv.URL, "", "stamp_fail", map[string]any{"id": 1})
}

func TestNotifySignatureAndFailures(t *testing.T) {
	var signature string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		signature = r.Header.Get("X-Signature")
	}))
	defer srv.Close()
	webhook.Notify(context.Background(), srv.URL, "secret", "stamp_fail", map[string]any{"id": 1})
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	if signature != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signature=%q", signature)
	}
	webhook.Notify(context.Background(), "", "", "ignored", nil)
	webhook.Notify(context.Background(), "://bad", "", "ignored", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	webhook.Notify(ctx, srv.URL, "", "ignored", nil)
}
