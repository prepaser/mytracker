package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/password"
)

var errInvalidSettingsForm = errors.New("invalid settings form")

type settingsEffects struct {
	passwordChanged   bool
	credentialRotated bool
}

func applySettingsForm(r *http.Request, next *config.Settings) (settingsEffects, error) {
	var effects settingsEffects
	if err := r.ParseForm(); err != nil {
		return effects, err
	}
	if user := strings.TrimSpace(r.FormValue("pub_user")); user != "" {
		next.PubUser = user
	}
	if r.FormValue("regen_pub_pass") == "1" {
		pass, err := config.RandToken(16)
		if err != nil {
			return effects, err
		}
		next.PubPass = pass
		effects.credentialRotated = true
	} else if pass := r.FormValue("pub_pass"); pass != "" {
		next.PubPass = pass
		effects.credentialRotated = true
	}

	next.TSAURL = strings.TrimSpace(r.FormValue("tsa_url"))
	if _, ok := r.Form["ots_calendars"]; ok {
		next.OTSCalendars = formLines(r.FormValue("ots_calendars"))
	}
	setOptional(r, "tile_url", &next.TileURL)
	if err := formInt(r, "retention_days", &next.RetentionDays); err != nil {
		return effects, err
	}
	if value := strings.TrimSpace(r.FormValue("retention_by")); value != "" {
		next.RetentionBy = value
	}
	next.WebhookURL = strings.TrimSpace(r.FormValue("webhook_url"))
	next.TSATrustRoots = strings.TrimSpace(r.FormValue("tsa_trust_roots"))
	next.TSAPolicyOIDs = formLines(r.FormValue("tsa_policy_oids"))
	setOptional(r, "tile_attribution", &next.TileAttribution)
	next.MQTTEnabled = r.FormValue("mqtt_enabled") == "1"
	next.MQTTBroker = strings.TrimSpace(r.FormValue("mqtt_broker"))
	next.MQTTTopic = strings.TrimSpace(r.FormValue("mqtt_topic"))
	next.MQTTUser = strings.TrimSpace(r.FormValue("mqtt_user"))
	if pass := r.FormValue("mqtt_pass"); pass != "" {
		next.MQTTPass = pass
	}
	next.MQTTCAFile = strings.TrimSpace(r.FormValue("mqtt_ca_file"))
	setNonempty(r, "backup_interval", &next.BackupInterval)
	if err := formInt(r, "backup_keep", &next.BackupKeep); err != nil {
		return effects, err
	}
	if secret := r.FormValue("webhook_secret"); secret != "" {
		next.WebhookSecret = secret
	}
	if _, ok := r.Form["esplora_urls"]; ok {
		values := formLines(r.FormValue("esplora_urls"))
		if len(values) < 1 || len(values) > 5 {
			return effects, fmt.Errorf("%w: esplora_urls must contain between 1 and 5 URLs", errInvalidSettingsForm)
		}
		next.EsploraURLs = values
	}
	if err := formInt(r, "ots_min_confirmations", &next.OTSConfirmations); err != nil {
		return effects, err
	}
	if err := formInt(r, "session_days", &next.SessionDays); err != nil {
		return effects, err
	}
	setNonempty(r, "session_idle", &next.SessionIdle)
	setOptional(r, "mqtt_client_id", &next.MQTTClientID)
	if err := formInt(r, "stay_radius_m", &next.StayRadiusM); err != nil {
		return effects, err
	}
	setNonempty(r, "stay_min_duration", &next.StayMinDuration)
	setNonempty(r, "trip_gap", &next.TripGap)

	newPass := r.FormValue("new_password")
	if newPass != "" {
		if len(newPass) < passwordMin || len(newPass) > passwordMax {
			return effects, fmt.Errorf("%w: password must be 14 to 256 bytes", errInvalidSettingsForm)
		}
		hash, err := password.Hash([]byte(newPass))
		if err != nil {
			return effects, err
		}
		next.AdminHash = hash
		effects.passwordChanged = true
	}
	return effects, nil
}

func formInt(r *http.Request, key string, dst *int) error {
	raw := strings.TrimSpace(r.FormValue(key))
	if raw == "" {
		return nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("%w: %s is invalid", errInvalidSettingsForm, key)
	}
	*dst = value
	return nil
}

func formLines(raw string) []string {
	values := make([]string, 0)
	for value := range strings.SplitSeq(raw, "\n") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func setNonempty(r *http.Request, key string, dst *string) {
	if value := strings.TrimSpace(r.FormValue(key)); value != "" {
		*dst = value
	}
}

func setOptional(r *http.Request, key string, dst *string) {
	value := strings.TrimSpace(r.FormValue(key))
	if _, ok := r.Form[key]; ok {
		*dst = value
	}
}
