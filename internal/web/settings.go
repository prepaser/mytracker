package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
)

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.settingsPage(w, r, "")
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, errMsg string) {
	cfg, revision := s.Cfg.SnapshotWithRevision()
	s.settingsPageWithSnapshot(w, r, errMsg, cfg, revision, true)
}

func (s *Server) settingsPageWithSnapshot(w http.ResponseWriter, r *http.Request, errMsg string, cfg *config.Config, revision string, preserveSubmittedRevision bool) {
	ids, _ := s.Store.ListIdentities()
	shares, _ := s.Store.ListShares()
	data := map[string]any{
		"Page":             "settings",
		"ActiveTab":        settingsTab(r),
		"TSAURL":           cfg.TSAURL,
		"OTSCalendars":     strings.Join(cfg.OTSCalendars, "\n"),
		"PubUser":          cfg.PubUser,
		"TileURL":          cfg.TileURL,
		"TileAttr":         cfg.TileAttribution,
		"RetentionDays":    cfg.RetentionDays,
		"RetentionBy":      cfg.RetentionBy,
		"WebhookURL":       cfg.WebhookURL,
		"TSATrustRoots":    cfg.TSATrustRoots,
		"TSAPolicyOIDs":    strings.Join(cfg.TSAPolicyOIDs, "\n"),
		"EsploraURLs":      strings.Join(cfg.EsploraURLs, "\n"),
		"OTSConfirmations": cfg.OTSConfirmations,
		"SessionDays":      cfg.SessionDays,
		"MQTTClientID":     cfg.MQTTClientID,
		"MQTTEnabled":      cfg.MQTTEnabled,
		"MQTTBroker":       cfg.MQTTBroker,
		"MQTTTopic":        cfg.MQTTTopic,
		"MQTTUser":         cfg.MQTTUser,
		"MQTTCAFile":       cfg.MQTTCAFile,
		"BackupInterval":   cfg.BackupInterval,
		"BackupKeep":       cfg.BackupKeep,
		"Saved":            r.URL.Query().Get("saved") == "1",
		"Error":            errMsg,
		"SaveError":        errMsg != "",
		"RegenPubPass":     r.FormValue("regen_pub_pass") == "1",
		"CSRF":             s.csrfToken(r),
		"Identities":       ids,
		"Shares":           shareViews(shares),
		"SessionIdle":      cfg.SessionIdle,
		"StayRadiusM":      cfg.StayRadiusM,
		"StayMinDuration":  cfg.StayMinDuration,
		"TripGap":          cfg.TripGap,
		"SettingsRevision": revision,
	}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			settingsDraft(data, r)
			if preserveSubmittedRevision {
				revision := r.FormValue("settings_revision")
				if revision != "" {
					data["SettingsRevision"] = revision
				}
			} else {
				data["RegenPubPass"] = false
			}
		}
	}
	page(w, "settings.html", data)
}

func settingsDraft(data map[string]any, r *http.Request) {
	for key, field := range map[string]string{
		"pub_user": "PubUser", "tsa_url": "TSAURL", "ots_calendars": "OTSCalendars",
		"tile_url": "TileURL", "tile_attribution": "TileAttr", "retention_by": "RetentionBy",
		"webhook_url": "WebhookURL", "tsa_trust_roots": "TSATrustRoots", "tsa_policy_oids": "TSAPolicyOIDs",
		"esplora_urls": "EsploraURLs", "mqtt_broker": "MQTTBroker", "mqtt_topic": "MQTTTopic",
		"mqtt_client_id": "MQTTClientID", "mqtt_user": "MQTTUser", "mqtt_ca_file": "MQTTCAFile",
		"backup_interval": "BackupInterval", "session_idle": "SessionIdle", "stay_min_duration": "StayMinDuration",
		"trip_gap": "TripGap",
	} {
		if _, ok := r.Form[key]; ok {
			data[field] = r.FormValue(key)
		}
	}
	for key, field := range map[string]string{
		"retention_days": "RetentionDays", "backup_keep": "BackupKeep", "ots_min_confirmations": "OTSConfirmations",
		"session_days": "SessionDays", "stay_radius_m": "StayRadiusM",
	} {
		if _, ok := r.Form[key]; ok {
			data[field] = r.FormValue(key)
		}
	}
	if _, submitted := r.Form["csrf"]; submitted {
		data["MQTTEnabled"] = r.FormValue("mqtt_enabled") == "1"
		data["RegenPubPass"] = r.FormValue("regen_pub_pass") == "1"
	}
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		if r.URL.Path == "/api/settings" {
			jsonErr(w, errors.New("invalid request"), http.StatusBadRequest)
		} else {
			http.Error(w, "invalid request", http.StatusBadRequest)
		}
		return
	}
	var effects settingsEffects
	var before config.Settings
	update := s.Cfg.UpdateIfRevision
	if r.FormValue("new_password") != "" {
		update = s.Cfg.UpdateWithSessionRevocationIfRevision
	}
	updated, err := update(r.FormValue("settings_revision"), func(next *config.Settings) error {
		before = *next
		var err error
		effects, err = applySettingsForm(r, next)
		return err
	})
	if err != nil {
		if errors.Is(err, config.ErrRevisionConflict) {
			s.settingsSaveError(w, r, "Settings changed in another tab. Reload before saving.", http.StatusConflict, nil)
			return
		}
		if r.URL.Path == "/api/settings" && !errors.Is(err, config.ErrInvalidSettings) && !errors.Is(err, errInvalidSettingsForm) {
			slog.Error("settings save failed", "request_id", w.Header().Get("X-Request-ID"), "err", err)
			s.settingsSaveError(w, r, "internal error", http.StatusInternalServerError, nil)
		} else {
			s.settingsSaveError(w, r, err.Error(), http.StatusBadRequest, nil)
		}
		return
	}
	if effects.passwordChanged {
		clearSessionCookie(w, r, s.secureCookie(r))
		slog.Info("security session revoked", "scope", "all_password_change")
	}
	if effects.credentialRotated {
		slog.Info("security credential rotated", "kind", "owntracks")
	}
	s.Stamp.Wake()
	if s.Verification != nil {
		s.Verification.ReloadVerification()
	}
	s.Maintenance.Reload()
	if s.Timeline != nil && (settingsTab(r) == "tracking" || trackingSettingsChanged(before, updated.Settings)) {
		if err := s.Timeline.Reload(); err != nil {
			slog.Error("reload timeline failed")
			s.events.Publish(store.Change{Topic: store.ChangeConfig})
			s.settingsSaveError(w, r, "Settings saved, but timeline reload failed. Retry saving the tracking settings.", http.StatusInternalServerError, updated)
			return
		}
	}
	s.events.Publish(store.Change{Topic: store.ChangeConfig})
	redirect := "/settings?saved=1#" + settingsTab(r)
	if r.URL.Path == "/api/settings" {
		jsonOK(w, map[string]any{"data": map[string]string{"redirect": redirect}})
		return
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (s *Server) settingsSaveError(w http.ResponseWriter, r *http.Request, message string, status int, committed *config.Config) {
	if r.URL.Path == "/api/settings" {
		detail := map[string]any{
			"code": httpErrorCode(status), "message": message,
			"request_id": w.Header().Get("X-Request-ID"), "settings_revision": r.FormValue("settings_revision"),
		}
		if committed != nil {
			detail["settings_revision"] = committed.SettingsRevision()
			detail["committed"] = true
		}
		jsonOKStatus(w, map[string]any{"error": detail}, status)
		return
	}
	if status != http.StatusBadRequest {
		w.WriteHeader(status)
	}
	if committed != nil {
		s.settingsPageWithSnapshot(w, r, message, committed, committed.SettingsRevision(), false)
	} else {
		s.settingsPage(w, r, message)
	}
}

func trackingSettingsChanged(before, after config.Settings) bool {
	return before.StayRadiusM != after.StayRadiusM ||
		before.StayMinDuration != after.StayMinDuration ||
		before.TripGap != after.TripGap
}

func settingsTab(r *http.Request) string {
	tab := r.FormValue("active_tab")
	if tab == "" {
		tab = r.URL.Query().Get("tab")
	}
	switch tab {
	case "tracking", "proofs", "privacy", "operations":
		return tab
	default:
		return "tracking"
	}
}
