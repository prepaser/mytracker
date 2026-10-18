package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/prepaser/mytracker/internal/slogx"
)

var client = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func Notify(ctx context.Context, url, secret, event string, data map[string]any) {
	if err := Send(ctx, url, secret, event, data); err != nil {
		slog.Warn("webhook request failed", "err", slogx.SafeError(err))
	}
}

func Send(ctx context.Context, url, secret, event string, data map[string]any) error {
	if url == "" {
		return nil
	}
	body, err := json.Marshal(map[string]any{"event": event, "data": data})
	if err != nil {
		slog.Error("webhook marshal", "err", err)
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 256))
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	}
	return nil
}
