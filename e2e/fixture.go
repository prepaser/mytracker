//go:build ignore

package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
)

func main() {
	if err := seed(os.Args[1]); err != nil {
		log.Fatal(err)
	}
}

func seed(dir string) error {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.ListenAddr = "127.0.0.1:38991"
	cfg.PublicURL = "http://127.0.0.1:38991"
	cfg.AdminHash = "$2y$04$5n.MIn4gMoXy.6KJb1eYLuBs1rjlVeEpJYEzYLs4jPZ8icVGyARM."
	cfg.PubPass = "test-pub-pass"
	cfg.TSAURL = "http://127.0.0.1:1"
	cfg.OTSCalendars = []string{"http://127.0.0.1:1"}
	if err := cfg.Save(filepath.Join(dir, "config.toml")); err != nil {
		return err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	data, err := json.Marshal(cfg.Settings)
	if err != nil {
		return err
	}
	return st.SaveSettings(data, false)
}
