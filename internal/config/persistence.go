package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/prepaser/mytracker/internal/atomicfile"
	"golang.org/x/sys/unix"
)

func Load(path string) (*Config, string, error) {
	if path == "" {
		path = DefaultConfigPath
	}
	cfg, err := Read(path)
	if os.IsNotExist(err) {
		return initNew(path)
	}
	return cfg, "", err
}

func Read(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	dec := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	if err := dec.Decode(&cfg.Deployment); err != nil {
		return nil, err
	}
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func initNew(path string) (*Config, string, error) {
	cfg := Default()
	if err := cfg.Save(path); err != nil {
		return nil, "", err
	}
	return cfg, fmt.Sprintf("config created: %s", path), nil
}

func (c *Config) LocalLoginURL() string {
	addr := c.ListenAddr
	if c.TLSConfigured() {
		addr = c.HTTPSListenAddr
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return c.URLScheme() + "://localhost"
	}
	if (c.URLScheme() == "https" && port == "443") || (c.URLScheme() == "http" && port == "80") {
		return c.URLScheme() + "://localhost"
	}
	return c.URLScheme() + "://" + net.JoinHostPort("localhost", port)
}

func (c *Config) LoginURL() string {
	if public := strings.TrimRight(strings.TrimSpace(c.PublicURL), "/"); public != "" {
		return public
	}
	if len(c.AutoHTTPSDomains) > 0 {
		host := c.AutoHTTPSDomains[0]
		if _, port, err := net.SplitHostPort(c.HTTPSListenAddr); err == nil && port != "" && port != "443" {
			host = net.JoinHostPort(host, port)
		}
		return "https://" + host
	}
	return c.LocalLoginURL()
}

func (c *Config) Save(path string) error {
	if path == "" {
		path = DefaultConfigPath
	}
	b, err := toml.Marshal(c.Deployment)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	return atomicfile.Write(path, b, 0600)
}

func (c *Config) EnsureDirs() error {
	if err := ValidateDataDir(c.DataDir); err != nil {
		return err
	}
	if err := ensurePrivateDir(c.DataDir); err != nil {
		return err
	}
	for _, dir := range []string{c.ProofsDir(), c.BackupsDir()} {
		if err := ensurePrivateDir(dir); err != nil {
			return err
		}
	}
	if len(c.AutoHTTPSDomains) > 0 {
		return ensurePrivateDir(c.ACMECacheDir())
	}
	return nil
}

func ValidateDataDir(path string) error {
	trimmed := strings.TrimSpace(path)
	if trimmed != path {
		return fmt.Errorf("data_dir must not have surrounding whitespace")
	}
	path = filepath.Clean(path)
	if path == "" || path == "." {
		return fmt.Errorf("data_dir must be a dedicated directory")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if filepath.Dir(abs) == abs {
		return fmt.Errorf("data_dir must not be a filesystem root")
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("data_dir must be a real directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "mytracker.db" || name == "proofs" || name == "backups" || name == "acme" ||
			strings.HasPrefix(name, ".mytracker") || strings.HasPrefix(name, ".restore-") {
			return nil
		}
	}
	return fmt.Errorf("data_dir is not empty and does not look like a mytracker directory")
}

func ensurePrivateDir(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	root := filepath.VolumeName(abs) + string(os.PathSeparator)
	current := root
	for part := range strings.SplitSeq(strings.TrimPrefix(abs, root), string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, 0700); err != nil && !os.IsExist(err) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%s must be a real directory", current)
		}
	}
	fd, err := unix.Open(abs, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%s must be a real directory: %w", abs, err)
	}
	if err := unix.Fchmod(fd, 0700); err != nil {
		_ = unix.Close(fd)
		return err
	}
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("close %s: %w", abs, err)
	}
	return nil
}

func RandToken(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("token length must be positive")
	}
	b, err := RandBytes(n)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n], nil
}

func RandBytes(n int) ([]byte, error) {
	if n <= 0 {
		return nil, fmt.Errorf("length must be positive")
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}
