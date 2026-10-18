package config_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/password"
)

func TestLoadCreatesConfigOnFirstRun(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg, msg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if msg != "config created: "+path {
		t.Fatalf("expected init message, got %q", msg)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("config file not created")
	}
	if _, err := os.Stat(config.InitialPasswordPath()); !os.IsNotExist(err) {
		t.Fatalf("initial password unexpectedly created: %v", err)
	}
	if cfg.AdminHash != "" || cfg.PubPass != "" {
		t.Fatal("credentials generated before manager initialization")
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "app-data")
	cfg.ListenAddr = "0.0.0.0:8090"
	cfg.PubUser = "not-deployment"
	cfg.PubPass = "not-persisted"
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pub_user", "pub_pass", "admin_hash"} {
		if strings.Contains(string(body), key) {
			t.Fatalf("settings key %q persisted in deployment TOML:\n%s", key, body)
		}
	}
	loaded, _, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DataDir != cfg.DataDir || loaded.ListenAddr != cfg.ListenAddr {
		t.Fatalf("deployment did not round trip: %+v", loaded.Deployment)
	}
	if loaded.PubUser != config.Default().PubUser || loaded.PubPass != "" || loaded.AdminHash != "" {
		t.Fatalf("settings unexpectedly loaded from TOML: %+v", loaded.Settings)
	}
}

func TestManagerInitialPasswordCleanup(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	mgr := config.NewManager(cfg, &memorySettingsStore{})
	initial := mgr.InitialPasswordPath()
	if initial != ".initial-password" {
		t.Fatalf("initial path=%q", initial)
	}
	if err := os.WriteFile(initial, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RemoveInitialPassword(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RemoveInitialPassword(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.SessionIdleDuration(); got != 12*time.Hour {
		t.Fatalf("idle=%v", got)
	}
}

func TestPubURL(t *testing.T) {
	cfg := config.Default()
	cfg.PubUser = "user"
	cfg.PubPass = "pass"
	cfg.ListenAddr = ":8080"

	got := cfg.PubURL("192.168.1.10")
	want := "http://user:pass@192.168.1.10:8080/pub"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}

	got = cfg.PubURL("example.com:443")
	if got != "http://user:pass@example.com:443/pub" {
		t.Fatalf("got %q", got)
	}
}

func TestPubURLWithPublicURL(t *testing.T) {
	cfg := config.Default()
	cfg.PubUser = "user"
	cfg.PubPass = "pass"
	cfg.PublicURL = "https://tracker.example.com"

	got := cfg.PubURL("ignored.local:8080")
	want := "https://user:pass@tracker.example.com/pub"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}

	cfg.PublicURL = "https://tracker.example.com:8443/"
	got = cfg.PubURL("ignored")
	if got != "https://user:pass@tracker.example.com:8443/pub" {
		t.Fatalf("got %q", got)
	}
}

func TestGeneratedURLUsesNativeHTTPS(t *testing.T) {
	cfg := config.Default()
	cfg.HTTPSListenAddr = ":443"
	cfg.TLSCertFile = "/etc/mytracker/tls.crt"
	cfg.TLSKeyFile = "/etc/mytracker/tls.key"
	if got := cfg.ShareURL("abc", "tracker.example.com"); got != "https://tracker.example.com/share/abc" {
		t.Fatalf("share URL=%q", got)
	}
	if got := cfg.LocalLoginURL(); got != "https://localhost" {
		t.Fatalf("login URL=%q", got)
	}
	cfg.HTTPSListenAddr = "127.0.0.1:8443"
	if got := cfg.ShareURL("abc", "tracker.example.com:8080"); got != "https://tracker.example.com:8443/share/abc" {
		t.Fatalf("separate HTTPS URL=%q", got)
	}
	if got := cfg.LocalLoginURL(); got != "https://localhost:8443" {
		t.Fatalf("separate HTTPS login=%q", got)
	}
	cfg.ListenAddr = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("HTTPS-only config: %v", err)
	}
	cfg.ListenAddr = cfg.HTTPSListenAddr
	if err := cfg.Validate(); err == nil {
		t.Fatal("conflicting listeners accepted")
	}
	cfg.ListenAddr, cfg.HTTPSListenAddr = "127.0.0.1:8080", ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("TLS without HTTPS listener accepted")
	}
}

func TestLoginURL(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*config.Config)
		want  string
	}{
		{
			name: "public URL",
			setup: func(cfg *config.Config) {
				cfg.PublicURL = "https://tracker.example.com/"
			},
			want: "https://tracker.example.com",
		},
		{
			name: "automatic HTTPS default port",
			setup: func(cfg *config.Config) {
				cfg.AutoHTTPSDomains = []string{"tracker.example.com", "other.example.com"}
			},
			want: "https://tracker.example.com",
		},
		{
			name: "automatic HTTPS custom port",
			setup: func(cfg *config.Config) {
				cfg.AutoHTTPSDomains = []string{"tracker.example.com"}
				cfg.HTTPSListenAddr = ":8443"
			},
			want: "https://tracker.example.com:8443",
		},
		{
			name: "native HTTPS",
			setup: func(cfg *config.Config) {
				cfg.TLSCertFile = "/etc/mytracker/tls.crt"
				cfg.TLSKeyFile = "/etc/mytracker/tls.key"
				cfg.HTTPSListenAddr = ":8443"
			},
			want: "https://localhost:8443",
		},
		{
			name:  "default local",
			setup: func(cfg *config.Config) {},
			want:  "http://localhost:8080",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			tt.setup(cfg)
			if got := cfg.LoginURL(); got != tt.want {
				t.Fatalf("login URL=%q want %q", got, tt.want)
			}
		})
	}
}

func TestGeneratedURLUsesTrustedRequestHTTPS(t *testing.T) {
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1:8080"
	cfg.PubUser = "user"
	cfg.PubPass = "pass"
	if got := cfg.PubURLForRequest("tracker.example.com", true); got != "https://user:pass@tracker.example.com/pub" {
		t.Fatalf("pub URL=%q", got)
	}
	if got := cfg.ShareURLForRequest("token", "tracker.example.com", true); got != "https://tracker.example.com/share/token" {
		t.Fatalf("share URL=%q", got)
	}
	cfg.PublicURL = "http://internal.example.com"
	if got := cfg.ShareURLForRequest("token", "tracker.example.com", true); got != "http://internal.example.com/share/token" {
		t.Fatalf("configured share URL=%q", got)
	}
}

func TestPathAndDurationFallbacks(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = "private-data"
	if got := cfg.DBPath(); got != filepath.Join("private-data", "mytracker.db") {
		t.Fatalf("db path=%q", got)
	}
	if got := config.InitialPasswordPath(); got != ".initial-password" {
		t.Fatalf("initial password path=%q", got)
	}
	cfg.ListenAddr = "invalid"
	if got := cfg.LocalLoginURL(); got != "http://localhost" {
		t.Fatalf("fallback login URL=%q", got)
	}
	cfg.SessionIdle = "invalid"
	if got := cfg.SessionIdleDuration(); got != 12*time.Hour {
		t.Fatalf("fallback idle duration=%v", got)
	}

	nested := filepath.Join(t.TempDir(), "nested", "config.toml")
	if err := cfg.Save(nested); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Fatal(err)
	}
}

func TestSaveIsAtomicAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1:8081"
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	cfg.ListenAddr = "127.0.0.1:8082"
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ListenAddr != "127.0.0.1:8082" {
		t.Fatalf("listen addr=%q", loaded.ListenAddr)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".config.toml.tmp-*"))
	if len(files) != 0 {
		t.Fatalf("temporary files left behind: %v", files)
	}
}

func TestShareURL(t *testing.T) {
	cfg := config.Default()
	cfg.PublicURL = "https://tracker.example.com"
	got := cfg.ShareURL("abc123", "ignored")
	if got != "https://tracker.example.com/share/abc123" {
		t.Fatalf("got %q", got)
	}
}

func TestGeneratedURLRejectsMalformedHost(t *testing.T) {
	cfg := config.Default()
	cfg.ListenAddr = ":8080"
	cfg.PubUser = "user"
	cfg.PubPass = "pass"
	for _, host := range []string{
		"evil.example/path",
		"user@evil.example",
		"evil_example",
		"example.com:99999",
		"example.com\r\nX-Evil: yes",
	} {
		if got := cfg.ShareURL("abc", host); got != "http://localhost:8080/share/abc" {
			t.Fatalf("host=%q url=%q", host, got)
		}
	}
	if got := cfg.PubURL("[::1]:8443"); got != "http://user:pass@[::1]:8443/pub" {
		t.Fatalf("ipv6 url=%q", got)
	}
}

func TestValidateRejectsBadURLs(t *testing.T) {
	cfg := config.Default()
	cfg.PublicURL = "javascript:alert(1)"
	if err := cfg.Normalize(); err == nil {
		t.Fatal("expected bad public url")
	}
	cfg = config.Default()
	cfg.TSAURL = "file:///tmp/token"
	if err := cfg.Normalize(); err == nil {
		t.Fatal("expected bad tsa url")
	}
	cfg = config.Default()
	cfg.PublicURL = "https://user:pass@tracker.example.com"
	if err := cfg.Normalize(); err == nil {
		t.Fatal("expected public URL credentials to be rejected")
	}
}

func TestValidateRejectsInvalidOwnTracksUsername(t *testing.T) {
	for _, username := range []string{"user:name", "user\nname", "user\x00name", "user\x7fname"} {
		cfg := config.Default()
		cfg.PubUser = username
		if err := cfg.Validate(); err == nil {
			t.Fatalf("username %q accepted", username)
		}
	}
}

func TestEsploraPoolValidation(t *testing.T) {
	cfg := config.Default()
	cfg.EsploraURLs = []string{"https://one.example/api", "https://one.example/api"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("duplicate provider accepted")
	}
	for _, urls := range [][]string{
		{"HTTPS://ONE.EXAMPLE:443/api", "https://one.example/api"},
		{"http://LOCALHOST:80/api/", "http://localhost/api"},
		{"https://one.example/api#ignored", "https://one.example/api"},
		{"https://one.example.:443/api/%7e", "https://one.example/api/~"},
	} {
		cfg = config.Default()
		cfg.EsploraURLs = urls
		if err := cfg.Validate(); err == nil {
			t.Fatalf("provider aliases accepted: %v", urls)
		}
	}
	cfg = config.Default()
	cfg.EsploraURLs = []string{"HTTPS://ONE.EXAMPLE:443/api/"}
	if err := cfg.Normalize(); err != nil || cfg.EsploraURLs[0] != "https://one.example/api" {
		t.Fatalf("provider normalization=%v urls=%v", err, cfg.EsploraURLs)
	}
	for _, raw := range []string{
		"http://example.com/api",
		"https://user:pass@example.com/api",
		"https://example.com/api?token=secret",
	} {
		cfg = config.Default()
		cfg.EsploraURLs = []string{raw}
		if err := cfg.Normalize(); err == nil {
			t.Fatalf("provider %q accepted", raw)
		}
	}
	cfg = config.Default()
	cfg.EsploraURLs = []string{"http://127.0.0.1:3000/api"}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}

}

func TestProofTrustPolicyValidation(t *testing.T) {
	cfg := config.Default()
	cfg.TSAPolicyOIDs = []string{"1.2.3.4", "1.2.3.4"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("duplicate TSA policy accepted")
	}
	cfg = config.Default()
	cfg.TSAPolicyOIDs = []string{"3.1"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid TSA policy accepted")
	}
	cfg = config.Default()
	cfg.OTSConfirmations = 145
	if err := cfg.Validate(); err == nil {
		t.Fatal("excessive confirmations accepted")
	}

}

func TestRateDurations(t *testing.T) {
	cfg := config.Default()
	if d, err := cfg.LoginRateDuration(); err != nil || d.String() != "5m0s" {
		t.Fatalf("login duration=%v err=%v", d, err)
	}
	if d, err := cfg.PubRateDuration(); err != nil || d.String() != "1m0s" {
		t.Fatalf("pub duration=%v err=%v", d, err)
	}
}

func TestManagerSnapshotAndUpdate(t *testing.T) {
	cfg := managerTestConfig(t)
	store := seedSettingsStore(t, cfg)
	mgr := config.NewManager(cfg, store)
	snap := mgr.Snapshot()
	snap.PubUser = "mutated"
	if mgr.Snapshot().PubUser == "mutated" {
		t.Fatal("snapshot mutation leaked")
	}
	next, err := mgr.Update(func(s *config.Settings) error {
		s.PubUser = "updated"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.PubUser != "updated" || mgr.Snapshot().PubUser != "updated" {
		t.Fatal("update not applied")
	}
	if store.saveCalls != 1 || store.revoked {
		t.Fatalf("save calls=%d revoked=%t", store.saveCalls, store.revoked)
	}
	var persisted config.Settings
	if err := json.Unmarshal(store.data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.PubUser != "updated" {
		t.Fatalf("settings not persisted: %+v", persisted)
	}
}

func TestManagerUpdateFailureDoesNotPublish(t *testing.T) {
	cfg := managerTestConfig(t)
	store := seedSettingsStore(t, cfg)
	mgr := config.NewManager(cfg, store)
	before := append([]byte(nil), store.data...)
	if _, err := mgr.Update(func(s *config.Settings) error {
		s.PubUser = "not-published"
		return errors.New("stop")
	}); err == nil {
		t.Fatal("callback failure accepted")
	}
	if mgr.Snapshot().PubUser != cfg.PubUser {
		t.Fatal("callback failure changed snapshot")
	}
	if _, err := mgr.Update(func(s *config.Settings) error {
		s.SessionDays = 366
		return nil
	}); err == nil {
		t.Fatal("validation failure accepted")
	}
	store.saveErr = errors.New("database unavailable")
	if _, err := mgr.Update(func(s *config.Settings) error {
		s.PubUser = "not-published"
		return nil
	}); err == nil {
		t.Fatal("persistence failure accepted")
	}
	if string(store.data) != string(before) || mgr.Snapshot().PubUser != cfg.PubUser {
		t.Fatal("failed update changed persisted or published settings")
	}
}

func TestManagerUpdateWithSessionRevocation(t *testing.T) {
	cfg := managerTestConfig(t)
	store := seedSettingsStore(t, cfg)
	mgr := config.NewManager(cfg, store)
	if _, err := mgr.UpdateWithSessionRevocation(func(s *config.Settings) error {
		s.SessionDays = 14
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !store.revoked {
		t.Fatal("session revocation was not requested")
	}
}

func TestManagerInitialize(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	store := &memorySettingsStore{}
	mgr := config.NewManager(cfg, store)
	created, err := mgr.Initialize()
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first initialization not reported")
	}
	initialPath := config.InitialPasswordPath()
	initialPassword, err := os.ReadFile(initialPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(initialPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("initial password mode=%o", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "initial-password")); !os.IsNotExist(err) {
		t.Fatalf("plaintext password stored in data directory: %v", err)
	}
	snap := mgr.Snapshot()
	if snap.AdminHash == "" || snap.PubPass == "" {
		t.Fatal("credentials not initialized")
	}
	if ok, _ := password.Verify(snap.AdminHash, []byte(strings.TrimSpace(string(initialPassword)))); !ok {
		t.Fatal("initial password does not match admin hash")
	}
	if strings.Contains(string(store.data), strings.TrimSpace(string(initialPassword))) {
		t.Fatal("plaintext password persisted in settings")
	}
	if err := os.Rename(initialPath, "initial-password"); err != nil {
		t.Fatal(err)
	}
	created, err = mgr.Initialize()
	if err != nil || created {
		t.Fatalf("second initialization created=%t err=%v", created, err)
	}
	if _, err := os.Stat("initial-password"); !os.IsNotExist(err) {
		t.Fatalf("visible password file remains: %v", err)
	}
	if data, err := os.ReadFile(initialPath); err != nil || string(data) != string(initialPassword) {
		t.Fatalf("password changed during rename: %v", err)
	}
}

func TestManagerInitializeFailureDoesNotPublish(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	store := &memorySettingsStore{initializeErr: errors.New("database unavailable")}
	mgr := config.NewManager(cfg, store)
	if _, err := mgr.Initialize(); err == nil {
		t.Fatal("initialization failure accepted")
	}
	if mgr.Snapshot().AdminHash != "" {
		t.Fatal("failed initialization published credentials")
	}
	if _, err := os.Stat(config.InitialPasswordPath()); !os.IsNotExist(err) {
		t.Fatalf("failed initialization created password file: %v", err)
	}
}

func TestManagerInitializeRecoversAfterDatabaseFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	store := &memorySettingsStore{initializeAfterCallbackErr: errors.New("database unavailable")}
	mgr := config.NewManager(cfg, store)
	if _, err := mgr.Initialize(); err == nil {
		t.Fatal("initialization failure accepted")
	}
	first, err := os.ReadFile(config.InitialPasswordPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, ".initial-password.pending")); err != nil {
		t.Fatalf("pending marker missing: %v", err)
	}
	store.initializeAfterCallbackErr = nil
	created, err := mgr.Initialize()
	if err != nil || !created {
		t.Fatalf("recovery created=%t err=%v", created, err)
	}
	second, err := os.ReadFile(config.InitialPasswordPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("recovery changed the initial password")
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, ".initial-password.pending")); !os.IsNotExist(err) {
		t.Fatalf("pending marker remains: %v", err)
	}
}

func TestInitialPasswordRelocation(t *testing.T) {
	for name, existing := range map[string]string{"missing": "", "same": "bootstrap\n", "conflict": "different\n"} {
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			cfg := managerTestConfig(t)
			cfg.DataDir = t.TempDir()
			old := filepath.Join(cfg.DataDir, "initial-password")
			if err := os.WriteFile(old, []byte("bootstrap\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if existing != "" {
				if err := os.WriteFile(config.InitialPasswordPath(), []byte(existing), 0644); err != nil {
					t.Fatal(err)
				}
			}
			mgr := config.NewManager(cfg, seedSettingsStore(t, cfg))
			created, err := mgr.Initialize()
			if existing == "different\n" {
				if err == nil {
					t.Fatal("conflicting bootstrap file accepted")
				}
				if data, err := os.ReadFile(old); err != nil || string(data) != "bootstrap\n" {
					t.Fatalf("source changed on conflict: %v", err)
				}
				if data, err := os.ReadFile(config.InitialPasswordPath()); err != nil || string(data) != existing {
					t.Fatalf("destination changed on conflict: %v", err)
				}
				return
			}
			if err != nil || created {
				t.Fatalf("relocation created=%t err=%v", created, err)
			}
			if _, err := os.Stat(old); !os.IsNotExist(err) {
				t.Fatalf("old password file remains: %v", err)
			}
			if data, err := os.ReadFile(config.InitialPasswordPath()); err != nil || string(data) != "bootstrap\n" {
				t.Fatalf("relocated password mismatch: %v", err)
			}
			info, err := os.Stat(config.InitialPasswordPath())
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("relocated password permissions: %v", err)
			}
		})
	}
}

func TestInitialPasswordRejectsExistingFileAndDataWorkingDir(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if err := os.WriteFile(config.InitialPasswordPath(), []byte("keep\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &memorySettingsStore{}
	if _, err := config.NewManager(cfg, store).Initialize(); err == nil || len(store.data) != 0 {
		t.Fatal("initialization overwrote existing password file")
	}
	if data, err := os.ReadFile(config.InitialPasswordPath()); err != nil || string(data) != "keep\n" {
		t.Fatalf("existing password changed: %v", err)
	}
	cfg.DataDir = root
	if _, err := config.NewManager(cfg, store).Initialize(); err == nil {
		t.Fatal("initialization inside data directory accepted")
	}
}

func managerTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.AdminHash = "existing-hash"
	cfg.PubPass = "pub-secret"
	return cfg
}

func seedSettingsStore(t *testing.T, cfg *config.Config) *memorySettingsStore {
	t.Helper()
	data, err := json.Marshal(cfg.Settings)
	if err != nil {
		t.Fatal(err)
	}
	return &memorySettingsStore{data: data, initialized: true}
}

type memorySettingsStore struct {
	data                       []byte
	initialized                bool
	saveErr                    error
	initializeErr              error
	initializeAfterCallbackErr error
	saveCalls                  int
	revoked                    bool
}

func (s *memorySettingsStore) LoadSettings() ([]byte, error) {
	return append([]byte(nil), s.data...), nil
}

func (s *memorySettingsStore) SaveSettings(data []byte, revoke bool) error {
	s.saveCalls++
	if s.saveErr != nil {
		return s.saveErr
	}
	s.data = append([]byte(nil), data...)
	s.revoked = revoke
	return nil
}

func (s *memorySettingsStore) InitializeSettings(init func() ([]byte, error)) ([]byte, bool, error) {
	if s.initializeErr != nil {
		return nil, false, s.initializeErr
	}
	if s.initialized {
		return append([]byte(nil), s.data...), false, nil
	}
	data, err := init()
	if err != nil {
		return nil, false, err
	}
	if s.initializeAfterCallbackErr != nil {
		return nil, false, s.initializeAfterCallbackErr
	}
	s.data = append([]byte(nil), data...)
	s.initialized = true
	return append([]byte(nil), data...), true, nil
}

func TestDefaultTOMLGolden(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := config.Default().Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "default.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("default TOML changed\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestRandTokenLength(t *testing.T) {
	tok, err := config.RandToken(16)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 16 {
		t.Fatalf("len=%d", len(tok))
	}
}

func TestEnsureDirs(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.AutoHTTPSDomains = []string{"tracker.example.com"}
	for _, path := range []string{cfg.DataDir, cfg.ProofsDir(), cfg.BackupsDir(), cfg.ACMECacheDir()} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cfg.DataDir, cfg.ProofsDir(), cfg.BackupsDir(), cfg.ACMECacheDir()} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("directory %s info=%v err=%v", path, info, err)
		}
	}
}

func TestEnsureDirsRejectsUnsafeDataDirectory(t *testing.T) {
	dir := t.TempDir()
	important := filepath.Join(dir, "important")
	if err := os.Mkdir(important, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(important, "keep"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = important
	if err := cfg.EnsureDirs(); err == nil {
		t.Fatal("non-mytracker directory accepted")
	}
	info, err := os.Stat(important)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("directory permissions changed to %v", info.Mode().Perm())
	}
	cfg.DataDir = string(filepath.Separator)
	if err := cfg.EnsureDirs(); err == nil {
		t.Fatal("filesystem root accepted")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(important, link); err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = link
	if err := cfg.EnsureDirs(); err == nil {
		t.Fatal("symlink data directory accepted")
	}
	cfg.DataDir = " data "
	if err := cfg.EnsureDirs(); err == nil {
		t.Fatal("data directory with surrounding whitespace accepted")
	}
}

func TestEnsureDirsRejectsFixedDirectorySymlinks(t *testing.T) {
	for _, name := range []string{"proofs", "backups", "acme"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			dataDir := filepath.Join(dir, "data")
			if err := os.Mkdir(dataDir, 0700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "outside")
			if err := os.Mkdir(target, 0755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(target, "keep")
			if err := os.WriteFile(marker, []byte("untouched"), 0644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dataDir, name)
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}

			cfg := config.Default()
			cfg.DataDir = dataDir
			if name == "acme" {
				cfg.AutoHTTPSDomains = []string{"tracker.example.com"}
			}
			if err := cfg.EnsureDirs(); err == nil {
				t.Fatal("fixed directory symlink accepted")
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0755 {
				t.Fatalf("symlink target mode changed to %v", info.Mode().Perm())
			}
			contents, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != "untouched" {
				t.Fatalf("symlink target contents changed: %q", contents)
			}
			info, err = os.Lstat(link)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Fatal("fixed directory was replaced")
			}
		})
	}
}

func TestValidateOperationalBounds(t *testing.T) {
	cases := []struct {
		name string
		set  func(*config.Config)
	}{
		{"login limit", func(c *config.Config) { c.LoginRateLimit = 0 }},
		{"login window", func(c *config.Config) { c.LoginRateWindow = "-1s" }},
		{"pub limit", func(c *config.Config) { c.PubRateLimit = 1_000_001 }},
		{"pub window", func(c *config.Config) { c.PubRateWindow = "25h" }},
		{"share limit", func(c *config.Config) { c.ShareRateLimit = 0 }},
		{"share window", func(c *config.Config) { c.ShareRateWindow = "bad" }},
		{"workers", func(c *config.Config) { c.StampWorkers = 65 }},
		{"retries", func(c *config.Config) { c.StampRetries = 11 }},
		{"retention", func(c *config.Config) { c.RetentionDays = -1 }},
		{"retention overflow", func(c *config.Config) { c.RetentionDays = 106752 }},
		{"retention by", func(c *config.Config) { c.RetentionBy = "wrong" }},
		{"session", func(c *config.Config) { c.SessionDays = 366 }},
		{"tracked ips", func(c *config.Config) { c.RateLimitMaxIPs = 99 }},
		{"backup interval", func(c *config.Config) { c.BackupInterval = "0s" }},
		{"backup keep", func(c *config.Config) { c.BackupKeep = 0 }},
		{"import max", func(c *config.Config) { c.ImportMaxBytes = 100 }},
		{"stay radius", func(c *config.Config) { c.StayRadiusM = 1 }},
		{"stay duration", func(c *config.Config) { c.StayMinDuration = "0s" }},
		{"trip gap", func(c *config.Config) { c.TripGap = "bad" }},
		{"tile", func(c *config.Config) { c.TileURL = "file:///tile" }},
		{"mqtt", func(c *config.Config) { c.MQTTEnabled = true; c.MQTTBroker = "" }},
		{"listen address", func(c *config.Config) { c.ListenAddr = "localhost" }},
		{"https listen address", func(c *config.Config) { c.HTTPSListenAddr = "bad" }},
		{"TLS certificate pair", func(c *config.Config) { c.TLSCertFile = "cert.pem" }},
		{"TLS mode conflict", func(c *config.Config) {
			c.TLSCertFile, c.TLSKeyFile = "cert.pem", "key.pem"
			c.AutoHTTPSDomains = []string{"tracker.example.com"}
		}},
		{"automatic HTTPS domain", func(c *config.Config) { c.AutoHTTPSDomains = []string{"*.example.com"} }},
		{"automatic HTTPS IP", func(c *config.Config) { c.AutoHTTPSDomains = []string{"127.0.0.1"} }},
		{"automatic HTTPS local name", func(c *config.Config) { c.AutoHTTPSDomains = []string{"localhost"} }},
		{"automatic HTTPS duplicate", func(c *config.Config) {
			c.AutoHTTPSDomains = []string{"tracker.example.com", "tracker.example.com"}
		}},
		{"automatic HTTPS public URL", func(c *config.Config) {
			c.AutoHTTPSDomains = []string{"tracker.example.com"}
			c.PublicURL = "https://other.example.com"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.set(cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestBackupRetentionDays(t *testing.T) {
	cfg := config.Default()
	if got := cfg.BackupRetentionDays(); got != 14 {
		t.Fatalf("default days=%d", got)
	}
	cfg.BackupInterval = "12h"
	if got := cfg.BackupRetentionDays(); got != 7 {
		t.Fatalf("half-day interval days=%d", got)
	}
	cfg.BackupInterval = "bad"
	if got := cfg.BackupRetentionDays(); got != 0 {
		t.Fatalf("invalid interval days=%d", got)
	}
}

func TestOTSCalendarValidationRedactsCredentials(t *testing.T) {
	cfg := config.Default()
	secret := "https://user:secret@example.com/calendar"
	cfg.OTSCalendars = []string{secret}
	err := cfg.Normalize()
	if err == nil || strings.Contains(err.Error(), "user") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe validation error: %v", err)
	}
}

func TestOTSCalendarAcceptsHTTPAndPreservesQuery(t *testing.T) {
	cfg := config.Default()
	cfg.OTSCalendars = []string{"HTTP://calendar.example/calendar/?token=a/"}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.OTSCalendars[0]; got != "http://calendar.example/calendar/?token=a/" {
		t.Fatalf("calendar=%q", got)
	}
}

func TestNormalizePublicURLPreservesEscapedPath(t *testing.T) {
	cfg := config.Default()
	cfg.PublicURL = "https://tracker.example/calendar/%2F/"
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.PublicURL; got != "https://tracker.example/calendar/%2F" {
		t.Fatalf("public url=%q", got)
	}
}

func TestConfigHelpers(t *testing.T) {
	cfg := config.Default()
	cfg.OTSCalendars = []string{"https://one.example"}
	cfg.TrustedProxies = []string{"127.0.0.1"}
	clone := cfg.Clone()
	clone.OTSCalendars[0] = "changed"
	clone.EsploraURLs[0] = "changed"
	clone.TrustedProxies[0] = "changed"
	clone.AutoHTTPSDomains = []string{"changed.example"}
	if cfg.OTSCalendars[0] == "changed" || cfg.EsploraURLs[0] == "changed" ||
		cfg.TrustedProxies[0] == "changed" {
		t.Fatal("clone shares slices")
	}
	if cfg.SessionTTL() != 7*24*time.Hour {
		t.Fatal("session ttl")
	}
	if cfg.MQTTFingerprint() == "" || cfg.BackupsDir() != filepath.Join(cfg.DataDir, "backups") {
		t.Fatal("config helper")
	}
	if cfg.StayDuration() != 10*time.Minute || cfg.TripGapDuration() != 30*time.Minute {
		t.Fatal("timeline durations")
	}
	empty := config.Default()
	empty.StayRadiusM = 0
	empty.StayMinDuration = ""
	empty.TripGap = ""
	if err := empty.Normalize(); err != nil {
		t.Fatal(err)
	}
	if empty.StayRadiusM != 200 || empty.StayMinDuration != "10m" || empty.TripGap != "30m" {
		t.Fatalf("timeline defaults=%+v", empty)
	}
}

func TestNormalizeAutoHTTPS(t *testing.T) {
	cfg := config.Default()
	cfg.AutoHTTPSDomains = []string{" Tracker.Example.COM "}
	cfg.PublicURL = "https://tracker.example.com"
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.AutoHTTPSDomains[0]; got != "tracker.example.com" {
		t.Fatalf("domain=%q", got)
	}
}

func TestConfigErrorPaths(t *testing.T) {
	cfg := config.Default()
	cfg.SessionDays = 0
	if cfg.SessionTTL() != 7*24*time.Hour {
		t.Fatal("session fallback")
	}
	if _, err := config.RandToken(0); err == nil {
		t.Fatal("zero token length accepted")
	}
	if _, err := config.RandBytes(0); err == nil {
		t.Fatal("zero byte length accepted")
	}
	path := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(path, []byte(":\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.Load(path); err == nil {
		t.Fatal("malformed config accepted")
	}
}

func TestLoadRejectsNonDeploymentTOMLKeys(t *testing.T) {
	for _, key := range []string{
		`webhook_url = "https://hooks.example.test/notify"`,
		`admin_hash = "hash"`,
		`pub_pass = "secret"`,
		"unknown_setting = true",
	} {
		t.Run(strings.TrimSpace(strings.SplitN(key, "=", 2)[0]), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte("listen_addr = \"127.0.0.1:8080\"\n"+key+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := config.Load(path); err == nil {
				t.Fatalf("unknown TOML key accepted: %s", key)
			}
		})
	}
}

func TestLoadRejectsInvalidTOML(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate":   "stamp_workers = 2\nstamp_workers = 4\n",
		"wrong type":  `stamp_workers = "4"`,
		"wrong array": `trusted_proxies = [1]`,
		"YAML":        "listen_addr: 127.0.0.1:8080\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := config.Load(path); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestApplySettingsUsesDefaultsAndStrictTypedJSON(t *testing.T) {
	cfg := config.Default()
	data := []byte(`{"admin_hash":"hash","pub_user":"owner","pub_pass":"secret"}`)
	if err := cfg.ApplySettings(data); err != nil {
		t.Fatal(err)
	}
	if cfg.AdminHash != "hash" || cfg.PubUser != "owner" || cfg.PubPass != "secret" ||
		cfg.SessionDays != 7 || cfg.SessionIdle != "12h" || len(cfg.EsploraURLs) != 2 {
		t.Fatalf("settings defaults not applied: %+v", cfg.Settings)
	}
	for _, raw := range []string{
		`{"admin_hash":"hash","pub_user":"owner","pub_pass":"secret","unknown":true}`,
		`{"admin_hash":"hash","pub_user":"owner","pub_pass":"secret","session_days":"7"}`,
		`{"admin_hash":"hash","pub_user":"owner","pub_pass":"secret","session_days":366}`,
		`{"admin_hash":"hash","pub_user":"owner","pub_pass":"secret"}{}`,
	} {
		before := cfg.Clone()
		if err := cfg.ApplySettings([]byte(raw)); err == nil {
			t.Fatalf("invalid settings accepted: %s", raw)
		}
		if cfg.PubUser != before.PubUser || cfg.PubPass != before.PubPass || cfg.SessionDays != before.SessionDays {
			t.Fatalf("invalid settings changed config: %+v", cfg.Settings)
		}
	}
}
