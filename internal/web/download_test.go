package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDownloadProgressOutlivesServerTimeout(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		name := "http1"
		if http2 {
			name = "http2"
		}
		t.Run(name, func(t *testing.T) {
			finished := make(chan error, 1)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				download, err := newDownloadWriter(w, r.Context(), 200*time.Millisecond)
				if err != nil {
					finished <- err
					return
				}
				time.Sleep(100 * time.Millisecond)
				for range 10 {
					if _, err = download.Write([]byte("part\n")); err != nil {
						break
					}
					if err = http.NewResponseController(download).Flush(); err != nil {
						break
					}
					time.Sleep(40 * time.Millisecond)
				}
				finished <- err
			}))
			server.Config.WriteTimeout = 50 * time.Millisecond
			server.EnableHTTP2 = http2
			if http2 {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			client := server.Client()
			client.Timeout = 3 * time.Second
			response, err := client.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			if err != nil || !bytes.Equal(body, bytes.Repeat([]byte("part\n"), 10)) {
				t.Fatalf("body=%q err=%v", body, err)
			}
			if http2 && response.ProtoMajor != 2 {
				t.Fatalf("protocol=%s", response.Proto)
			}
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAcknowledgeDownload(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		secure   bool
		want     bool
	}{
		{name: "valid", id: "1-22-333", want: true},
		{name: "secure", id: "9-8-7", secure: true, want: true},
		{name: "empty"},
		{name: "invalid", id: "abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/download?download_id="+tc.id, nil)
			out := httptest.NewRecorder()
			acknowledgeDownload(out, req, tc.secure)
			cookies := out.Result().Cookies()
			if len(cookies) == 0 {
				if tc.want {
					t.Fatal("download was not acknowledged")
				}
				return
			}
			if !tc.want || len(cookies) != 1 {
				t.Fatalf("cookies=%+v", cookies)
			}
			cookie := cookies[0]
			if cookie.Name != "mytracker-download-"+tc.id || cookie.Value != "1" || cookie.Path != "/" ||
				cookie.MaxAge != 3600 || cookie.Secure != tc.secure || cookie.SameSite != http.SameSiteStrictMode {
				t.Fatalf("cookie=%+v", cookie)
			}
		})
	}
}

func TestDownloadStalledClientTimesOut(t *testing.T) {
	finished := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		download, err := newDownloadWriter(w, r.Context(), 100*time.Millisecond)
		if err == nil {
			_, err = download.Write(make([]byte, 16<<20))
		}
		finished <- err
	}))
	defer server.Close()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("stalled write error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stalled download did not release the handler")
	}
}

func TestDownloadCanceledRequestStopsWriting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	recorder := httptest.NewRecorder()
	w, err := newDownloadWriter(recorder, ctx, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if n, err := w.Write([]byte("canceled")); n != 0 || !errors.Is(err, context.Canceled) || recorder.Body.Len() != 0 {
		t.Fatalf("n=%d err=%v body=%q", n, err, recorder.Body.String())
	}
}
