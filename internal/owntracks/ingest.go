package owntracks

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/store"
)

var ErrInvalidPayload = errors.New("invalid OwnTracks payload")

const MaxPayloadBytes = 1 << 20
const MaxIdentityBytes = 128

func normalizeIdentity(user, device string) (string, string) {
	return strings.TrimSpace(user), strings.TrimSpace(device)
}

func Ingest(st *store.Store, stamp Stamper, user, device string, body []byte, maxAttempts ...int) (int64, error) {
	if len(body) > MaxPayloadBytes {
		return 0, fmt.Errorf("%w: payload too large", ErrInvalidPayload)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, fmt.Errorf("%w: malformed JSON", ErrInvalidPayload)
	}
	return ingestDecoded(st, stamp, user, device, raw, maxAttempts...)
}

func ingestDecoded(st *store.Store, stamp Stamper, user, device string, raw map[string]any, maxAttempts ...int) (int64, error) {
	if !ingestType(raw) {
		return 0, nil
	}
	if user == "" || device == "" {
		user, device = identFromTopic(raw, user, device)
	}
	user, device = normalizeIdentity(user, device)
	if !validIdentity(user) || !validIdentity(device) {
		return 0, fmt.Errorf("%w: invalid user or device", ErrInvalidPayload)
	}
	rec, err := canonical.FromLocation(user, device, raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidPayload, err)
	}
	if rec.Tst > time.Now().Add(24*time.Hour).Unix() {
		return 0, fmt.Errorf("%w: tst is too far in the future", ErrInvalidPayload)
	}
	digest, err := rec.Digest()
	if err != nil {
		return 0, fmt.Errorf("%w: invalid numeric field: %v", ErrInvalidPayload, err)
	}
	minimal := minimizePayload(raw, rec)
	check, err := canonical.FromLocation(user, device, minimal)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidPayload, err)
	}
	checkDigest, err := check.Digest()
	if err != nil {
		return 0, fmt.Errorf("%w: invalid numeric field: %v", ErrInvalidPayload, err)
	}
	if checkDigest != digest {
		return 0, fmt.Errorf("minimal payload changes canonical digest")
	}
	payload, err := json.Marshal(minimal)
	if err != nil {
		return 0, err
	}
	loc := &store.Location{
		User: user, Device: device, Tst: rec.Tst, Lat: rec.Lat, Lon: rec.Lon,
		Payload: json.RawMessage(payload), Digest: digest[:],
	}
	id, err := st.Insert(loc, maxAttempts...)
	if errors.Is(err, store.ErrDuplicate) {
		slog.Debug("duplicate location")
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	stamp.Process(id, digest)
	return id, nil
}

func minimizePayload(raw map[string]any, rec canonical.Record) map[string]any {
	out := make(map[string]any, 9)
	if typ, ok := raw["_type"].(string); ok {
		out["_type"] = typ
	}
	out["tst"], out["lat"], out["lon"] = rec.Tst, rec.Lat, rec.Lon
	for _, key := range []string{"acc", "alt", "vel", "cog"} {
		if value, ok := canonical.AsFloat(raw[key]); ok {
			out[key] = value
		}
	}
	if value, ok := canonical.AsInt64(raw["batt"]); ok {
		out["batt"] = value
	}
	return out
}

func validIdentity(value string) bool {
	if value == "" || len(value) > MaxIdentityBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func ingestType(raw map[string]any) bool {
	typ, _ := raw["_type"].(string)
	switch typ {
	case "location", "waypoint":
		return true
	case "transition":
		if _, ok := raw["tst"]; !ok {
			if t, ok := canonical.AsInt64(raw["t"]); ok {
				raw["tst"] = float64(t)
			}
		}
		_, lat := canonical.AsFloat(raw["lat"])
		_, lon := canonical.AsFloat(raw["lon"])
		return lat && lon
	default:
		return false
	}
}

func identFromTopic(raw map[string]any, user, device string) (string, string) {
	if topic, ok := raw["topic"].(string); ok {
		parts := strings.Split(topic, "/")
		if user == "" && len(parts) >= 3 {
			user = parts[1]
		}
		if device == "" && len(parts) >= 3 {
			device = parts[2]
		}
	}
	if user == "" {
		user = "owntracks"
	}
	if device == "" {
		device = "phone"
	}
	return user, device
}
