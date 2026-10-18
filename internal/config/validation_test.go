package config_test

import (
	"testing"

	"github.com/prepaser/mytracker/internal/config"
)

func TestSessionIdleMinimum(t *testing.T) {
	for _, value := range []string{"0s", "500ms", "1s", "1999ms", "2s", "2m"} {
		cfg := config.Default()
		cfg.SessionIdle = value
		valid := value == "2s" || value == "2m"
		if err := cfg.Validate(); (err == nil) != valid {
			t.Fatalf("idle=%s err=%v", value, err)
		}
	}
}
