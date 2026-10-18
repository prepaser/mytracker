package canonical_test

import (
	"encoding/hex"
	"math"
	"testing"

	"github.com/prepaser/mytracker/internal/canonical"
)

func TestFromLocation(t *testing.T) {
	raw := map[string]any{
		"tst":  float64(1751800000),
		"lat":  37.5665,
		"lon":  126.978,
		"acc":  float64(10),
		"batt": float64(85),
	}
	rec, err := canonical.FromLocation("alice", "phone", raw)
	if err != nil {
		t.Fatal(err)
	}
	if rec.User != "alice" || rec.Device != "phone" || rec.Tst != 1751800000 {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if rec.Batt != 85 {
		t.Fatalf("batt=%d", rec.Batt)
	}
}

func TestFromLocationMissingFields(t *testing.T) {
	_, err := canonical.FromLocation("u", "d", map[string]any{"lat": 1.0, "lon": 2.0})
	if err == nil {
		t.Fatal("expected error for missing tst")
	}
}

func TestFromLocationRejectsInvalidValues(t *testing.T) {
	cases := []map[string]any{
		{"tst": 0.0, "lat": 1.0, "lon": 2.0},
		{"tst": 1.5, "lat": 1.0, "lon": 2.0},
		{"tst": math.Inf(1), "lat": 1.0, "lon": 2.0},
		{"tst": 1.0, "lat": 91.0, "lon": 2.0},
		{"tst": 1.0, "lat": 1.0, "lon": -181.0},
		{"tst": 1.0, "lat": math.NaN(), "lon": 2.0},
	}
	for _, raw := range cases {
		if _, err := canonical.FromLocation("u", "d", raw); err == nil {
			t.Fatalf("invalid location accepted: %#v", raw)
		}
	}
}

func TestNumericConversionsRejectLossyValues(t *testing.T) {
	for _, value := range []any{1.5, math.Inf(1), math.NaN(), float64(1 << 63)} {
		if _, ok := canonical.AsInt64(value); ok {
			t.Fatalf("invalid integer accepted: %v", value)
		}
	}
	if got, ok := canonical.AsInt64(float64(42)); !ok || got != 42 {
		t.Fatalf("integer=%d ok=%t", got, ok)
	}
}

func TestDigestDeterministic(t *testing.T) {
	rec := canonical.Record{
		User: "alice", Device: "phone", Tst: 1751800000,
		Lat: 37.5665, Lon: 126.978, Acc: 10,
	}
	d1, err := rec.Digest()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := rec.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatal("digest not deterministic")
	}
}

func TestDigestRounding(t *testing.T) {
	a := canonical.Record{User: "u", Device: "d", Tst: 1, Lat: 37.5665001, Lon: 126.9780001}
	b := canonical.Record{User: "u", Device: "d", Tst: 1, Lat: 37.5665004, Lon: 126.9780004}
	da, _ := a.Digest()
	db, _ := b.Digest()
	if da != db {
		t.Fatal("coordinates within 6dp should hash equal")
	}
}

func TestDigestGolden(t *testing.T) {
	rec := canonical.Record{
		User: "alice", Device: "phone", Tst: 1751800000,
		Lat: 37.5665, Lon: 126.978, Acc: 10,
	}
	d, err := rec.Digest()
	if err != nil {
		t.Fatal(err)
	}
	want := "85e250944cc26689a1677c5546b755f460aee9db2fd46b5bb9cea4e8f05fb006"
	if got := hex.EncodeToString(d[:]); got != want {
		t.Fatalf("golden mismatch\ngot:  %s\nwant: %s", got, want)
	}
}
