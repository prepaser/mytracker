package config_test

import (
	"testing"

	"github.com/prepaser/mytracker/internal/config"
)

func TestMQTTFingerprintSeparatesFieldBoundaries(t *testing.T) {
	first := config.Default()
	first.MQTTUser = "a|b"
	first.MQTTPass = "c"

	second := config.Default()
	second.MQTTUser = "a"
	second.MQTTPass = "b|c"

	if first.MQTTFingerprint() == second.MQTTFingerprint() {
		t.Fatal("MQTT fingerprint collision")
	}
}
