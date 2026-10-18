package canonical

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
)

type Record struct {
	User   string  `json:"user"`
	Device string  `json:"device"`
	Tst    int64   `json:"tst"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Acc    float64 `json:"acc,omitempty"`
	Alt    float64 `json:"alt,omitempty"`
	Vel    float64 `json:"vel,omitempty"`
	Cog    float64 `json:"cog,omitempty"`
	Batt   int     `json:"batt,omitempty"`
}

func FromLocation(user, device string, raw map[string]any) (Record, error) {
	r := Record{User: user, Device: device}
	var ok bool
	if r.Tst, ok = AsInt64(raw["tst"]); !ok {
		if r.Tst, ok = AsInt64(raw["t"]); !ok {
			return r, fmt.Errorf("missing tst")
		}
	}
	if r.Tst <= 0 {
		return r, fmt.Errorf("invalid tst")
	}
	if r.Lat, ok = AsFloat(raw["lat"]); !ok {
		return r, fmt.Errorf("missing lat")
	}
	if r.Lon, ok = AsFloat(raw["lon"]); !ok {
		return r, fmt.Errorf("missing lon")
	}
	if r.Lat < -90 || r.Lat > 90 || r.Lon < -180 || r.Lon > 180 {
		return r, fmt.Errorf("invalid coordinates")
	}
	r.Acc, _ = AsFloat(raw["acc"])
	r.Alt, _ = AsFloat(raw["alt"])
	r.Vel, _ = AsFloat(raw["vel"])
	r.Cog, _ = AsFloat(raw["cog"])
	if v, ok := AsInt64(raw["batt"]); ok {
		r.Batt = int(v)
	}
	return r, nil
}

func (r Record) Digest() ([32]byte, error) {
	r.Lat = round(r.Lat, 6)
	r.Lon = round(r.Lon, 6)
	r.Acc = round(r.Acc, 1)
	r.Alt = round(r.Alt, 1)
	r.Vel = round(r.Vel, 1)
	r.Cog = round(r.Cog, 1)
	b, err := json.Marshal(r)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

func round(v float64, p int) float64 {
	pow := math.Pow(10, float64(p))
	return math.Round(v*pow) / pow
}

func AsFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	default:
		return 0, false
	}
}

func AsInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) || math.Trunc(x) != x || x < -1<<63 || x >= 1<<63 {
			return 0, false
		}
		return int64(x), true
	case int:
		return int64(x), true
	case int64:
		return x, true
	default:
		return 0, false
	}
}
