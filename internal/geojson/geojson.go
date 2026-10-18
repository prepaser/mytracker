package geojson

import (
	"math"
	"time"

	"github.com/prepaser/mytracker/internal/store"
)

type FeatureCollection struct {
	Type     string    `json:"type"`
	Features []Feature `json:"features"`
	Metadata any       `json:"metadata,omitempty"`
}

type Feature struct {
	Type       string         `json:"type"`
	Geometry   any            `json:"geometry"`
	Properties map[string]any `json:"properties,omitempty"`
}

type lineString struct {
	Type        string      `json:"type"`
	Coordinates [][]float64 `json:"coordinates"`
}

type multiLineString struct {
	Type        string        `json:"type"`
	Coordinates [][][]float64 `json:"coordinates"`
}

type point struct {
	Type        string    `json:"type"`
	Coordinates []float64 `json:"coordinates"`
}

type pointProps func(loc store.Location, i, last int) map[string]any

func FromLocations(locs []store.Location) FeatureCollection {
	return build(locs, func(loc store.Location, _, _ int) map[string]any {
		return map[string]any{
			"kind":       "point",
			"id":         loc.ID,
			"user":       loc.User,
			"device":     loc.Device,
			"tst":        loc.Tst,
			"time":       formatTime(loc.Tst),
			"tsa_status": loc.TsaStatus,
			"ots_status": loc.OtsStatus,
		}
	})
}

func FromShareLocations(locs []store.Location, precisionM ...int) FeatureCollection {
	precision := 0
	if len(precisionM) > 0 {
		precision = precisionM[0]
	}
	if precision > 0 {
		locs = append([]store.Location(nil), locs...)
		for i := range locs {
			locs[i].Lat, locs[i].Lon = Quantize(locs[i].Lat, locs[i].Lon, precision)
		}
	}
	return build(locs, func(loc store.Location, i, last int) map[string]any {
		return map[string]any{
			"kind":    "point",
			"time":    formatTime(loc.Tst),
			"current": i == last,
		}
	})
}

func Quantize(lat, lon float64, meters int) (float64, float64) {
	if meters <= 0 {
		return lat, lon
	}
	latStep := float64(meters) / 111_320
	lat = quantizeAxis(lat, latStep, 90)
	if math.Abs(lat) == 90 {
		return lat, 0
	}
	// The longitude grid must depend only on the public latitude.
	lonStep := latStep / math.Abs(math.Cos(lat*math.Pi/180))
	return lat, quantizeAxis(lon, lonStep, 180)
}

func quantizeAxis(value, step, bound float64) float64 {
	value = math.Round(value/step) * step
	if value == 0 {
		return 0
	}
	return math.Max(-bound, math.Min(bound, value))
}

func build(locs []store.Location, props pointProps) FeatureCollection {
	fc := FeatureCollection{Type: "FeatureCollection", Features: []Feature{}}
	if len(locs) == 0 {
		return fc
	}
	geometry := trackGeometry(locs)
	fc.Features = append(fc.Features, Feature{
		Type:     "Feature",
		Geometry: geometry,
		Properties: map[string]any{
			"kind":   "track",
			"points": len(locs),
		},
	})
	last := len(locs) - 1
	for i, loc := range locs {
		fc.Features = append(fc.Features, Feature{
			Type: "Feature",
			Geometry: point{
				Type:        "Point",
				Coordinates: []float64{loc.Lon, loc.Lat},
			},
			Properties: props(loc, i, last),
		})
	}
	return fc
}

func trackGeometry(locs []store.Location) any {
	type identity struct{ user, device string }
	groups := make(map[identity]int)
	order := make([]identity, 0)
	for _, loc := range locs {
		key := identity{user: loc.User, device: loc.Device}
		if _, ok := groups[key]; !ok {
			groups[key] = len(order)
			order = append(order, key)
		}
	}
	if len(order) <= 1 {
		coords := make([][]float64, len(locs))
		for i, loc := range locs {
			coords[i] = []float64{loc.Lon, loc.Lat}
		}
		if len(coords) == 1 {
			coords = append(coords, []float64{coords[0][0], coords[0][1]})
		}
		return lineString{Type: "LineString", Coordinates: coords}
	}
	segments := make([][][]float64, len(order))
	for _, loc := range locs {
		key := identity{user: loc.User, device: loc.Device}
		i := groups[key]
		segments[i] = append(segments[i], []float64{loc.Lon, loc.Lat})
	}
	for i := range segments {
		if len(segments[i]) == 1 {
			segments[i] = append(segments[i], segments[i][0])
		}
	}
	return multiLineString{Type: "MultiLineString", Coordinates: segments}
}

func formatTime(tst int64) string {
	return time.Unix(tst, 0).UTC().Format(time.RFC3339)
}
