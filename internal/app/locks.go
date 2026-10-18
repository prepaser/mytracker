package app

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/prepaser/mytracker/internal/backup"
	"github.com/prepaser/mytracker/internal/config"
	"golang.org/x/sys/unix"
)

func recoverRestore(cfg *config.Config, lock *os.File, exclusive bool) error {
	_, err := os.Stat(filepath.Join(cfg.DataDir, ".restore-journal.json"))
	if os.IsNotExist(err) {
		return backup.Scavenge(cfg)
	}
	if err != nil {
		return err
	}
	if !exclusive {
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return fmt.Errorf("restore recovery requires exclusive access: %w", err)
		}
	}
	if err := backup.RecoverRestore(cfg); err != nil {
		return err
	}
	if !exclusive {
		return unix.Flock(int(lock.Fd()), unix.LOCK_SH)
	}
	return nil
}

func lockDataDir(dir string, exclusive bool) (*os.File, error) {
	return lockFile(filepath.Join(dir, ".mytracker.lock"), exclusive)
}

func lockExistingFile(path string) (*os.File, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func lockFile(path string, exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	mode := unix.LOCK_SH | unix.LOCK_NB
	if exclusive {
		mode = unix.LOCK_EX | unix.LOCK_NB
	}
	if err := unix.Flock(int(f.Fd()), mode); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func warnListenAddr(cfg *config.Config) {
	addr := cfg.ListenAddr
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		warnPublicListener(cfg, addr)
	}
	if strings.HasPrefix(addr, ":") {
		warnPublicListener(cfg, addr)
	}
	if cfg.PublicURL == "" && (host == "" || host == "0.0.0.0" || host == "::") {
		slog.Warn("public_url is not set; generated links will depend on the request Host")
	}
}

func warnPublicListener(cfg *config.Config, addr string) {
	if len(cfg.AutoHTTPSDomains) > 0 {
		return
	}
	slog.Warn("listening on all interfaces without TLS", "addr", addr)
}
