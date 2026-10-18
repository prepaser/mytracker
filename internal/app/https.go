package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"golang.org/x/crypto/acme/autocert"
)

type httpsSetup struct {
	tlsConfig  *tls.Config
	httpServer *http.Server
	mode       string
}

func configureHTTPS(cfg *config.Config) (*httpsSetup, error) {
	if cfg.TLSCertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS certificate: %w", err)
		}
		return &httpsSetup{
			tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}},
			mode:      "manual",
		}, nil
	}
	if len(cfg.AutoHTTPSDomains) == 0 {
		return &httpsSetup{mode: "http"}, nil
	}
	mgr := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(cfg.ACMECacheDir()),
		HostPolicy: autoHTTPSHostPolicy(cfg.AutoHTTPSDomains),
	}
	tlsConfig := mgr.TLSConfig()
	tlsConfig.MinVersion = tls.VersionTLS12
	setup := &httpsSetup{tlsConfig: tlsConfig, mode: "automatic"}
	if cfg.ListenAddr != "" {
		setup.httpServer = newHTTPRedirectServer(cfg, mgr)
	}
	return setup, nil
}

func autoHTTPSHostPolicy(domains []string) autocert.HostPolicy {
	allowed := make(map[string]bool, len(domains))
	for _, domain := range domains {
		allowed[strings.ToLower(domain)] = true
	}
	return func(_ context.Context, raw string) error {
		host := raw
		if parsed, _, err := net.SplitHostPort(raw); err == nil {
			host = parsed
		}
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		if !allowed[host] {
			return errors.New("ACME host is not allowed")
		}
		return nil
	}
}

func newHTTPRedirectServer(cfg *config.Config, mgr *autocert.Manager) *http.Server {
	allowed := make(map[string]bool, len(cfg.AutoHTTPSDomains))
	for _, domain := range cfg.AutoHTTPSDomains {
		allowed[domain] = true
	}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if u, err := url.Parse("//" + host); err == nil {
			host = u.Hostname()
		}
		if !allowed[host] || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			http.NotFound(w, r)
			return
		}
		_, port, _ := net.SplitHostPort(cfg.HTTPSListenAddr)
		if port != "" && port != "443" {
			host = net.JoinHostPort(host, port)
		}
		if cfg.PublicURL != "" {
			u, _ := url.Parse(cfg.PublicURL)
			host = u.Host
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mgr.HTTPHandler(fallback),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}
