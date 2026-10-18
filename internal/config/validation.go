package config

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/prepaser/mytracker/internal/verification"
)

func (c *Config) Validate() error {
	if err := validListenAddr("listen_addr", c.ListenAddr, c.TLSConfigured()); err != nil {
		return err
	}
	if err := validListenAddr("https_listen_addr", c.HTTPSListenAddr, !c.TLSConfigured()); err != nil {
		return err
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return fmt.Errorf("tls_cert_file and tls_key_file must be set together")
	}
	if len(c.AutoHTTPSDomains) > 10 {
		return fmt.Errorf("auto_https_domains must contain at most 10 domains")
	}
	if len(c.AutoHTTPSDomains) > 0 && c.TLSCertFile != "" {
		return fmt.Errorf("automatic HTTPS and TLS certificate files are mutually exclusive")
	}
	if c.TLSConfigured() && c.ListenAddr != "" && c.HTTPSListenAddr == c.ListenAddr {
		return fmt.Errorf("https_listen_addr and listen_addr must differ")
	}
	seenDomains := make(map[string]bool, len(c.AutoHTTPSDomains))
	for _, domain := range c.AutoHTTPSDomains {
		if !validAutoHTTPSDomain(domain) {
			return fmt.Errorf("auto_https_domains contains invalid domain %q", domain)
		}
		if seenDomains[domain] {
			return fmt.Errorf("auto_https_domains contains duplicate domain")
		}
		seenDomains[domain] = true
	}
	if c.PublicURL != "" {
		clean, err := cleanHTTPURL(c.PublicURL, false)
		if err != nil {
			return fmt.Errorf("public_url: %w", err)
		}
		if len(c.AutoHTTPSDomains) > 0 {
			u, _ := url.Parse(clean)
			if u.Scheme != "https" || !seenDomains[strings.ToLower(u.Hostname())] {
				return fmt.Errorf("public_url must use HTTPS and an auto_https_domains host")
			}
		} else if c.TLSCertFile != "" {
			u, _ := url.Parse(clean)
			if u.Scheme != "https" {
				return fmt.Errorf("public_url must use HTTPS when TLS is enabled")
			}
		}
	}
	if _, err := cleanHTTPURL(c.TSAURL, false); err != nil {
		return fmt.Errorf("tsa_url: %w", err)
	}
	for _, cal := range c.OTSCalendars {
		if _, err := cleanHTTPURL(cal, true); err != nil {
			return fmt.Errorf("ots_calendar: %w", err)
		}
	}
	if c.LoginRateLimit < 1 || c.LoginRateLimit > 1_000_000 {
		return fmt.Errorf("login_rate_limit must be between 1 and 1000000")
	}
	if d, err := c.LoginRateDuration(); err != nil || d <= 0 || d > 24*time.Hour {
		if err == nil {
			err = fmt.Errorf("must be between 1ns and 24h")
		}
		return fmt.Errorf("login_rate_window: %w", err)
	}
	if c.PubRateLimit < 1 || c.PubRateLimit > 1_000_000 {
		return fmt.Errorf("pub_rate_limit must be between 1 and 1000000")
	}
	if d, err := c.PubRateDuration(); err != nil || d <= 0 || d > 24*time.Hour {
		if err == nil {
			err = fmt.Errorf("must be between 1ns and 24h")
		}
		return fmt.Errorf("pub_rate_window: %w", err)
	}
	if c.ShareRateLimit < 1 || c.ShareRateLimit > 1_000_000 {
		return fmt.Errorf("share_rate_limit must be between 1 and 1000000")
	}
	if d, err := c.ShareRateDuration(); err != nil || d <= 0 || d > 24*time.Hour {
		if err == nil {
			err = fmt.Errorf("must be between 1ns and 24h")
		}
		return fmt.Errorf("share_rate_window: %w", err)
	}
	if c.StampWorkers < 1 || c.StampWorkers > 64 {
		return fmt.Errorf("stamp_workers must be between 1 and 64")
	}
	if c.StampRetries < 1 || c.StampRetries > 10 {
		return fmt.Errorf("stamp_retries must be between 1 and 10")
	}
	if c.RetentionDays < 0 || c.RetentionDays > 106751 {
		return fmt.Errorf("retention_days must be between 0 and 106751")
	}
	if rb := c.RetentionBy; rb != "" && rb != "tst" && rb != "received" {
		return fmt.Errorf("retention_by must be tst or received")
	}
	if c.WebhookURL != "" {
		if _, err := cleanHTTPURL(c.WebhookURL, true); err != nil {
			return fmt.Errorf("webhook_url: %w", err)
		}
	}
	if c.MQTTEnabled && c.MQTTBroker == "" {
		return fmt.Errorf("mqtt_broker required when mqtt_enabled")
	}
	if c.PubUser != "" && !validBasicUsername(c.PubUser) {
		return fmt.Errorf("pub_user must not contain ':' or control characters")
	}
	if len(c.EsploraURLs) < 1 || len(c.EsploraURLs) > 5 {
		return fmt.Errorf("esplora_urls must contain between 1 and 5 URLs")
	}
	if c.OTSConfirmations < 1 || c.OTSConfirmations > 144 {
		return fmt.Errorf("ots_min_confirmations must be between 1 and 144")
	}
	if len(c.TSAPolicyOIDs) > 16 {
		return fmt.Errorf("tsa_policy_oids must contain at most 16 OIDs")
	}
	seenPolicies := make(map[string]bool)
	for _, oid := range c.TSAPolicyOIDs {
		if !verification.ValidPolicyOID(oid) {
			return fmt.Errorf("tsa_policy_oids contains invalid OID %q", oid)
		}
		if seenPolicies[oid] {
			return fmt.Errorf("tsa_policy_oids contains duplicate OID")
		}
		seenPolicies[oid] = true
	}
	seenEsplora := make(map[string]bool)
	for _, raw := range c.EsploraURLs {
		u, err := cleanProviderURL(raw)
		if err != nil {
			return fmt.Errorf("esplora_urls: %w", err)
		}
		if seenEsplora[u] {
			return fmt.Errorf("esplora_urls contains duplicate URL")
		}
		seenEsplora[u] = true
	}
	if c.SessionDays < 1 || c.SessionDays > 365 {
		return fmt.Errorf("session_days must be between 1 and 365")
	}
	if d, err := time.ParseDuration(c.SessionIdle); err != nil || d < 2*time.Second || d > c.SessionTTL() {
		if err == nil {
			err = fmt.Errorf("must be at least 2s and no longer than session_days")
		}
		return fmt.Errorf("session_idle: %w", err)
	}
	if c.RateLimitMaxIPs < 100 || c.RateLimitMaxIPs > 1_000_000 {
		return fmt.Errorf("rate_limit_max_ips must be between 100 and 1000000")
	}
	if c.TileURL != "" && !strings.HasPrefix(c.TileURL, "http://") && !strings.HasPrefix(c.TileURL, "https://") {
		return fmt.Errorf("tile_url: scheme must be http or https")
	}
	if d, err := c.BackupDuration(); err != nil || d <= 0 || d > 30*24*time.Hour {
		if err == nil {
			err = fmt.Errorf("must be between 1ns and 720h")
		}
		return fmt.Errorf("backup_interval: %w", err)
	}
	if c.BackupKeep < 1 || c.BackupKeep > 365 {
		return fmt.Errorf("backup_keep must be between 1 and 365")
	}
	if c.ImportMaxBytes < 1<<20 || c.ImportMaxBytes > 1<<30 {
		return fmt.Errorf("import_max_bytes must be between 1MiB and 1GiB")
	}
	if c.StayRadiusM < 25 || c.StayRadiusM > 100000 {
		return fmt.Errorf("stay_radius_m must be between 25 and 100000")
	}
	if d, err := time.ParseDuration(c.StayMinDuration); err != nil || d < time.Minute || d > 24*time.Hour {
		return fmt.Errorf("stay_min_duration must be between 1m and 24h")
	}
	if d, err := time.ParseDuration(c.TripGap); err != nil || d < time.Minute || d > 7*24*time.Hour {
		return fmt.Errorf("trip_gap must be between 1m and 168h")
	}
	return nil
}

func validBasicUsername(value string) bool {
	for _, r := range value {
		if r == ':' || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (c *Config) Normalize() error {
	c.ListenAddr = strings.TrimSpace(c.ListenAddr)
	c.HTTPSListenAddr = strings.TrimSpace(c.HTTPSListenAddr)
	c.TLSCertFile = strings.TrimSpace(c.TLSCertFile)
	c.TLSKeyFile = strings.TrimSpace(c.TLSKeyFile)
	for i := range c.AutoHTTPSDomains {
		c.AutoHTTPSDomains[i] = strings.ToLower(strings.TrimSpace(c.AutoHTTPSDomains[i]))
	}
	if c.PublicURL != "" {
		u, err := cleanHTTPURL(c.PublicURL, false)
		if err != nil {
			return fmt.Errorf("public_url: %w", err)
		}
		c.PublicURL = u
	}
	u, err := cleanHTTPURL(c.TSAURL, true)
	if err != nil {
		return fmt.Errorf("tsa_url: %w", err)
	}
	c.TSAURL = u
	for i, cal := range c.OTSCalendars {
		u, err := cleanHTTPURL(cal, true)
		if err != nil {
			return fmt.Errorf("ots_calendar: %w", err)
		}
		c.OTSCalendars[i] = u
	}
	if c.TileURL != "" {
		c.TileURL = strings.TrimSpace(c.TileURL)
	}
	if c.WebhookURL != "" {
		u, err := cleanHTTPURL(c.WebhookURL, true)
		if err != nil {
			return fmt.Errorf("webhook_url: %w", err)
		}
		c.WebhookURL = u
	}
	for i := range c.TSAPolicyOIDs {
		c.TSAPolicyOIDs[i] = strings.TrimSpace(c.TSAPolicyOIDs[i])
	}
	if c.OTSConfirmations == 0 {
		c.OTSConfirmations = 6
	}
	if len(c.EsploraURLs) == 0 {
		c.EsploraURLs = []string{
			"https://blockstream.info/api",
			"https://mempool.space/api",
		}
	}
	for i, raw := range c.EsploraURLs {
		u, err := cleanProviderURL(raw)
		if err != nil {
			return fmt.Errorf("esplora_urls: %w", err)
		}
		c.EsploraURLs[i] = u
	}
	if c.SessionDays == 0 {
		c.SessionDays = 7
	}
	if c.SessionIdle == "" {
		c.SessionIdle = "12h"
	}
	if c.StayRadiusM == 0 {
		c.StayRadiusM = 200
	}
	if c.StayMinDuration == "" {
		c.StayMinDuration = "10m"
	}
	if c.TripGap == "" {
		c.TripGap = "30m"
	}
	if c.RetentionBy == "" {
		c.RetentionBy = "tst"
	}
	return c.Validate()
}

func validListenAddr(name, addr string, allowEmpty bool) error {
	if allowEmpty && addr == "" {
		return nil
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return fmt.Errorf("%s must be a host:port address", name)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%s has an invalid port", name)
	}
	return nil
}

func validAutoHTTPSDomain(domain string) bool {
	return strings.Contains(domain, ".") && net.ParseIP(domain) == nil &&
		!strings.ContainsAny(domain, "*: /@") && validDNSName(domain)
}

func (c *Config) LoginRateDuration() (time.Duration, error) {
	return time.ParseDuration(c.LoginRateWindow)
}

func (c *Config) PubRateDuration() (time.Duration, error) {
	return time.ParseDuration(c.PubRateWindow)
}

func (c *Config) ShareRateDuration() (time.Duration, error) {
	return time.ParseDuration(c.ShareRateWindow)
}

func (c *Config) BackupDuration() (time.Duration, error) {
	return time.ParseDuration(c.BackupInterval)
}

func (c *Config) BackupRetentionDays() int {
	d, err := c.BackupDuration()
	if err != nil || d <= 0 || c.BackupKeep <= 0 {
		return 0
	}
	return int(math.Ceil(float64(c.BackupKeep) * d.Hours() / 24))
}
