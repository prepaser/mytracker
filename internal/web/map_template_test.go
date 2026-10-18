package web

import (
	"bytes"
	"strings"
	"testing"
)

func TestMapTemplatePreservesZeroCoordinates(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string]any
		want string
	}{
		{"zero", map[string]any{"LastLat": 0.0, "LastLon": 0.0}, `"lastLat":0,"lastLon":0`},
		{"missing", map[string]any{}, `"lastLat":null,"lastLon":null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := tpl.ExecuteTemplate(&out, "map.html", tc.data); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("missing coordinate config %s", tc.want)
			}
		})
	}
}
