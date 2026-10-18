package geojson_test

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/prepaser/mytracker/internal/geojson"
	"github.com/prepaser/mytracker/internal/store"
)

func TestFromLocations(t *testing.T) {
	locs := []store.Location{
		{ID: 1, User: "a", Device: "p", Tst: 100, Lat: 37.0, Lon: 127.0},
		{ID: 2, User: "a", Device: "p", Tst: 200, Lat: 37.1, Lon: 127.1},
	}
	fc := geojson.FromLocations(locs)
	if fc.Type != "FeatureCollection" || len(fc.Features) != 3 {
		t.Fatalf("features=%d", len(fc.Features))
	}
	b, err := json.Marshal(fc)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b) {
		t.Fatal("invalid json")
	}
}

func TestFromLocationsSinglePointUsesValidLineString(t *testing.T) {
	loc := store.Location{ID: 1, User: "a", Device: "p", Tst: 100, Lat: 37, Lon: 127}
	fc := geojson.FromLocations([]store.Location{loc})
	raw, err := json.Marshal(fc.Features[0].Geometry)
	if err != nil {
		t.Fatal(err)
	}
	var geometry struct {
		Type        string      `json:"type"`
		Coordinates [][]float64 `json:"coordinates"`
	}
	if err := json.Unmarshal(raw, &geometry); err != nil {
		t.Fatal(err)
	}
	if geometry.Type != "LineString" || len(geometry.Coordinates) != 2 ||
		!reflect.DeepEqual(geometry.Coordinates[0], geometry.Coordinates[1]) {
		t.Fatalf("geometry=%s", raw)
	}
}

func TestFromLocationsSeparatesIdentities(t *testing.T) {
	locs := []store.Location{
		{User: "a", Device: "phone", Tst: 100, Lat: 37, Lon: 127},
		{User: "b", Device: "watch", Tst: 200, Lat: 47, Lon: -122},
		{User: "a", Device: "phone", Tst: 300, Lat: 37.1, Lon: 127.1},
	}
	fc := geojson.FromLocations(locs)
	raw, err := json.Marshal(fc.Features[0].Geometry)
	if err != nil {
		t.Fatal(err)
	}
	var geometry struct {
		Type        string        `json:"type"`
		Coordinates [][][]float64 `json:"coordinates"`
	}
	if err := json.Unmarshal(raw, &geometry); err != nil {
		t.Fatal(err)
	}
	if geometry.Type != "MultiLineString" || len(geometry.Coordinates) != 2 ||
		len(geometry.Coordinates[0]) != 2 || len(geometry.Coordinates[1]) != 2 ||
		geometry.Coordinates[1][0][0] != geometry.Coordinates[1][1][0] ||
		geometry.Coordinates[1][0][1] != geometry.Coordinates[1][1][1] {
		t.Fatalf("geometry=%s", raw)
	}
}

func TestFromShareLocations(t *testing.T) {
	locs := []store.Location{
		{Tst: 100, Lat: 37.0, Lon: 127.0},
		{Tst: 200, Lat: 37.1, Lon: 127.1},
	}
	fc := geojson.FromShareLocations(locs)
	if len(fc.Features) != 3 {
		t.Fatalf("features=%d", len(fc.Features))
	}
	if fc.Features[2].Properties["current"] != true {
		t.Fatal("last point should be current")
	}
	if _, ok := fc.Features[2].Properties["id"]; ok {
		t.Fatal("share geojson must not expose id")
	}
}

func TestShareLocationsReducePrecision(t *testing.T) {
	locs := []store.Location{{ID: 1, Lat: 37.5665123, Lon: 126.9780123, Tst: 1}}
	exact := geojson.FromShareLocations(locs)
	coarse := geojson.FromShareLocations(locs, 1000)
	if reflect.DeepEqual(exact.Features[1].Geometry, coarse.Features[1].Geometry) {
		t.Fatal("coordinates were not reduced")
	}
	if locs[0].Lat != 37.5665123 || locs[0].Lon != 126.9780123 {
		t.Fatal("share quantization changed the original location")
	}
}

func TestQuantizeUsesPublicLatitude(t *testing.T) {
	for _, meters := range []int{100, 500, 1000} {
		step := float64(meters) / 111320
		for _, seed := range []float64{37.123456, -37.123456, 0, 89.9, -89.9} {
			center := math.Round(seed/step) * step
			lat1, lon1 := geojson.Quantize(center-step/4, 127.123456, meters)
			lat2, lon2 := geojson.Quantize(center+step/4, 127.123456, meters)
			if lat1 != lat2 || lon1 != lon2 || math.Signbit(lat1) != math.Signbit(lat2) {
				t.Fatalf("precision=%d same grid row exposes latitude: (%g,%g) != (%g,%g)", meters, lat1, lon1, lat2, lon2)
			}
		}
	}
}

func TestQuantizeBoundsAndExactCoordinates(t *testing.T) {
	for _, point := range [][2]float64{{90, 180}, {-90, -180}, {89.99999, 179.99999}, {-89.99999, -179.99999}, {-0.0001, -0.0001}} {
		lat, lon := geojson.Quantize(point[0], point[1], 1000)
		if math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
			t.Fatalf("invalid public coordinate (%g,%g)", lat, lon)
		}
		if (lat == 0 && math.Signbit(lat)) || (lon == 0 && math.Signbit(lon)) {
			t.Fatal("signed zero exposes position within the grid cell")
		}
		exactLat, exactLon := geojson.Quantize(point[0], point[1], 0)
		if exactLat != point[0] || exactLon != point[1] {
			t.Fatal("zero precision changed exact coordinates")
		}
	}
}
