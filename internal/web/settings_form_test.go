package web

import (
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/password"
)

func TestApplySettingsFormRejectsMalformedNumbers(t *testing.T) {
	req := httptest.NewRequest("POST", "/settings", strings.NewReader(url.Values{"backup_keep": {"bad"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cfg := config.Default()
	if _, err := applySettingsForm(req, &cfg.Settings); err == nil || !strings.Contains(err.Error(), "backup_keep") {
		t.Fatalf("err=%v", err)
	}
}

func TestApplySettingsFormPreservesPasswordWhitespace(t *testing.T) {
	pass := "  correct horse battery staple  "
	req := httptest.NewRequest("POST", "/settings", strings.NewReader(url.Values{"new_password": {pass}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cfg := config.Default()
	effects, err := applySettingsForm(req, &cfg.Settings)
	if err != nil || !effects.passwordChanged {
		t.Fatalf("effects=%+v err=%v", effects, err)
	}
	if ok, _ := password.Verify(cfg.AdminHash, []byte(pass)); !ok {
		t.Fatal("saved password differs from input")
	}
	if ok, _ := password.Verify(cfg.AdminHash, []byte(strings.TrimSpace(pass))); ok {
		t.Fatal("whitespace was silently removed")
	}
}

func TestApplySettingsFormPreservesCredentialWhitespace(t *testing.T) {
	cfg := config.Default()
	form := url.Values{
		"pub_pass":       {"  pub-secret  "},
		"mqtt_pass":      {"\tmqtt-secret\t"},
		"webhook_secret": {" webhook-secret "},
	}
	req := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := applySettingsForm(req, &cfg.Settings); err != nil {
		t.Fatal(err)
	}
	if cfg.PubPass != "  pub-secret  " || cfg.MQTTPass != "\tmqtt-secret\t" || cfg.WebhookSecret != " webhook-secret " {
		t.Fatalf("credentials were normalized: pub=%q mqtt=%q webhook=%q", cfg.PubPass, cfg.MQTTPass, cfg.WebhookSecret)
	}
}

func TestSettingsDraftRetainsRawValuesWithoutSecrets(t *testing.T) {
	cfg := config.Default()
	original := cfg.Clone()
	form := url.Values{
		"csrf": {"csrf"}, "active_tab": {"operations"}, "pub_user": {"draft-user"},
		"retention_days": {"not-a-number"}, "backup_keep": {"also-invalid"},
		"new_password": {"never-render"}, "regen_pub_pass": {"1"},
	}
	req := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	data := map[string]any{"PubUser": cfg.PubUser, "RetentionDays": cfg.RetentionDays, "BackupKeep": cfg.BackupKeep, "RegenPubPass": false}
	if err := req.ParseForm(); err != nil {
		t.Fatal(err)
	}
	settingsDraft(data, req)
	if data["PubUser"] != "draft-user" || data["RetentionDays"] != "not-a-number" || data["BackupKeep"] != "also-invalid" || data["RegenPubPass"] != true {
		t.Fatalf("draft=%v", data)
	}
	_, passwordShown := data["new_password"]
	_, pubPasswordShown := data["pub_pass"]
	if passwordShown || pubPasswordShown {
		t.Fatalf("secret entered draft: %v", data)
	}
	if cfg.PubUser != original.PubUser || cfg.AdminHash != original.AdminHash {
		t.Fatal("settings changed while rendering a failed draft")
	}
}

func TestApplySettingsFormClearsOptionalValues(t *testing.T) {
	cfg := config.Default()
	cfg.TileURL, cfg.TileAttribution, cfg.MQTTClientID = "https://tiles.example/{z}", "tiles", "client"
	form := url.Values{"tile_url": {""}, "tile_attribution": {""}, "mqtt_client_id": {""}}
	req := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := applySettingsForm(req, &cfg.Settings); err != nil {
		t.Fatal(err)
	}
	if cfg.TileURL != "" || cfg.TileAttribution != "" || cfg.MQTTClientID != "" {
		t.Fatalf("optional values not cleared: %+v", cfg)
	}
}

func TestApplySettingsFormUpdatesProviderLists(t *testing.T) {
	cfg := config.Default()
	form := url.Values{
		"ots_calendars": {" \n\t"},
		"esplora_urls":  {"https://one.example/api"},
	}
	req := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := applySettingsForm(req, &cfg.Settings); err != nil {
		t.Fatal(err)
	}
	if len(cfg.OTSCalendars) != 0 {
		t.Fatalf("ots_calendars=%v", cfg.OTSCalendars)
	}
	if len(cfg.EsploraURLs) != 1 || cfg.EsploraURLs[0] != "https://one.example/api" {
		t.Fatalf("esplora_urls=%v", cfg.EsploraURLs)
	}
}

func TestApplySettingsFormRejectsEmptyEsploraURLs(t *testing.T) {
	cfg := config.Default()
	original := append([]string(nil), cfg.EsploraURLs...)
	form := url.Values{"esplora_urls": {" \n\t"}}
	req := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := applySettingsForm(req, &cfg.Settings); err == nil || !strings.Contains(err.Error(), "esplora_urls") {
		t.Fatalf("err=%v", err)
	}
	if !slices.Equal(cfg.EsploraURLs, original) {
		t.Fatalf("esplora_urls changed on invalid form: %v", cfg.EsploraURLs)
	}
}

func TestApplySettingsFormPreservesOmittedProviderLists(t *testing.T) {
	cfg := config.Default()
	ots := append([]string(nil), cfg.OTSCalendars...)
	esplora := append([]string(nil), cfg.EsploraURLs...)
	form := url.Values{"tsa_url": {"https://tsa.example/tsr"}}
	req := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := applySettingsForm(req, &cfg.Settings); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.OTSCalendars, ots) || !slices.Equal(cfg.EsploraURLs, esplora) {
		t.Fatalf("provider lists changed: ots=%v esplora=%v", cfg.OTSCalendars, cfg.EsploraURLs)
	}
}

func TestApplySettingsFormChangesPassword(t *testing.T) {
	cfg := config.Default()
	settings := &cfg.Settings
	newPassword := "a sufficiently long password"
	req := httptest.NewRequest("POST", "/settings", strings.NewReader(url.Values{
		"new_password": {newPassword},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	effects, err := applySettingsForm(req, settings)
	if err != nil || !effects.passwordChanged {
		t.Fatalf("effects=%+v err=%v", effects, err)
	}
	if ok, _ := password.Verify(settings.AdminHash, []byte(newPassword)); !ok {
		t.Fatal("new password hash does not verify")
	}
}

func TestTrackingSettingsChanged(t *testing.T) {
	before := config.Default().Settings
	if trackingSettingsChanged(before, before) {
		t.Fatal("unchanged tracking settings reported as changed")
	}
	for _, change := range []func(*config.Settings){
		func(s *config.Settings) { s.StayRadiusM++ },
		func(s *config.Settings) { s.StayMinDuration = "11m" },
		func(s *config.Settings) { s.TripGap = "31m" },
	} {
		after := before
		change(&after)
		if !trackingSettingsChanged(before, after) {
			t.Fatalf("tracking change was not detected: %+v", after)
		}
	}
}
