package config

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"path/filepath"
	"slices"
	"time"
)

const DefaultConfigPath = "config.toml"

type Config struct {
	Deployment
	Settings
}

type Deployment struct {
	DataDir          string   `toml:"data_dir"`
	ListenAddr       string   `toml:"listen_addr"`
	HTTPSListenAddr  string   `toml:"https_listen_addr"`
	TLSCertFile      string   `toml:"tls_cert_file"`
	TLSKeyFile       string   `toml:"tls_key_file"`
	AutoHTTPSDomains []string `toml:"auto_https_domains"`
	PublicURL        string   `toml:"public_url"`
	LoginRateLimit   int      `toml:"login_rate_limit"`
	LoginRateWindow  string   `toml:"login_rate_window"`
	PubRateLimit     int      `toml:"pub_rate_limit"`
	PubRateWindow    string   `toml:"pub_rate_window"`
	ShareRateLimit   int      `toml:"share_rate_limit"`
	ShareRateWindow  string   `toml:"share_rate_window"`
	StampWorkers     int      `toml:"stamp_workers"`
	StampRetries     int      `toml:"stamp_retries"`
	TrustedProxies   []string `toml:"trusted_proxies"`
	RateLimitMaxIPs  int      `toml:"rate_limit_max_ips"`
	LogLevel         string   `toml:"log_level"`
	ImportMaxBytes   int64    `toml:"import_max_bytes"`
}

type Settings struct {
	AdminHash        string   `json:"admin_hash"`
	PubUser          string   `json:"pub_user"`
	PubPass          string   `json:"pub_pass"`
	TSAURL           string   `json:"tsa_url"`
	OTSCalendars     []string `json:"ots_calendars"`
	TileURL          string   `json:"tile_url"`
	RetentionDays    int      `json:"retention_days"`
	WebhookURL       string   `json:"webhook_url"`
	TSATrustRoots    string   `json:"tsa_trust_roots"`
	TSAPolicyOIDs    []string `json:"tsa_policy_oids"`
	TileAttribution  string   `json:"tile_attribution"`
	MQTTEnabled      bool     `json:"mqtt_enabled"`
	MQTTBroker       string   `json:"mqtt_broker"`
	MQTTTopic        string   `json:"mqtt_topic"`
	MQTTUser         string   `json:"mqtt_user"`
	MQTTPass         string   `json:"mqtt_pass"`
	MQTTClientID     string   `json:"mqtt_client_id"`
	EsploraURLs      []string `json:"esplora_urls"`
	OTSConfirmations int      `json:"ots_min_confirmations"`
	SessionDays      int      `json:"session_days"`
	WebhookSecret    string   `json:"webhook_secret"`
	RetentionBy      string   `json:"retention_by"`
	MQTTCAFile       string   `json:"mqtt_ca_file"`
	BackupInterval   string   `json:"backup_interval"`
	BackupKeep       int      `json:"backup_keep"`
	SessionIdle      string   `json:"session_idle"`
	StayRadiusM      int      `json:"stay_radius_m"`
	StayMinDuration  string   `json:"stay_min_duration"`
	TripGap          string   `json:"trip_gap"`
}

func Default() *Config {
	return &Config{
		Deployment: Deployment{
			DataDir:         "data",
			ListenAddr:      "127.0.0.1:8080",
			HTTPSListenAddr: ":443",
			LoginRateLimit:  10,
			LoginRateWindow: "5m",
			PubRateLimit:    120,
			PubRateWindow:   "1m",
			ShareRateLimit:  120,
			ShareRateWindow: "1m",
			StampWorkers:    4,
			StampRetries:    3,
			RateLimitMaxIPs: 10000,
			LogLevel:        "info",
			ImportMaxBytes:  64 << 20,
		},
		Settings: Settings{
			PubUser:         "owntracks",
			TSAURL:          "https://freetsa.org/tsr",
			OTSCalendars:    []string{"https://alice.btc.calendar.opentimestamps.org/", "https://bob.btc.calendar.opentimestamps.org/"},
			TileURL:         "https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png",
			TileAttribution: "&copy; <a href=\"https://www.openstreetmap.org/copyright\">OpenStreetMap</a>",
			MQTTTopic:       "owntracks/#",
			EsploraURLs: []string{
				"https://blockstream.info/api",
				"https://mempool.space/api",
			},
			OTSConfirmations: 6,
			SessionDays:      7,
			RetentionBy:      "tst",
			BackupInterval:   "24h",
			BackupKeep:       14,
			SessionIdle:      "12h",
			StayRadiusM:      200,
			StayMinDuration:  "10m",
			TripGap:          "30m",
		},
	}
}

func (c *Config) Clone() *Config {
	cp := *c
	cp.OTSCalendars = slices.Clone(c.OTSCalendars)
	cp.AutoHTTPSDomains = slices.Clone(c.AutoHTTPSDomains)
	cp.EsploraURLs = slices.Clone(c.EsploraURLs)
	cp.TSAPolicyOIDs = slices.Clone(c.TSAPolicyOIDs)
	cp.TrustedProxies = slices.Clone(c.TrustedProxies)
	return &cp
}

func (c *Config) ProofsDir() string    { return filepath.Join(c.DataDir, "proofs") }
func (c *Config) DBPath() string       { return filepath.Join(c.DataDir, "mytracker.db") }
func (c *Config) BackupsDir() string   { return filepath.Join(c.DataDir, "backups") }
func (c *Config) ACMECacheDir() string { return filepath.Join(c.DataDir, "acme") }
func (c *Config) TLSConfigured() bool {
	return c.TLSCertFile != "" || len(c.AutoHTTPSDomains) > 0
}
func (c *Config) SessionTTL() time.Duration {
	if c.SessionDays < 1 {
		return 7 * 24 * time.Hour
	}
	return time.Duration(c.SessionDays) * 24 * time.Hour
}

func (c *Config) SessionIdleDuration() time.Duration {
	d, err := time.ParseDuration(c.SessionIdle)
	if err != nil || d <= 0 {
		return 12 * time.Hour
	}
	return d
}

func (c *Config) StayDuration() time.Duration {
	d, _ := time.ParseDuration(c.StayMinDuration)
	return d
}
func (c *Config) TripGapDuration() time.Duration { d, _ := time.ParseDuration(c.TripGap); return d }

func (c *Config) MQTTFingerprint() string {
	h := sha256.New()
	if c.MQTTEnabled {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	for _, value := range []string{c.MQTTBroker, c.MQTTTopic, c.MQTTUser, c.MQTTPass, c.MQTTClientID, c.MQTTCAFile} {
		var length [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(length[:], uint64(len(value)))
		h.Write(length[:n])
		h.Write([]byte(value))
	}
	return hex.EncodeToString(h.Sum(nil))
}
